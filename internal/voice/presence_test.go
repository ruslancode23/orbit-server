package voice

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/events"
)

type fakeLookup struct {
	channels map[uuid.UUID]channels.Channel
}

func (f *fakeLookup) Lookup(_ context.Context, id uuid.UUID) (channels.Channel, error) {
	channel, ok := f.channels[id]
	if !ok {
		return channels.Channel{}, channels.ErrNotFound
	}
	return channel, nil
}

func (f *fakeLookup) ListVoice(_ context.Context, workspaceID uuid.UUID) ([]channels.Channel, error) {
	out := []channels.Channel{}
	for _, channel := range f.channels {
		if channel.WorkspaceID == workspaceID && channel.Kind == channels.KindVoice {
			out = append(out, channel)
		}
	}
	return out, nil
}

type fakeRooms struct {
	participants map[string][]Participant
	err          error
	calls        int
}

func (f *fakeRooms) List(_ context.Context, room string) ([]Participant, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.participants[room], nil
}

type fakePublisher struct {
	events []events.Event
}

func (f *fakePublisher) Publish(event events.Event) { f.events = append(f.events, event) }

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func presenceFixture() (*Presence, *fakeRooms, *fakePublisher, uuid.UUID, uuid.UUID) {
	workspace := uuid.New()
	channel := uuid.New()
	lookup := &fakeLookup{channels: map[uuid.UUID]channels.Channel{
		channel: {ID: channel, WorkspaceID: workspace, Kind: channels.KindVoice, Name: "room"},
	}}
	rooms := &fakeRooms{participants: map[string][]Participant{
		channel.String(): {{UserID: uuid.New(), Name: "Anna"}},
	}}
	publisher := &fakePublisher{}
	presence := NewPresence(lookup, rooms, publisher, testLogger())
	return presence, rooms, publisher, workspace, channel
}

func TestPresenceReadsLiveKitOnceAndCaches(t *testing.T) {
	presence, rooms, _, workspace, channel := presenceFixture()

	first := presence.ForWorkspace(t.Context(), workspace)
	if len(first) != 1 || first[0].ChannelId != channel {
		t.Fatalf("presence = %+v, want one entry for the channel", first)
	}
	if len(first[0].Participants) != 1 || first[0].Participants[0].Name != "Anna" {
		t.Errorf("participants = %+v, want Anna", first[0].Participants)
	}
	if rooms.calls != 1 {
		t.Errorf("LiveKit reads = %d, want 1", rooms.calls)
	}

	// Within the stale window the snapshot comes from memory.
	presence.ForWorkspace(t.Context(), workspace)
	if rooms.calls != 1 {
		t.Errorf("LiveKit reads = %d after the second read, want 1", rooms.calls)
	}
}

func TestPresenceRefreshesWhenStale(t *testing.T) {
	presence, rooms, _, workspace, channel := presenceFixture()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	presence.now = func() time.Time { return now }
	presence.ForWorkspace(t.Context(), workspace)

	rooms.participants[channel.String()] = []Participant{{UserID: uuid.New(), Name: "Boris"}}
	now = now.Add(presence.stale)
	got := presence.ForWorkspace(t.Context(), workspace)

	if rooms.calls != 2 {
		t.Errorf("LiveKit reads = %d, want 2 after the cache went stale", rooms.calls)
	}
	if got[0].Participants[0].Name != "Boris" {
		t.Errorf("participants = %+v, want the fresh Boris", got[0].Participants)
	}
}

func TestPresenceRefreshAnnouncesChanges(t *testing.T) {
	presence, rooms, publisher, _, channel := presenceFixture()
	presence.Refresh(t.Context(), channel)
	if len(publisher.events) != 1 {
		t.Fatalf("events = %d, want 1", len(publisher.events))
	}
	event := publisher.events[0]
	if event.Kind != events.KindVoicePresence || event.ChannelID != channel {
		t.Errorf("event = %+v, want voice.presence for the channel", event)
	}

	// The same participants again must not produce a second event.
	presence.Refresh(t.Context(), channel)
	if len(publisher.events) != 1 {
		t.Errorf("events = %d after an unchanged refresh, want 1", len(publisher.events))
	}

	rooms.participants[channel.String()] = nil
	presence.Refresh(t.Context(), channel)
	if len(publisher.events) != 2 {
		t.Fatalf("events = %d after everyone left, want 2", len(publisher.events))
	}
	payload, ok := publisher.events[1].Payload.(api.VoicePresence)
	if !ok {
		t.Fatalf("payload = %T, want api.VoicePresence", publisher.events[1].Payload)
	}
	if len(payload.Participants) != 0 {
		t.Error("the last event must carry an empty participant list")
	}
}

func TestPresenceFinishClearsRoom(t *testing.T) {
	presence, _, publisher, workspace, channel := presenceFixture()
	presence.Refresh(t.Context(), channel)
	presence.Finish(t.Context(), channel)

	got := presence.ForWorkspace(t.Context(), workspace)
	if len(got) != 1 || len(got[0].Participants) != 0 {
		t.Errorf("presence after finish = %+v, want an empty room", got)
	}
	if len(publisher.events) != 2 {
		t.Errorf("events = %d, want join and clear", len(publisher.events))
	}
}

func TestPresenceKeepsCacheWhenLiveKitFails(t *testing.T) {
	presence, rooms, _, workspace, _ := presenceFixture()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	presence.now = func() time.Time { return now }
	presence.ForWorkspace(t.Context(), workspace)
	rooms.err = errors.New("LiveKit is down")

	now = now.Add(presence.stale)
	got := presence.ForWorkspace(t.Context(), workspace)
	if len(got[0].Participants) != 1 {
		t.Errorf("participants = %+v, want the cached Anna", got[0].Participants)
	}
	// The failed attempt is remembered, so a request storm does not become a
	// request storm against a broken LiveKit.
	presence.ForWorkspace(t.Context(), workspace)
	if rooms.calls != 2 {
		t.Errorf("LiveKit reads = %d, want 2 (one success, one failed)", rooms.calls)
	}
}
