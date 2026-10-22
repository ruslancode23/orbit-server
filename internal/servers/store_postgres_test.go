package servers

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/perms"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
)

// The roles SQL on a real PostgreSQL: @everyone made with the server, the
// administrator role granted and taken, base permissions summed per member
// in the lists, and the old flags written through @everyone.
func TestPostgresRoles(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := t.Context()
	store := NewPostgresStore(pool)

	user := func(nickname string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO users (nickname, is_guest) VALUES ($1, true) RETURNING id`, nickname).Scan(&id); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		return id
	}
	owner, admin, member := user("owner"), user("admin"), user("member")

	everyone := Permissions{AttachFiles: true}.everyoneOf()
	workspace, err := store.CreateWorkspace(ctx, NewWorkspace{Name: "Сервер"}, everyone, owner)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if workspace.Everyone != everyone || !workspace.MemberPermissions.AttachFiles || workspace.MemberPermissions.Invite {
		t.Errorf("new server = %+v, want @everyone %v", workspace, everyone.Names())
	}
	for _, id := range []uuid.UUID{admin, member} {
		if _, err := store.AddMember(ctx, workspace.ID, id, RoleMember); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}
	if err := store.GrantAdmin(ctx, workspace.ID, admin); err != nil {
		t.Fatalf("GrantAdmin: %v", err)
	}
	// Granting twice keeps one role and one grant.
	if err := store.GrantAdmin(ctx, workspace.ID, admin); err != nil {
		t.Fatalf("GrantAdmin again: %v", err)
	}

	access, err := store.Access(ctx, workspace.ID, admin)
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if access.Role() != RoleAdmin || access.Base() != perms.All || len(access.Server.Roles) != 2 {
		t.Errorf("admin access = role %s, base %v, %d roles", access.Role(), access.Base().Names(), len(access.Server.Roles))
	}
	if _, err := store.Access(ctx, workspace.ID, uuid.New()); !errors.Is(err, ErrNotMember) {
		t.Errorf("outsider Access: err = %v, want ErrNotMember", err)
	}

	members, err := store.ListMembers(ctx, workspace.ID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	roles := map[uuid.UUID]Role{}
	for _, m := range members {
		roles[m.User.ID] = m.Role
		if m.User.ID == member && m.Permissions != everyone {
			t.Errorf("member permissions = %v, want @everyone %v", m.Permissions.Names(), everyone.Names())
		}
	}
	if roles[owner] != RoleOwner || roles[admin] != RoleAdmin || roles[member] != RoleMember {
		t.Errorf("roles = %v", roles)
	}

	memberships, err := store.ListMemberships(ctx, admin)
	if err != nil || len(memberships) != 1 || memberships[0].Role != RoleAdmin {
		t.Errorf("memberships = %+v, %v; want admin on one server", memberships, err)
	}

	// The old flags go through @everyone and come back.
	if err := store.SetEveryone(ctx, workspace.ID, everyone|perms.CreateInvite); err != nil {
		t.Fatalf("SetEveryone: %v", err)
	}
	updated, err := store.Workspace(ctx, workspace.ID)
	if err != nil || !updated.MemberPermissions.Invite {
		t.Errorf("after SetEveryone = %+v, %v; want invites on", updated.MemberPermissions, err)
	}

	if err := store.RevokeAdmin(ctx, workspace.ID, admin); err != nil {
		t.Fatalf("RevokeAdmin: %v", err)
	}
	if access, err := store.Access(ctx, workspace.ID, admin); err != nil || access.Role() != RoleMember {
		t.Errorf("after RevokeAdmin role = %s, %v; want member", access.Role(), err)
	}
}
