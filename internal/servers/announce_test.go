package servers

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/events"
)

// A server event reaches every member on their own stream, once, whatever
// server each of them is watching; nobody outside hears it, and the server's
// shared stream is not used, so no connection hears it twice.
func TestServerEventsReachEveryMemberOnce(t *testing.T) {
	w, _, watchers := newProfileWorld(t)
	name := "Новое имя"

	if _, err := w.svc.Update(t.Context(), w.workspace, w.owner, WorkspaceUpdate{Name: &name}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	members := map[uuid.UUID]bool{w.owner: true, w.admin: true, w.member: true, w.member2: true}
	for id, heard := range watchers.personal {
		if !members[id] {
			t.Errorf("%s is not a member and heard %+v", id, heard)
			continue
		}
		if len(heard) != 1 || heard[0].Kind != events.KindWorkspaceUpdated {
			t.Errorf("member %s heard %+v, want one workspace.updated", id, heard)
		}
	}
	if len(watchers.personal) != len(members) {
		t.Errorf("%d accounts heard it, want %d", len(watchers.personal), len(members))
	}
	if len(watchers.published) != 0 {
		t.Errorf("the shared server stream carried %+v, want nothing", watchers.published)
	}
}

// A kicked member hears member.changed too, though they are no longer on the
// list: their own rail must lose the server.
func TestKickedMemberHearsTheirOwnRemoval(t *testing.T) {
	w, _, watchers := newProfileWorld(t)

	if err := w.svc.Remove(t.Context(), w.workspace, w.admin, w.member); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	heard := watchers.personal[w.member]
	if len(heard) != 1 || heard[0].Kind != events.KindMemberChanged {
		t.Fatalf("the kicked member heard %+v, want one member.changed", heard)
	}
	if got := watchers.personal[w.owner]; len(got) != 1 {
		t.Errorf("the owner heard %+v, want one member.changed", got)
	}
}
