package servers

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/perms"
)

// The administrator (the server's admin role) manages roles below theirs and
// hands out only what they have; a plain member may not touch roles at all.
func TestCreateRoleRules(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.CreateRole(t.Context(), w.workspace, w.member, NewRole{Name: "Модератор"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("member CreateRole: err = %v, want ErrForbidden", err)
	}
	if _, err := w.svc.CreateRole(t.Context(), w.workspace, w.admin, NewRole{Name: "  "}); !errors.Is(err, ErrInvalidRoleName) {
		t.Errorf("blank name: err = %v, want ErrInvalidRoleName", err)
	}
	if _, err := w.svc.CreateRole(t.Context(), w.workspace, w.admin, NewRole{Name: "Ведущий", Color: "rainbow"}); !errors.Is(err, ErrInvalidColor) {
		t.Errorf("bad colour: err = %v, want ErrInvalidColor", err)
	}
	moderator, err := w.svc.CreateRole(t.Context(), w.workspace, w.admin, NewRole{
		Name: "Модератор", Color: "teal", Hoist: true, Permissions: perms.ManageMessages | perms.KickMembers,
	})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if moderator.Position != 1 || moderator.Name != "Модератор" {
		t.Errorf("new role = %+v, want it just above @everyone", moderator)
	}
	roles, err := w.svc.ListRoles(t.Context(), w.workspace, w.member)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	if len(roles) != 3 || !roles[len(roles)-1].Default {
		t.Errorf("roles = %+v, want three, @everyone last", roles)
	}
}

// Nobody grants what they lack: a role manager without banMembers cannot make
// a role that bans.
func TestRoleManagerGrantsOnlyWhatTheyHave(t *testing.T) {
	w := newWorld(t)
	manager, err := w.svc.CreateRole(t.Context(), w.workspace, w.owner, NewRole{Name: "Кадры", Permissions: perms.ManageRoles})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.GrantRole(t.Context(), w.workspace, w.owner, w.member2, manager.ID); err != nil {
		t.Fatalf("GrantRole: %v", err)
	}
	// The manager's role is at position 2 now that another role is made below it.
	if _, err := w.svc.CreateRole(t.Context(), w.workspace, w.member2, NewRole{Name: "Банщик", Permissions: perms.BanMembers}); !errors.Is(err, ErrForbidden) {
		t.Errorf("granting banMembers without having it: err = %v, want ErrForbidden", err)
	}
	if _, err := w.svc.CreateRole(t.Context(), w.workspace, w.member2, NewRole{Name: "Помощник"}); err != nil {
		t.Errorf("a role with nothing extra: %v", err)
	}
}

// @everyone changes its permissions only, is never deleted, granted or taken.
func TestEveryoneStaysEveryone(t *testing.T) {
	w := newWorld(t)
	roles, err := w.svc.ListRoles(t.Context(), w.workspace, w.owner)
	if err != nil {
		t.Fatal(err)
	}
	everyone := roles[len(roles)-1]
	name := "все"
	if _, err := w.svc.UpdateRole(t.Context(), everyone.ID, w.owner, RoleUpdate{Name: &name}); !errors.Is(err, ErrDefaultRole) {
		t.Errorf("rename @everyone: err = %v, want ErrDefaultRole", err)
	}
	set := everyone.Permissions | perms.CreateInvite
	updated, err := w.svc.UpdateRole(t.Context(), everyone.ID, w.owner, RoleUpdate{Permissions: &set})
	if err != nil || !updated.Permissions.Has(perms.CreateInvite) {
		t.Errorf("@everyone permissions = %v, %v; want invites", updated.Permissions.Names(), err)
	}
	if err := w.svc.DeleteRole(t.Context(), everyone.ID, w.owner); !errors.Is(err, ErrDefaultRole) {
		t.Errorf("delete @everyone: err = %v, want ErrDefaultRole", err)
	}
	if _, err := w.svc.GrantRole(t.Context(), w.workspace, w.owner, w.member, everyone.ID); !errors.Is(err, ErrDefaultRole) {
		t.Errorf("grant @everyone: err = %v, want ErrDefaultRole", err)
	}
}

// Giving a role takes a rank above both the role and the member; the owner
// may give anything to anyone else.
func TestGrantFollowsTheHierarchy(t *testing.T) {
	w := newWorld(t)
	helper, err := w.svc.CreateRole(t.Context(), w.workspace, w.owner, NewRole{Name: "Помощник", Permissions: perms.ManageMessages})
	if err != nil {
		t.Fatal(err)
	}
	member, err := w.svc.GrantRole(t.Context(), w.workspace, w.admin, w.member, helper.ID)
	if err != nil {
		t.Fatalf("admin GrantRole: %v", err)
	}
	if len(member.RoleIDs) != 1 || member.RoleIDs[0] != helper.ID || !member.Permissions.Has(perms.ManageMessages) {
		t.Errorf("member after grant = %+v", member)
	}
	if _, err := w.svc.GrantRole(t.Context(), w.workspace, w.member, w.member2, helper.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("member GrantRole: err = %v, want ErrForbidden", err)
	}
	// The admin may not take the administrator role off another admin: equal rank.
	admins, err := w.svc.ListRoles(t.Context(), w.workspace, w.owner)
	if err != nil {
		t.Fatal(err)
	}
	var adminRole uuid.UUID
	for _, role := range admins {
		if role.Permissions == perms.Administrator {
			adminRole = role.ID
		}
	}
	if _, err := w.svc.RevokeRole(t.Context(), w.workspace, w.admin, w.owner, adminRole); err == nil {
		t.Error("an admin acted on the owner")
	}
	if _, err := w.svc.RevokeRole(t.Context(), w.workspace, w.admin, w.member, helper.ID); err != nil {
		t.Errorf("admin RevokeRole: %v", err)
	}
}

// Reordering must name every role once; roles at or above the caller's own
// highest keep their places.
func TestReorderRolesRules(t *testing.T) {
	w := newWorld(t)
	a, err := w.svc.CreateRole(t.Context(), w.workspace, w.owner, NewRole{Name: "А"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := w.svc.CreateRole(t.Context(), w.workspace, w.owner, NewRole{Name: "Б"})
	if err != nil {
		t.Fatal(err)
	}
	roles, err := w.svc.ListRoles(t.Context(), w.workspace, w.owner)
	if err != nil {
		t.Fatal(err)
	}
	var adminRole uuid.UUID
	for _, role := range roles {
		if role.Permissions == perms.Administrator {
			adminRole = role.ID
		}
	}
	// Highest first: administrator, А, Б (Б was made last, so it is at 1).
	if _, err := w.svc.ReorderRoles(t.Context(), w.workspace, w.admin, []uuid.UUID{adminRole, a.ID}); !errors.Is(err, ErrIncompleteRoleOrder) {
		t.Errorf("partial order: err = %v, want ErrIncompleteRoleOrder", err)
	}
	if _, err := w.svc.ReorderRoles(t.Context(), w.workspace, w.admin, []uuid.UUID{adminRole, b.ID, a.ID}); err != nil {
		t.Errorf("admin swaps two roles below: %v", err)
	}
	if _, err := w.svc.ReorderRoles(t.Context(), w.workspace, w.admin, []uuid.UUID{b.ID, adminRole, a.ID}); !errors.Is(err, ErrForbidden) {
		t.Errorf("admin moves a role above their own: err = %v, want ErrForbidden", err)
	}
	if _, err := w.svc.ReorderRoles(t.Context(), w.workspace, w.owner, []uuid.UUID{b.ID, adminRole, a.ID}); err != nil {
		t.Errorf("owner reorders anything: %v", err)
	}
}
