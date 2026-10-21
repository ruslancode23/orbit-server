package invites

import (
	"testing"

	"github.com/ruslancode23/orbit-server/internal/events"
)

// The admins' list of links follows every change: a link made, used and
// revoked. A refused attempt changes nothing and announces nothing.
func TestLinkChangesReachTheStaff(t *testing.T) {
	w := newWorld(t)
	w.members.staff = nil

	if _, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.member, New{}); err == nil {
		t.Fatal("a member made a link on a server that does not allow it")
	}
	if len(w.members.staff) != 0 {
		t.Fatalf("a refused link announced %+v", w.members.staff)
	}

	invite, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.svc.Accept(t.Context(), invite.Code, w.stranger); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := w.svc.Revoke(t.Context(), invite.ID, w.owner); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if len(w.members.staff) != 3 {
		t.Fatalf("staff heard %d events, want 3 (made, used, revoked)", len(w.members.staff))
	}
	for _, event := range w.members.staff {
		if event.Kind != events.KindInvitesChanged || event.WorkspaceID != w.members.workspace.ID {
			t.Errorf("event = %+v, want invites.changed for the server", event)
		}
	}
}
