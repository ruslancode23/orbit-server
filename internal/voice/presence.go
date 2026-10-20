package voice

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/events"
)

// How long a cached room may be trusted before reading LiveKit again. Webhooks
// keep it fresh; the interval is the fallback for a missed webhook and for the
// first read after a restart.
const presenceStaleAfter = 30 * time.Second

// ChannelLookup is the part of internal/channels presence needs: it turns a
// channel id into its server and finds the voice channels of a server.
type ChannelLookup interface {
	Lookup(ctx context.Context, channelID uuid.UUID) (channels.Channel, error)
	ListVoice(ctx context.Context, workspaceID uuid.UUID) ([]channels.Channel, error)
}

// RoomReader lists who is in one LiveKit room.
type RoomReader interface {
	List(ctx context.Context, room string) ([]Participant, error)
}

// Publisher announces presence changes to the clients watching the server.
type Publisher interface {
	Publish(event events.Event)
}

type roomPresence struct {
	participants []Participant
	syncedAt     time.Time
}

// Presence keeps who is in each voice room in memory. LiveKit is the source of
// truth: webhooks update it at once, and a stale entry is read back when a
// client asks after a restart or a missed event.
type Presence struct {
	mu     sync.Mutex
	rooms  map[uuid.UUID]roomPresence
	lookup ChannelLookup
	live   RoomReader
	events Publisher
	log    *slog.Logger
	now    func() time.Time
	stale  time.Duration
}

func NewPresence(lookup ChannelLookup, live RoomReader, publisher Publisher, log *slog.Logger) *Presence {
	return &Presence{
		rooms:  map[uuid.UUID]roomPresence{},
		lookup: lookup,
		live:   live,
		events: publisher,
		log:    log,
		now:    time.Now,
		stale:  presenceStaleAfter,
	}
}

// ForWorkspace returns the presence of every voice channel of a server. A room
// that was never read (after a restart) or has gone stale is read from LiveKit
// before the answer is built; a LiveKit failure falls back to what is cached.
func (p *Presence) ForWorkspace(ctx context.Context, workspaceID uuid.UUID) []api.VoicePresence {
	voiceChannels, err := p.lookup.ListVoice(ctx, workspaceID)
	if err != nil {
		p.log.ErrorContext(ctx, "list voice channels", slog.Any("error", err))
		return []api.VoicePresence{}
	}

	out := make([]api.VoicePresence, 0, len(voiceChannels))
	for _, channel := range voiceChannels {
		p.refreshIfStale(ctx, channel)
		out = append(out, p.snapshot(channel.ID))
	}
	return out
}

// Refresh reads one room from LiveKit and tells the watchers if it changed.
// It is what a webhook calls for a joined, left or updated participant.
func (p *Presence) Refresh(ctx context.Context, channelID uuid.UUID) {
	channel, err := p.lookup.Lookup(ctx, channelID)
	if err != nil || channel.Kind != channels.KindVoice {
		return
	}
	p.update(ctx, channel, true)
}

// Finish clears a room that LiveKit says is over.
func (p *Presence) Finish(ctx context.Context, channelID uuid.UUID) {
	channel, err := p.lookup.Lookup(ctx, channelID)
	if err != nil {
		return
	}
	p.set(channel, nil, true)
}

func (p *Presence) refreshIfStale(ctx context.Context, channel channels.Channel) {
	p.mu.Lock()
	entry, ok := p.rooms[channel.ID]
	fresh := ok && p.now().Sub(entry.syncedAt) < p.stale
	p.mu.Unlock()
	if fresh {
		return
	}
	p.update(ctx, channel, false)
}

func (p *Presence) update(ctx context.Context, channel channels.Channel, announce bool) {
	participants, err := p.live.List(ctx, channel.ID.String())
	if err != nil {
		p.log.WarnContext(ctx, "read LiveKit participants", slog.Any("error", err),
			slog.String("channel", channel.ID.String()))
		// Remember the attempt so a broken LiveKit is not asked on every request.
		p.mu.Lock()
		entry := p.rooms[channel.ID]
		entry.syncedAt = p.now()
		p.rooms[channel.ID] = entry
		p.mu.Unlock()
		return
	}
	sort.Slice(participants, func(i, j int) bool { return participants[i].Name < participants[j].Name })
	p.set(channel, participants, announce)
}

func (p *Presence) set(channel channels.Channel, participants []Participant, announce bool) {
	p.mu.Lock()
	old, existed := p.rooms[channel.ID]
	changed := !existed || !sameParticipants(old.participants, participants)
	entry := roomPresence{participants: participants, syncedAt: p.now()}
	p.rooms[channel.ID] = entry
	p.mu.Unlock()

	if !announce || !changed {
		return
	}
	if p.events != nil {
		p.events.Publish(events.Event{
			Kind:        events.KindVoicePresence,
			WorkspaceID: channel.WorkspaceID,
			ChannelID:   channel.ID,
			Payload:     toAPIPresence(channel.ID, participants),
		})
	}
}

func (p *Presence) snapshot(channelID uuid.UUID) api.VoicePresence {
	p.mu.Lock()
	defer p.mu.Unlock()
	return toAPIPresence(channelID, p.rooms[channelID].participants)
}

func toAPIPresence(channelID uuid.UUID, participants []Participant) api.VoicePresence {
	out := make([]api.VoiceParticipant, 0, len(participants))
	for _, participant := range participants {
		out = append(out, api.VoiceParticipant{
			UserId:        participant.UserID,
			Name:          participant.Name,
			Muted:         participant.Muted,
			Deafened:      participant.Deafened,
			ScreenSharing: participant.Screen,
		})
	}
	return api.VoicePresence{ChannelId: channelID, Participants: out}
}

func sameParticipants(a, b []Participant) bool {
	return slices.EqualFunc(a, b, func(x, y Participant) bool { return x == y })
}
