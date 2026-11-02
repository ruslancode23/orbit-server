package channels

import (
	"errors"
	"testing"

	"github.com/ruslancode23/orbit-server/internal/audit"
	"github.com/ruslancode23/orbit-server/internal/events"
)

// The channel's slowmode is set by whoever manages the channel and is visible
// to every member as part of the channel.
func TestUpdateSetsSlowmode(t *testing.T) {
	w := newWorld(t)
	seconds := 45

	updated, err := w.svc.Update(t.Context(), w.general.ID, w.admin, Update{SlowmodeSeconds: &seconds})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Channel.SlowmodeSeconds != seconds {
		t.Errorf("slowmode = %d, want %d", updated.Channel.SlowmodeSeconds, seconds)
	}

	// A plain member reads the same setting.
	view, err := w.svc.Get(t.Context(), w.general.ID, w.member)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.Channel.SlowmodeSeconds != seconds {
		t.Errorf("member sees slowmode = %d, want %d", view.Channel.SlowmodeSeconds, seconds)
	}
}

// Zero turns slowmode off, and a value outside what the column stores is
// refused before anything is written.
func TestUpdateRejectsSlowmodeOutOfRange(t *testing.T) {
	w := newWorld(t)
	seconds := 30
	if _, err := w.svc.Update(t.Context(), w.general.ID, w.admin, Update{SlowmodeSeconds: &seconds}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	for _, bad := range []int{-1, maxSlowmodeSeconds + 1} {
		if _, err := w.svc.Update(t.Context(), w.general.ID, w.admin, Update{SlowmodeSeconds: &bad}); !errors.Is(err, ErrInvalidSlowmode) {
			t.Errorf("slowmode %d = %v, want ErrInvalidSlowmode", bad, err)
		}
	}
	view, err := w.svc.Get(t.Context(), w.general.ID, w.member)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.Channel.SlowmodeSeconds != seconds {
		t.Errorf("slowmode = %d, want the refused value to change nothing (%d)", view.Channel.SlowmodeSeconds, seconds)
	}

	off := 0
	if _, err := w.svc.Update(t.Context(), w.general.ID, w.admin, Update{SlowmodeSeconds: &off}); err != nil {
		t.Errorf("turning slowmode off: %v", err)
	}
}

// A member without manageChannels may not change the slowmode: the rule is in
// the service, not in the client's button.
func TestSlowmodeNeedsManageChannels(t *testing.T) {
	w := newWorld(t)
	seconds := 30

	if _, err := w.svc.Update(t.Context(), w.general.ID, w.member, Update{SlowmodeSeconds: &seconds}); !errors.Is(err, ErrForbidden) {
		t.Errorf("member update = %v, want ErrForbidden", err)
	}
}

// Clients hear about the new setting through channel.changed, and the journal
// keeps what it was set to.
func TestSlowmodeChangeIsAnnouncedAndRecorded(t *testing.T) {
	w := newWorld(t)
	watchers := &recordingWatchers{}
	w.svc.SetWatchers(watchers)
	journal := &captureAuditor{}
	w.svc.SetAuditor(journal)
	seconds := 120

	if _, err := w.svc.Update(t.Context(), w.general.ID, w.admin, Update{SlowmodeSeconds: &seconds}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(watchers.events) != 1 {
		t.Fatalf("announced %+v, want one change", watchers.events)
	}
	event := watchers.events[0]
	if event.Kind != events.KindChannelChanged || event.ChannelID != w.general.ID {
		t.Errorf("event = %+v, want channel.changed of %v", event, w.general.ID)
	}
	payload, ok := event.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload = %T, want the channel id", event.Payload)
	}
	if payload["channelId"] != w.general.ID {
		t.Errorf("payload channelId = %v, want %v", payload["channelId"], w.general.ID)
	}

	if len(journal.entries) != 1 {
		t.Fatalf("journal = %+v, want one entry", journal.entries)
	}
	if journal.entries[0].Action != audit.ActionChannelUpdate {
		t.Errorf("action = %s, want channel.update", journal.entries[0].Action)
	}
	if journal.entries[0].Metadata["slowmodeSeconds"] != seconds {
		t.Errorf("metadata = %+v, want the new slowmode", journal.entries[0].Metadata)
	}
}
