package privacy

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/events"
)

type privacyRecorder struct{ heard map[uuid.UUID][]events.Kind }

func (r *privacyRecorder) PublishToUser(userID uuid.UUID, event events.Event) {
	if r.heard == nil {
		r.heard = map[uuid.UUID][]events.Kind{}
	}
	r.heard[userID] = append(r.heard[userID], event.Kind)
}

// Every privacy change reaches the account's own devices — and only them: the
// blocked side learns nothing from the stream.
func TestPrivacyChangesReachOnlyTheOwnDevices(t *testing.T) {
	w := newWorld(t)
	recorder := &privacyRecorder{}
	w.svc.SetPublisher(recorder)

	if err := w.svc.Block(t.Context(), w.anna, w.bob); err != nil {
		t.Fatalf("Block: %v", err)
	}
	if err := w.svc.Unblock(t.Context(), w.anna, w.bob); err != nil {
		t.Fatalf("Unblock: %v", err)
	}
	if err := w.svc.SetPolicy(t.Context(), w.anna, PolicyFriends); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if err := w.svc.SetFriendRequestPolicy(t.Context(), w.anna, FriendRequestsNobody); err != nil {
		t.Fatalf("SetFriendRequestPolicy: %v", err)
	}
	// Refused changes say nothing.
	_ = w.svc.Block(t.Context(), w.anna, w.anna)
	_ = w.svc.SetPolicy(t.Context(), w.anna, Policy("everyone-and-their-dog"))

	if got := recorder.heard[w.anna]; len(got) != 4 {
		t.Errorf("anna heard %v, want four privacy.changed", got)
	}
	for _, kind := range recorder.heard[w.anna] {
		if kind != events.KindPrivacyChanged {
			t.Errorf("anna heard %s, want privacy.changed", kind)
		}
	}
	if got := recorder.heard[w.bob]; len(got) != 0 {
		t.Errorf("the blocked account heard %v", got)
	}
}
