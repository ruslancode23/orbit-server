package channels

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/events"
)

type recordingWatchers struct{ events []events.Event }

func (r *recordingWatchers) Announce(_ context.Context, event events.Event, _ ...uuid.UUID) {
	r.events = append(r.events, event)
}

// Every change to the channel list reaches the server's stream, so the other
// members see it without reloading. The payload names the channel only: the
// API view carries canWrite, which differs per reader.
func TestChannelChangesAreAnnounced(t *testing.T) {
	w := newWorld(t)
	watchers := &recordingWatchers{}
	w.svc.SetWatchers(watchers)

	created, err := w.svc.Create(t.Context(), w.workspace, w.admin, NewChannel{Name: "задачи"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	name := "дела"
	if _, err := w.svc.Update(t.Context(), created.Channel.ID, w.admin, Update{Name: &name}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := w.svc.Reorder(t.Context(), w.workspace, w.admin, []uuid.UUID{created.Channel.ID, w.general.ID}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	if err := w.svc.Delete(t.Context(), created.Channel.ID, w.admin); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	want := []struct {
		kind    events.Kind
		channel uuid.UUID
	}{
		{events.KindChannelChanged, created.Channel.ID},
		{events.KindChannelChanged, created.Channel.ID},
		{events.KindChannelChanged, uuid.Nil}, // the whole list moved
		{events.KindChannelDeleted, created.Channel.ID},
	}
	if len(watchers.events) != len(want) {
		t.Fatalf("events = %+v, want %d of them", watchers.events, len(want))
	}
	for i, wanted := range want {
		got := watchers.events[i]
		if got.Kind != wanted.kind || got.ChannelID != wanted.channel || got.WorkspaceID != w.workspace {
			t.Errorf("event %d = %s %s in %s, want %s %s in %s",
				i, got.Kind, got.ChannelID, got.WorkspaceID, wanted.kind, wanted.channel, w.workspace)
		}
		if payload, ok := got.Payload.(map[string]any); ok {
			if _, leaks := payload["canWrite"]; leaks {
				t.Errorf("event %d carries canWrite, which is per reader", i)
			}
		}
	}
}

// A refused change announces nothing: the list did not change.
func TestRefusedChannelChangesAreNotAnnounced(t *testing.T) {
	w := newWorld(t)
	watchers := &recordingWatchers{}
	w.svc.SetWatchers(watchers)

	name := "чужое"
	if _, err := w.svc.Update(t.Context(), w.general.ID, w.member, Update{Name: &name}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member rename: err = %v, want ErrForbidden", err)
	}
	if err := w.svc.Delete(t.Context(), w.general.ID, w.outsider); err == nil {
		t.Fatal("an outsider deleted a channel")
	}
	if _, err := w.svc.Create(t.Context(), w.workspace, w.admin, NewChannel{Name: "общий"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate: err = %v, want ErrNameTaken", err)
	}
	if len(watchers.events) != 0 {
		t.Errorf("events = %+v, want none", watchers.events)
	}
}
