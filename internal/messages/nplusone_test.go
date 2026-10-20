package messages

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/profile"
)

// countingProfiles answers like the profile module and counts its calls: a page
// of history must fill every author in one batch.
type countingProfiles struct {
	calls  int
	loaded int
}

func (p *countingProfiles) Profiles(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]profile.Public, error) {
	p.calls++
	p.loaded += len(ids)
	out := make(map[uuid.UUID]profile.Public, len(ids))
	for _, id := range ids {
		out[id] = profile.Public{
			ID:          id,
			Nickname:    "nick-" + id.String()[:8],
			DisplayName: "name-" + id.String()[:8],
			AccentColor: profile.AccentTeal,
		}
	}
	return out, nil
}

func TestHistoryFillsAuthorsInOneCall(t *testing.T) {
	w := newWorld(t)
	send(t, w, w.anna, "a-1", "one")
	send(t, w, w.bob, "b-1", "two")
	send(t, w, w.admin, "c-1", "three")

	profiles := &countingProfiles{}
	w.svc.SetProfiles(profiles)

	page, err := w.svc.History(t.Context(), w.channel, w.anna, nil, 50)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if profiles.calls != 1 {
		t.Errorf("profile calls = %d, want 1 for the whole page", profiles.calls)
	}
	if profiles.loaded != len(page.Messages) {
		t.Errorf("profiles loaded = %d, want %d", profiles.loaded, len(page.Messages))
	}
	for _, message := range page.Messages {
		if message.Author == nil || message.Author.DisplayName != "name-"+message.Author.ID.String()[:8] {
			t.Errorf("author = %+v, want the filled profile", message.Author)
		}
	}
}
