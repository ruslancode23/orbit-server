package servers

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/events"
)

// Deleting a server tells every former member on their own stream — most of
// them are not watching this server — and ends their subscriptions to it.
// Nobody outside the server hears anything.
func TestDeleteTellsEveryMemberAndEndsTheirStreams(t *testing.T) {
	w, _, watchers := newProfileWorld(t)

	if err := w.svc.Delete(t.Context(), w.workspace, w.owner); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	members := []uuid.UUID{w.owner, w.admin, w.member, w.member2}
	for _, id := range members {
		got := watchers.personal[id]
		if len(got) != 1 || got[0].Kind != events.KindWorkspaceDeleted || got[0].WorkspaceID != w.workspace {
			t.Errorf("member %s heard %+v, want one workspace.deleted for the server", id, got)
		}
	}
	if heard := watchers.personal[w.outsider]; len(heard) != 0 {
		t.Errorf("an outsider heard %+v", heard)
	}
	if len(watchers.revoked) != len(members) {
		t.Errorf("revoked %d subscriptions, want %d", len(watchers.revoked), len(members))
	}
}

// A refused delete announces nothing and cuts nobody off.
func TestRefusedDeleteTellsNobody(t *testing.T) {
	w, _, watchers := newProfileWorld(t)

	if err := w.svc.Delete(t.Context(), w.workspace, w.admin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin delete: err = %v, want ErrForbidden", err)
	}
	if len(watchers.personal) != 0 || len(watchers.revoked) != 0 {
		t.Errorf("personal = %+v, revoked = %v, want nothing", watchers.personal, watchers.revoked)
	}
}
