package conversations

import (
	"context"
	"testing"
	"time"

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
	if _, _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob}); err != nil {
		t.Fatalf("Start direct: %v", err)
	}
	if _, _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob, w.vika}); err != nil {
		t.Fatalf("Start group: %v", err)
	}
	for _, conversation := range w.store.conversations {
		seedLastMessage(w.store, conversation.ID, w.bob, 1, "one preview")
	}

	profiles := &countingProfiles{}
	w.svc.SetProfiles(profiles)

	page, err := w.svc.List(t.Context(), w.anna, "", 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	list := page.Conversations
	if profiles.calls != 1 {
		t.Errorf("profile calls = %d, want 1 for the whole conversation list", profiles.calls)
	}
	if profiles.loaded != 3 {
		t.Errorf("profiles loaded = %d, want the 3 distinct participants", profiles.loaded)
	}
	if w.store.listCalls != 1 {
		t.Errorf("store list calls = %d, want 1 for the whole page", w.store.listCalls)
	}
	for _, conversation := range list {
		for _, member := range conversation.Members {
			if member.DisplayName != "name-"+member.ID.String()[:8] {
				t.Errorf("member = %+v, want the filled profile", member.Public)
			}
		}
	}
}

// seedLastMessage gives a fake conversation the newest message the SQL list
// would have loaded, so the service can be tested without a database.
func seedLastMessage(store *fakeStore, id, author uuid.UUID, seq int64, content string) {
	conversation := store.conversations[id]
	lastAuthor := profile.Public{ID: author}
	conversation.LastMessage = &LastMessage{
		ID:        uuid.New(),
		Seq:       seq,
		Author:    &lastAuthor,
		Content:   content,
		Kind:      MessageKindUser,
		CreatedAt: time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
	}
}
