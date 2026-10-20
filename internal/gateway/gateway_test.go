package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

type fakeMembership struct {
	roles map[uuid.UUID]map[uuid.UUID]servers.Role
}

func (m *fakeMembership) Authorize(_ context.Context, workspaceID, userID uuid.UUID) (servers.Role, error) {
	role, ok := m.roles[workspaceID][userID]
	if !ok {
		return "", servers.ErrNotMember
	}
	return role, nil
}

// testServer wires a gateway over a real HTTP server, with a session already
// resolved, so tests speak the actual protocol over a real connection.
type testServer struct {
	url       string
	hub       *events.Hub
	members   *fakeMembership
	history   *recordingHistory
	workspace uuid.UUID
	channel   uuid.UUID
	user      uuid.UUID
}

type recordingHistory struct {
	missed map[uuid.UUID][]events.Event
	err    error
	calls  int
}

func (h *recordingHistory) SinceEvents(_ context.Context, channelID, _ uuid.UUID, afterSeq int64, limit int) ([]events.Event, error) {
	h.calls++
	if h.err != nil {
		return nil, h.err
	}
	var out []events.Event
	for _, event := range h.missed[channelID] {
		if event.Seq > afterSeq {
			out = append(out, event)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := &testServer{
		hub:       events.NewHub(),
		workspace: uuid.New(),
		channel:   uuid.New(),
		user:      uuid.New(),
		history:   &recordingHistory{missed: map[uuid.UUID][]events.Event{}},
	}
	ts.members = &fakeMembership{roles: map[uuid.UUID]map[uuid.UUID]servers.Role{
		ts.workspace: {ts.user: servers.RoleMember},
	}}

	handler := NewHandler(ts.hub, ts.members, ts.history, []string{"*"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// The session is what auth.Middleware would have put there.
	withSession := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithSession(r.Context(), auth.Session{
			ID:   uuid.New(),
			User: auth.User{ID: ts.user, Nickname: "Anna"},
		})
		handler.ServeHTTP(w, r.WithContext(ctx))
	})

	srv := httptest.NewServer(withSession)
	t.Cleanup(srv.Close)
	ts.url = "ws" + srv.URL[len("http"):]
	return ts
}

func (ts *testServer) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.Dial(t.Context(), ts.url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// The upgrade response has no body to read, but it still must be closed.
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func write(t *testing.T, conn *websocket.Conn, frame map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, frame); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func read(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var frame map[string]any
	if err := wsjson.Read(ctx, conn, &frame); err != nil {
		t.Fatalf("read: %v", err)
	}
	return frame
}

func TestSubscribeAndReceiveLiveEvents(t *testing.T) {
	ts := newTestServer(t)
	conn := ts.dial(t)

	write(t, conn, map[string]any{"type": "subscribe", "workspaceId": ts.workspace})
	if frame := read(t, conn); frame["type"] != "subscribed" {
		t.Fatalf("frame = %v, want subscribed", frame)
	}

	// Wait until the hub really has the subscription, then publish.
	waitFor(t, func() bool { return ts.hub.Subscribers(ts.workspace) == 1 })
	ts.hub.Publish(events.Event{
		Kind: events.KindMessageCreated, WorkspaceID: ts.workspace,
		ChannelID: ts.channel, Seq: 5, Payload: map[string]any{"content": "привет"},
	})

	frame := read(t, conn)
	if frame["type"] != string(events.KindMessageCreated) {
		t.Errorf("type = %v, want %v", frame["type"], events.KindMessageCreated)
	}
	if frame["seq"] != float64(5) {
		t.Errorf("seq = %v, want 5", frame["seq"])
	}
	if frame["channelId"] != ts.channel.String() {
		t.Errorf("channelId = %v, want %v", frame["channelId"], ts.channel)
	}
}

// Someone who is not on the server gets nothing, and the check happens on the
// subscribe, not only when the connection opened.
func TestSubscribeChecksMembership(t *testing.T) {
	ts := newTestServer(t)
	conn := ts.dial(t)
	stranger := uuid.New()

	write(t, conn, map[string]any{"type": "subscribe", "workspaceId": stranger})

	frame := read(t, conn)
	if frame["type"] != "error" || frame["code"] != "not_found" {
		t.Errorf("frame = %v, want a not_found error", frame)
	}
	if ts.hub.Subscribers(stranger) != 0 {
		t.Error("a subscription was created for a server the caller is not on")
	}
}

// Losing access closes the stream of that account at once, without waiting for
// the client to reconnect (FR-02).
func TestRevokeClosesTheStream(t *testing.T) {
	ts := newTestServer(t)
	conn := ts.dial(t)

	write(t, conn, map[string]any{"type": "subscribe", "workspaceId": ts.workspace})
	read(t, conn) // subscribed
	waitFor(t, func() bool { return ts.hub.Subscribers(ts.workspace) == 1 })

	// The servers module does this when someone is banned or removed.
	ts.hub.Revoke(ts.workspace, ts.user)

	frame := read(t, conn)
	if frame["type"] != "error" || frame["code"] != "forbidden" {
		t.Errorf("frame = %v, want a forbidden error", frame)
	}
	waitFor(t, func() bool { return ts.hub.Subscribers(ts.workspace) == 0 })
}

// A reconnecting client names where it stopped and gets exactly what it missed:
// nothing lost, nothing twice.
func TestCatchUpByCursor(t *testing.T) {
	ts := newTestServer(t)
	ts.history.missed[ts.channel] = []events.Event{
		{Kind: events.KindMessageCreated, ChannelID: ts.channel, Seq: 2, Payload: map[string]any{"seq": 2}},
		{Kind: events.KindMessageCreated, ChannelID: ts.channel, Seq: 3, Payload: map[string]any{"seq": 3}},
	}
	conn := ts.dial(t)

	write(t, conn, map[string]any{
		"type":        "subscribe",
		"workspaceId": ts.workspace,
		"cursors":     map[string]any{ts.channel.String(): 1},
	})
	read(t, conn) // subscribed

	for _, wantSeq := range []float64{2, 3} {
		frame := read(t, conn)
		if frame["seq"] != wantSeq {
			t.Fatalf("seq = %v, want %v", frame["seq"], wantSeq)
		}
	}

	waitFor(t, func() bool { return ts.hub.Subscribers(ts.workspace) == 1 })
	// An event the catch-up already delivered is not sent again...
	ts.hub.Publish(events.Event{
		Kind: events.KindMessageCreated, WorkspaceID: ts.workspace, ChannelID: ts.channel, Seq: 3,
	})
	// ...but the next one is.
	ts.hub.Publish(events.Event{
		Kind: events.KindMessageCreated, WorkspaceID: ts.workspace, ChannelID: ts.channel, Seq: 4,
	})

	frame := read(t, conn)
	if frame["seq"] != float64(4) {
		t.Errorf("seq = %v, want 4: seq 3 came twice", frame["seq"])
	}
}

// A channel the client may no longer read is skipped instead of failing the
// whole subscribe.
func TestCatchUpSkipsUnreadableChannels(t *testing.T) {
	ts := newTestServer(t)
	ts.history.err = errors.New("not found")
	conn := ts.dial(t)

	write(t, conn, map[string]any{
		"type":        "subscribe",
		"workspaceId": ts.workspace,
		"cursors":     map[string]any{ts.channel.String(): 1},
	})
	if frame := read(t, conn); frame["type"] != "subscribed" {
		t.Fatalf("frame = %v, want subscribed", frame)
	}

	waitFor(t, func() bool { return ts.hub.Subscribers(ts.workspace) == 1 })
	ts.hub.Publish(events.Event{
		Kind: events.KindMessageCreated, WorkspaceID: ts.workspace, ChannelID: ts.channel, Seq: 9,
	})
	if frame := read(t, conn); frame["seq"] != float64(9) {
		t.Errorf("seq = %v, want 9", frame["seq"])
	}
}

func TestResubscribeReplacesTheOldSubscription(t *testing.T) {
	ts := newTestServer(t)
	other := uuid.New()
	ts.members.roles[other] = map[uuid.UUID]servers.Role{ts.user: servers.RoleMember}
	conn := ts.dial(t)

	write(t, conn, map[string]any{"type": "subscribe", "workspaceId": ts.workspace})
	read(t, conn)
	waitFor(t, func() bool { return ts.hub.Subscribers(ts.workspace) == 1 })

	write(t, conn, map[string]any{"type": "subscribe", "workspaceId": other})
	read(t, conn)

	waitFor(t, func() bool {
		return ts.hub.Subscribers(ts.workspace) == 0 && ts.hub.Subscribers(other) == 1
	})
}

func TestUnsubscribeAndPing(t *testing.T) {
	ts := newTestServer(t)
	conn := ts.dial(t)

	write(t, conn, map[string]any{"type": "ping"})
	if frame := read(t, conn); frame["type"] != "pong" {
		t.Errorf("frame = %v, want pong", frame)
	}

	write(t, conn, map[string]any{"type": "subscribe", "workspaceId": ts.workspace})
	read(t, conn)
	waitFor(t, func() bool { return ts.hub.Subscribers(ts.workspace) == 1 })

	write(t, conn, map[string]any{"type": "unsubscribe"})
	if frame := read(t, conn); frame["type"] != "unsubscribed" {
		t.Errorf("frame = %v, want unsubscribed", frame)
	}
	waitFor(t, func() bool { return ts.hub.Subscribers(ts.workspace) == 0 })
}

func TestUnknownFrameIsAnError(t *testing.T) {
	ts := newTestServer(t)
	conn := ts.dial(t)

	write(t, conn, map[string]any{"type": "что-то"})

	frame := read(t, conn)
	if frame["type"] != "error" || frame["code"] != "invalid_request" {
		t.Errorf("frame = %v, want an invalid_request error", frame)
	}
}

// Closing the connection releases the subscription, so a hub does not grow
// with every client that walks away.
func TestClosingTheConnectionUnsubscribes(t *testing.T) {
	ts := newTestServer(t)
	conn := ts.dial(t)

	write(t, conn, map[string]any{"type": "subscribe", "workspaceId": ts.workspace})
	read(t, conn)
	waitFor(t, func() bool { return ts.hub.Subscribers(ts.workspace) == 1 })

	_ = conn.Close(websocket.StatusNormalClosure, "done")

	waitFor(t, func() bool { return ts.hub.Subscribers(ts.workspace) == 0 })
}

// waitFor polls a condition for a short while: the gateway does its work in
// goroutines, and a test must not race it.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within two seconds")
}

// A conversation has no server, so its events ride the account stream: any
// connection of that person gets them, whatever server it is watching.
func TestPersonalStreamCarriesConversationEvents(t *testing.T) {
	ts := newTestServer(t)
	conn := ts.dial(t)
	conv := uuid.New()
	waitFor(t, func() bool { return ts.hub.PersonalSubscribers(ts.user) == 1 })

	ts.hub.PublishToUser(ts.user, events.Event{
		Kind: events.KindMessageCreated, ChannelID: conv, Seq: 1,
		Payload: map[string]any{"content": "привет"},
	})

	frame := read(t, conn)
	if frame["type"] != string(events.KindMessageCreated) {
		t.Errorf("type = %v, want %v", frame["type"], events.KindMessageCreated)
	}
	if frame["channelId"] != conv.String() {
		t.Errorf("channelId = %v, want %v", frame["channelId"], conv)
	}
	if _, ok := frame["workspaceId"]; ok {
		t.Errorf("a conversation event named a workspace: %v", frame)
	}
}

// An event of somebody else never reaches this connection.
func TestPersonalStreamDoesNotLeakOtherAccounts(t *testing.T) {
	ts := newTestServer(t)
	conn := ts.dial(t)
	waitFor(t, func() bool { return ts.hub.PersonalSubscribers(ts.user) == 1 })

	ts.hub.PublishToUser(uuid.New(), events.Event{
		Kind: events.KindConversationCreated, ChannelID: uuid.New(),
	})
	write(t, conn, map[string]any{"type": "ping"})
	if frame := read(t, conn); frame["type"] != "pong" {
		t.Errorf("frame = %v, want pong: an event of another account arrived", frame)
	}
}

// Reconnecting with per-conversation cursors catches up over the account
// stream, and the live stream does not repeat what the catch-up delivered.
func TestConversationCatchUpByCursor(t *testing.T) {
	ts := newTestServer(t)
	conv := uuid.New()
	ts.history.missed[conv] = []events.Event{
		{Kind: events.KindMessageCreated, ChannelID: conv, Seq: 2},
		{Kind: events.KindMessageCreated, ChannelID: conv, Seq: 3},
	}
	conn := ts.dial(t)

	write(t, conn, map[string]any{
		"type":          "resume",
		"conversations": map[string]any{conv.String(): 1},
	})
	for _, wantSeq := range []float64{2, 3} {
		if frame := read(t, conn); frame["seq"] != wantSeq {
			t.Fatalf("seq = %v, want %v", frame["seq"], wantSeq)
		}
	}
	if frame := read(t, conn); frame["type"] != "resumed" {
		t.Fatalf("frame = %v, want resumed", frame)
	}

	waitFor(t, func() bool { return ts.hub.PersonalSubscribers(ts.user) == 1 })
	ts.hub.PublishToUser(ts.user, events.Event{Kind: events.KindMessageCreated, ChannelID: conv, Seq: 3})
	ts.hub.PublishToUser(ts.user, events.Event{Kind: events.KindMessageCreated, ChannelID: conv, Seq: 4})
	if frame := read(t, conn); frame["seq"] != float64(4) {
		t.Errorf("seq = %v, want 4: seq 3 came twice", frame["seq"])
	}
}

// A personal read.changed still names its server: the client uses that id to
// refresh the unread badges of the sidebar.
func TestPersonalStreamKeepsTheWorkspaceOfReadEvents(t *testing.T) {
	ts := newTestServer(t)
	conn := ts.dial(t)
	waitFor(t, func() bool { return ts.hub.PersonalSubscribers(ts.user) == 1 })

	ts.hub.PublishToUser(ts.user, events.Event{
		Kind: events.KindReadChanged, WorkspaceID: ts.workspace, ChannelID: ts.channel, Seq: 4,
	})

	frame := read(t, conn)
	if frame["workspaceId"] != ts.workspace.String() {
		t.Errorf("workspaceId = %v, want %v", frame["workspaceId"], ts.workspace)
	}
	if frame["channelId"] != ts.channel.String() {
		t.Errorf("channelId = %v, want %v", frame["channelId"], ts.channel)
	}
}
