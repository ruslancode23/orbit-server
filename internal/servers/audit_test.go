package servers

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/audit"
)

// failingAuditor stands in for a journal whose database is unavailable.
type failingAuditor struct{ err error }

func (a failingAuditor) Record(context.Context, audit.Entry) error { return a.err }

// captureAuditor keeps every entry the service writes.
type captureAuditor struct{ entries []audit.Entry }

func (a *captureAuditor) Record(_ context.Context, entry audit.Entry) error {
	a.entries = append(a.entries, entry)
	return nil
}

func newAuditFixture(t *testing.T) (*fakeStore, *Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	store := newFakeStore()
	svc := NewService(store, nil, Icons{})
	owner, member := uuid.New(), uuid.New()
	workspace, err := svc.Create(t.Context(), NewWorkspace{Name: "Сервер"}, owner)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := store.AddMember(t.Context(), workspace.Workspace.ID, member, RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return store, svc, owner, workspace.Workspace.ID
}

// A broken journal must not turn a ban, a kick or a role change into an error:
// the moderation action is already committed and must stand on its own.
func TestBrokenJournalDoesNotUndoModeration(t *testing.T) {
	ctx := t.Context()
	store, svc, owner, workspaceID := newAuditFixture(t)
	svc.SetAuditor(failingAuditor{err: errors.New("journal database is down")})

	target := uuid.New()
	if _, err := store.AddMember(ctx, workspaceID, target, RoleMember); err != nil {
		t.Fatalf("add target: %v", err)
	}

	if _, err := svc.Ban(ctx, workspaceID, owner, target, "spam"); err != nil {
		t.Fatalf("Ban = %v, want success despite the journal", err)
	}
	if banned, err := store.IsBanned(ctx, workspaceID, target); err != nil || !banned {
		t.Fatalf("target is not banned (err %v): the action was rolled back", err)
	}
	if err := svc.Unban(ctx, workspaceID, owner, target); err != nil {
		t.Fatalf("Unban = %v", err)
	}
	if banned, err := store.IsBanned(ctx, workspaceID, target); err != nil || banned {
		t.Fatalf("target is still banned (err %v)", err)
	}

	if _, err := store.AddMember(ctx, workspaceID, target, RoleMember); err != nil {
		t.Fatalf("re-add target: %v", err)
	}
	if err := svc.Remove(ctx, workspaceID, owner, target); err != nil {
		t.Fatalf("Remove = %v, want success despite the journal", err)
	}
	if _, ok := store.members[workspaceID][target]; ok {
		t.Fatal("target is still a member: the kick was rolled back")
	}

	if _, err := store.AddMember(ctx, workspaceID, target, RoleMember); err != nil {
		t.Fatalf("re-add target: %v", err)
	}
	if _, err := svc.SetRole(ctx, workspaceID, owner, target, RoleAdmin); err != nil {
		t.Fatalf("SetRole = %v, want success despite the journal", err)
	}
}

// Every moderation action in this module leaves one row, with the actor, the
// target and the little metadata the reader needs to understand it.
func TestModerationIsRecorded(t *testing.T) {
	ctx := t.Context()
	store, svc, owner, workspaceID := newAuditFixture(t)
	journal := &captureAuditor{}
	svc.SetAuditor(journal)

	target := uuid.New()
	if _, err := store.AddMember(ctx, workspaceID, target, RoleMember); err != nil {
		t.Fatalf("add target: %v", err)
	}
	if _, err := svc.Ban(ctx, workspaceID, owner, target, "spam"); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	if err := svc.Unban(ctx, workspaceID, owner, target); err != nil {
		t.Fatalf("Unban: %v", err)
	}
	if _, err := store.AddMember(ctx, workspaceID, target, RoleMember); err != nil {
		t.Fatalf("re-add target: %v", err)
	}
	if err := svc.Remove(ctx, workspaceID, owner, target); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := store.AddMember(ctx, workspaceID, target, RoleMember); err != nil {
		t.Fatalf("re-add target: %v", err)
	}
	role, err := svc.CreateRole(ctx, workspaceID, owner, NewRole{Name: "Модератор"})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if _, err := svc.GrantRole(ctx, workspaceID, owner, target, role.ID); err != nil {
		t.Fatalf("GrantRole: %v", err)
	}
	if _, err := svc.SetRole(ctx, workspaceID, owner, target, RoleOwner); err != nil {
		t.Fatalf("hand ownership over: %v", err)
	}

	wanted := []audit.Action{
		audit.ActionMemberBan,
		audit.ActionMemberUnban,
		audit.ActionMemberKick,
		audit.ActionRoleCreate,
		audit.ActionMemberRoleGrant,
		audit.ActionOwnershipTransfer,
	}
	if len(journal.entries) != len(wanted) {
		t.Fatalf("entries = %d, want %d: %+v", len(journal.entries), len(wanted), journal.entries)
	}
	for i, action := range wanted {
		entry := journal.entries[i]
		if entry.Action != action {
			t.Errorf("entry %d = %s, want %s", i, entry.Action, action)
		}
		if entry.WorkspaceID != workspaceID {
			t.Errorf("entry %d workspace = %s, want %s", i, entry.WorkspaceID, workspaceID)
		}
		if entry.ActorID == nil || *entry.ActorID != owner {
			t.Errorf("entry %d actor = %v, want %s", i, entry.ActorID, owner)
		}
	}

	if entry := journal.entries[0]; entry.TargetType != audit.TargetUser || entry.TargetID == nil || *entry.TargetID != target {
		t.Errorf("ban entry = %+v, want the user target", entry)
	}
	grant := journal.entries[4]
	if grant.TargetID == nil || *grant.TargetID != target {
		t.Errorf("grant target = %v, want the member", grant.TargetID)
	}
	if grant.Metadata["roleId"] != role.ID || grant.Metadata["roleName"] != "Модератор" {
		t.Errorf("grant metadata = %v, want the role id and name", grant.Metadata)
	}
	transfer := journal.entries[5]
	if transfer.TargetID == nil || *transfer.TargetID != target {
		t.Errorf("ownership target = %v, want the new owner", transfer.TargetID)
	}
	if transfer.Metadata["previousOwnerId"] != owner {
		t.Errorf("ownership metadata = %v, want the previous owner", transfer.Metadata)
	}
}

// Leaving a server by oneself is not a moderation action and is not recorded.
func TestLeavingIsNotRecorded(t *testing.T) {
	ctx := t.Context()
	store, svc, _, workspaceID := newAuditFixture(t)
	journal := &captureAuditor{}
	svc.SetAuditor(journal)

	leaver := uuid.New()
	if _, err := store.AddMember(ctx, workspaceID, leaver, RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := svc.Remove(ctx, workspaceID, leaver, leaver); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if len(journal.entries) != 0 {
		t.Fatalf("entries = %+v, want none for leaving", journal.entries)
	}
}
