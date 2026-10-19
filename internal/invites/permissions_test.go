package invites

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/servers"
)

// A server may let members invite; managing links stays with admins.
func TestMembersInviteWhenTheServerAllows(t *testing.T) {
	w := newWorld(t)
	workspaceID := w.members.workspace.ID

	if _, err := w.svc.Create(t.Context(), workspaceID, w.member, New{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("flag off: err = %v, want ErrForbidden", err)
	}

	w.members.workspace.MemberPermissions.Invite = true
	invite, err := w.svc.Create(t.Context(), workspaceID, w.member, New{})
	if err != nil {
		t.Fatalf("flag on: %v", err)
	}
	if invite.WorkspaceID != workspaceID || invite.Code == "" {
		t.Errorf("invite = %+v, want a live link to this server", invite)
	}

	if _, err := w.svc.List(t.Context(), workspaceID, w.member); !errors.Is(err, ErrForbidden) {
		t.Errorf("list: err = %v, want ErrForbidden", err)
	}
	if err := w.svc.Revoke(t.Context(), invite.ID, w.member); !errors.Is(err, ErrForbidden) {
		t.Errorf("revoke: err = %v, want ErrForbidden", err)
	}

	// Switched off again, it binds the next request.
	w.members.workspace.MemberPermissions.Invite = false
	if _, err := w.svc.Create(t.Context(), workspaceID, w.member, New{}); !errors.Is(err, ErrForbidden) {
		t.Errorf("flag off again: err = %v, want ErrForbidden", err)
	}
	// The owner invites whatever the flag says; an outsider learns nothing.
	if _, err := w.svc.Create(t.Context(), workspaceID, w.owner, New{}); err != nil {
		t.Errorf("owner: %v", err)
	}
	if _, err := w.svc.Create(t.Context(), workspaceID, w.stranger, New{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("stranger: err = %v, want ErrNotFound", err)
	}
	// Another server is another server, even with the flag on here.
	w.members.workspace.MemberPermissions = servers.Permissions{Invite: true}
	if _, err := w.svc.Create(t.Context(), uuid.New(), w.member, New{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("another server: err = %v, want ErrNotFound", err)
	}
}
