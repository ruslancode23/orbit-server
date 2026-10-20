// Package gateway serves the WebSocket connection a client keeps open to see
// what happens live: new messages, edits, reactions, membership changes.
// Reading history is still the HTTP API; this is only the live stream.
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

const (
	// writeTimeout caps one frame: a reader that stops reading must not pin a
	// goroutine forever.
	writeTimeout = 10 * time.Second
	// maxFrameBytes caps what a client may send; the frames here are tiny.
	maxFrameBytes = 16 * 1024
	// catchUpLimit caps the replay per channel after a reconnect. A client that
	// missed more asks the HTTP history for the rest.
	catchUpLimit = 100
	// maxCatchUpChannels caps the cursors one subscribe may carry.
	maxCatchUpChannels = 100
)

// Membership answers whether an account may watch a server.
type Membership interface {
	Authorize(ctx context.Context, workspaceID, userID uuid.UUID) (servers.Role, error)
}

// History replays what a reconnecting client missed in one channel.
type History interface {
	SinceEvents(ctx context.Context, channelID, callerID uuid.UUID, afterSeq int64, limit int) ([]events.Event, error)
}

// Hub is the part of events.Hub the gateway uses. A connection holds two
// subscriptions: one for the server it is watching, one for the account itself,
// because conversations and secret envelopes belong to no server.
type Hub interface {
	Subscribe(userID, workspaceID uuid.UUID) *events.Subscription
	SubscribeUser(userID uuid.UUID) *events.Subscription
}

type Handler struct {
	hub     Hub
	members Membership
	history History
	log     *slog.Logger
	// origins are the sites allowed to open a connection; same list as CORS.
	origins []string
}

func NewHandler(hub Hub, members Membership, history History, origins []string, log *slog.Logger) *Handler {
	return &Handler{hub: hub, members: members, history: history, origins: origins, log: log}
}

// clientFrame is what the client sends.
type clientFrame struct {
	Type        string           `json:"type"`
	WorkspaceID uuid.UUID        `json:"workspaceId,omitempty"`
	Cursors     map[string]int64 `json:"cursors,omitempty"`
	// Conversations carries the per-conversation cursors of the personal
	// stream: a dialogue is a channel, so its history is replayed the same way.
	Conversations map[string]int64 `json:"conversations,omitempty"`
}

// serverFrame is what the client receives.
type serverFrame struct {
	Type        string     `json:"type"`
	WorkspaceID *uuid.UUID `json:"workspaceId,omitempty"`
	ChannelID   *uuid.UUID `json:"channelId,omitempty"`
	Seq         int64      `json:"seq,omitempty"`
	Payload     any        `json:"payload,omitempty"`
	Code        string     `json:"code,omitempty"`
	Message     string     `json:"message,omitempty"`
}

// ServeHTTP upgrades the connection. The session comes from the cookie, which
// the browser sends on an upgrade — a header it cannot set.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFrom(r.Context())
	if !ok {
		// The route is guarded by auth.RequireSession; this is belt and braces.
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.origins})
	if err != nil {
		h.log.InfoContext(r.Context(), "websocket upgrade failed", slog.Any("error", err))
		return
	}
	conn.SetReadLimit(maxFrameBytes)
	defer func() { _ = conn.CloseNow() }()

	h.serve(r.Context(), conn, session.User.ID)
}

// serve reads client frames until the connection ends. One connection watches
// one server at a time: switching servers replaces the subscription. The
// account stream is opened at once and lives as long as the connection, so a
// dialogue arrives whether or not a server is being watched.
func (h *Handler) serve(ctx context.Context, conn *websocket.Conn, userID uuid.UUID) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// writes serialises outgoing frames: the reader loop and both event pumps
	// write, and a WebSocket connection takes one writer at a time.
	writes := make(chan serverFrame, 32)
	go h.pumpWrites(ctx, conn, writes)

	// One skip table for both streams: a message replayed after a reconnect must
	// not also arrive live, whichever pump sees it first.
	tracker := newReplayTracker()
	personal := h.hub.SubscribeUser(userID)
	defer personal.Close()
	go h.pumpPersonalEvents(ctx, personal, tracker, writes)

	var current *events.Subscription
	defer func() {
		if current != nil {
			current.Close()
		}
	}()

	for {
		var frame clientFrame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return // the client went away, or sent something unreadable
		}

		switch frame.Type {
		case "subscribe":
			if current != nil {
				current.Close()
				current = nil
			}
			sub, err := h.subscribe(ctx, userID, frame, tracker, writes)
			if err != nil {
				send(ctx, writes, errorFrame(err))
				continue
			}
			current = sub

		case "resume":
			// The account stream has no server to subscribe to: the client names
			// the conversations it already has and gets what it missed.
			h.resume(ctx, userID, frame.Conversations, tracker, writes)
			send(ctx, writes, serverFrame{Type: "resumed"})

		case "unsubscribe":
			if current != nil {
				current.Close()
				current = nil
			}
			send(ctx, writes, serverFrame{Type: "unsubscribed"})

		case "ping":
			send(ctx, writes, serverFrame{Type: "pong"})

		default:
			send(ctx, writes, serverFrame{
				Type: "error", Code: "invalid_request", Message: "unknown frame type",
			})
		}
	}
}

// subscribe checks the role, replays what the client missed and starts the
// live stream. The role is read here, so a ban between two subscribes is seen.
func (h *Handler) subscribe(ctx context.Context, userID uuid.UUID, frame clientFrame, tracker *replayTracker, writes chan<- serverFrame) (*events.Subscription, error) {
	if frame.WorkspaceID == uuid.Nil {
		return nil, errInvalid
	}
	if _, err := h.members.Authorize(ctx, frame.WorkspaceID, userID); err != nil {
		if errors.Is(err, servers.ErrNotMember) {
			return nil, errNotFound
		}
		return nil, err
	}
	if len(frame.Cursors) > maxCatchUpChannels {
		return nil, errInvalid
	}

	// Subscribe before replaying: an event that arrives during the catch-up
	// waits in the queue instead of being lost between the two.
	sub := h.hub.Subscribe(userID, frame.WorkspaceID)

	workspaceID := frame.WorkspaceID
	send(ctx, writes, serverFrame{Type: "subscribed", WorkspaceID: &workspaceID})

	h.replay(ctx, userID, frame.Cursors, tracker, writes)
	go h.pumpEvents(ctx, sub, tracker, writes)
	return sub, nil
}

// resume replays what the client missed in the conversations it names. It does
// not start a server subscription: a dialogue is readable from any page.
func (h *Handler) resume(ctx context.Context, userID uuid.UUID, cursors map[string]int64, tracker *replayTracker, writes chan<- serverFrame) {
	h.replay(ctx, userID, cursors, tracker, writes)
}

// replay sends what the client missed in each channel it named, oldest first,
// and records the highest seq it sent per channel so the live streams can skip
// an event the replay already delivered.
func (h *Handler) replay(ctx context.Context, userID uuid.UUID, cursors map[string]int64, tracker *replayTracker, writes chan<- serverFrame) {
	for rawID, afterSeq := range cursors {
		channelID, err := uuid.Parse(rawID)
		if err != nil {
			continue
		}
		missed, err := h.history.SinceEvents(ctx, channelID, userID, afterSeq, catchUpLimit)
		if err != nil {
			// A channel the client may no longer read: nothing to replay.
			continue
		}
		for _, missedEvent := range missed {
			send(ctx, writes, serverFrame{
				Type:      string(missedEvent.Kind),
				ChannelID: &channelID,
				Seq:       missedEvent.Seq,
				Payload:   missedEvent.Payload,
			})
			tracker.mark(channelID, missedEvent.Seq)
		}
	}
}

// pumpEvents forwards live events until the subscription ends.
func (h *Handler) pumpEvents(ctx context.Context, sub *events.Subscription, tracker *replayTracker, writes chan<- serverFrame) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			if sub.Dropped() {
				send(ctx, writes, serverFrame{
					Type: "error", Code: "behind",
					Message: "the connection fell behind; reconnect and catch up by cursor",
				})
			} else {
				// A revoke: the account lost access to this server.
				send(ctx, writes, serverFrame{
					Type: "error", Code: "forbidden", Message: "you are no longer on this server",
				})
			}
			return
		case event := <-sub.Events():
			if tracker.seen(event.ChannelID, event.Seq) {
				continue
			}
			workspaceID, channelID := event.WorkspaceID, event.ChannelID
			frame := serverFrame{
				Type:        string(event.Kind),
				WorkspaceID: &workspaceID,
				Seq:         event.Seq,
				Payload:     event.Payload,
			}
			if channelID != uuid.Nil {
				frame.ChannelID = &channelID
			}
			send(ctx, writes, frame)
		}
	}
}

// pumpPersonalEvents forwards the account stream: new dialogues, messages in
// them, read cursors and secret envelopes. It has no server of its own, so a
// frame carries only the channel the event happened in.
func (h *Handler) pumpPersonalEvents(ctx context.Context, sub *events.Subscription, tracker *replayTracker, writes chan<- serverFrame) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			if sub.Dropped() {
				send(ctx, writes, serverFrame{
					Type: "error", Code: "behind",
					Message: "the connection fell behind; reconnect and catch up by cursor",
				})
			}
			return
		case event := <-sub.Events():
			if tracker.seen(event.ChannelID, event.Seq) {
				continue
			}
			if event.Seq > 0 {
				tracker.mark(event.ChannelID, event.Seq)
			}
			frame := serverFrame{
				Type:    string(event.Kind),
				Seq:     event.Seq,
				Payload: event.Payload,
			}
			// A read cursor belongs to a server even though it travels on the
			// account stream; the client needs the id to refresh its badges.
			if event.WorkspaceID != uuid.Nil {
				workspaceID := event.WorkspaceID
				frame.WorkspaceID = &workspaceID
			}
			if event.ChannelID != uuid.Nil {
				channelID := event.ChannelID
				frame.ChannelID = &channelID
			}
			send(ctx, writes, frame)
		}
	}
}

// replayTracker remembers the highest seq sent on each channel, so a catch-up
// and a live event cannot both deliver the same message.
type replayTracker struct {
	mu      sync.RWMutex
	highest map[uuid.UUID]int64
}

func newReplayTracker() *replayTracker {
	return &replayTracker{highest: map[uuid.UUID]int64{}}
}

func (t *replayTracker) seen(channelID uuid.UUID, seq int64) bool {
	if seq <= 0 {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return seq <= t.highest[channelID]
}

func (t *replayTracker) mark(channelID uuid.UUID, seq int64) {
	if seq <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if seq > t.highest[channelID] {
		t.highest[channelID] = seq
	}
}

// pumpWrites is the only writer on the connection.
func (h *Handler) pumpWrites(ctx context.Context, conn *websocket.Conn, writes <-chan serverFrame) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-writes:
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := wsjson.Write(writeCtx, conn, frame)
			cancel()
			if err != nil {
				_ = conn.Close(websocket.StatusInternalError, "write failed")
				return
			}
		}
	}
}

// send never blocks the caller: a connection whose write queue is full is
// already in trouble, and its subscription will be dropped on the next event.
func send(ctx context.Context, writes chan<- serverFrame, frame serverFrame) {
	select {
	case writes <- frame:
	case <-ctx.Done():
	}
}

var (
	errInvalid  = errors.New("gateway: invalid frame")
	errNotFound = errors.New("gateway: not found")
)

func errorFrame(err error) serverFrame {
	switch {
	case errors.Is(err, errNotFound):
		return serverFrame{Type: "error", Code: "not_found", Message: "not found"}
	case errors.Is(err, errInvalid):
		return serverFrame{Type: "error", Code: "invalid_request", Message: "invalid frame"}
	default:
		return serverFrame{Type: "error", Code: "internal", Message: "internal error"}
	}
}
