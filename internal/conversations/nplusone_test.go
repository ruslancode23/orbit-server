package conversations

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/profile"
)

// countingProfiles answers like the profile module and counts its calls: a list
// of conversations must fill every participant in one batch.
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
			AccentColor: profile.AccentViolet,
		}
	}
	return out, nil
}

func TestConversationListFillsParticipantsInOneCall(t *testing.T) {
	w := newWorld(t)
	if _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob}); err != nil {
		t.Fatalf("Start direct: %v", err)
	}
	if _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob, w.vika}); err != nil {
		t.Fatalf("Start group: %v", err)
	}

	profiles := &countingProfiles{}
	w.svc.SetProfiles(profiles)

	list, err := w.svc.List(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if profiles.calls != 1 {
		t.Errorf("profile calls = %d, want 1 for the whole conversation list", profiles.calls)
	}
	if profiles.loaded != 3 {
		t.Errorf("profiles loaded = %d, want the 3 distinct participants", profiles.loaded)
	}
	for _, conversation := range list {
		for _, member := range conversation.Members {
			if member.DisplayName != "name-"+member.ID.String()[:8] {
				t.Errorf("member = %+v, want the filled profile", member.Public)
			}
		}
	}
}
