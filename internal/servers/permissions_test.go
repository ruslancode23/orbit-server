package servers

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
)

func on() *bool  { v := true; return &v }
func off() *bool { v := false; return &v }

var allPermissions = []Permission{PermissionInvite, PermissionCreateChannels, PermissionAttachFiles}

func TestAllows(t *testing.T) {
	everything := Permissions{Invite: true, CreateChannels: true, AttachFiles: true}
	nothing := Permissions{}

	for _, permission := range allPermissions {
		t.Run(string(permission), func(t *testing.T) {
			// The owner and admins may always, whatever the server says.
			for _, role := range []Role{RoleOwner, RoleAdmin} {
				if !nothing.Allows(role, permission) {
					t.Errorf("%s refused with everything off", role)
				}
			}
			if !everything.Allows(RoleMember, permission) {
				t.Error("member refused with everything on")
			}
			if nothing.Allows(RoleMember, permission) {
				t.Error("member allowed with everything off")
			}
			if everything.Allows("", permission) {
				t.Error("no role at all is allowed")
			}
		})
	}
	if nothing.Allows(RoleMember, "fly") {
		t.Error("an unknown permission is allowed")
	}
}

// The defaults keep what members could do before: attach files, nothing else.
func TestNewServerStartsWithTheDefaults(t *testing.T) {
	w := newWorld(t)
	got, err := w.svc.Get(t.Context(), w.workspace, w.member)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Workspace.MemberPermissions != DefaultPermissions() {
		t.Errorf("permissions = %+v, want %+v", got.Workspace.MemberPermissions, DefaultPermissions())
	}
	if want := (Permissions{AttachFiles: true}); DefaultPermissions() != want {
		t.Errorf("defaults = %+v, want %+v", DefaultPermissions(), want)
	}
}

func TestCreateTakesPermissions(t *testing.T) {
	svc := NewService(newFakeStore(), nil, Icons{})
	created, err := svc.Create(t.Context(), NewWorkspace{
		Name:        "Открытая",
		Permissions: PermissionsUpdate{Invite: on(), AttachFiles: off()},
	}, uuid.New())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// What was not sent keeps its default.
	want := Permissions{Invite: true, CreateChannels: false, AttachFiles: false}
	if created.Workspace.MemberPermissions != want {
		t.Errorf("permissions = %+v, want %+v", created.Workspace.MemberPermissions, want)
	}
}

func TestRequirePermission(t *testing.T) {
	for _, permission := range allPermissions {
		t.Run(string(permission), func(t *testing.T) {
			w := newWorld(t)
			// Everything off: only the member is refused.
			if _, err := w.svc.Update(t.Context(), w.workspace, w.owner, WorkspaceUpdate{
				Permissions: PermissionsUpdate{Invite: off(), CreateChannels: off(), AttachFiles: off()},
			}); err != nil {
				t.Fatalf("Update: %v", err)
			}
			for caller, want := range map[uuid.UUID]error{
				w.owner: nil, w.admin: nil, w.member: ErrForbidden, w.outsider: ErrNotMember,
			} {
				if _, err := w.svc.RequirePermission(t.Context(), w.workspace, caller, permission); !errors.Is(err, want) {
					t.Errorf("caller %v: err = %v, want %v", caller, err, want)
				}
			}
		})
	}
}

// Who may change the permissions: the same people who may change the server.
func TestOnlyAdminsChangePermissions(t *testing.T) {
	w := newWorld(t)
	update := WorkspaceUpdate{Permissions: PermissionsUpdate{Invite: on()}}

	if _, err := w.svc.Update(t.Context(), w.workspace, w.member, update); !errors.Is(err, ErrForbidden) {
		t.Errorf("member: err = %v, want ErrForbidden", err)
	}
	if _, err := w.svc.Update(t.Context(), w.workspace, w.outsider, update); !errors.Is(err, ErrNotMember) {
		t.Errorf("outsider: err = %v, want ErrNotMember", err)
	}
	updated, err := w.svc.Update(t.Context(), w.workspace, w.admin, update)
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	// Only the flag sent changed.
	want := Permissions{Invite: true, AttachFiles: true}
	if updated.Workspace.MemberPermissions != want {
		t.Errorf("permissions = %+v, want %+v", updated.Workspace.MemberPermissions, want)
	}
}

// Nothing is cached: switching a flag binds the very next call, which is how an
// already connected client is held to it.
func TestPermissionChangeAppliesImmediately(t *testing.T) {
	w, _, watchers := newProfileWorld(t)

	if _, err := w.svc.RequirePermission(t.Context(), w.workspace, w.member, PermissionAttachFiles); err != nil {
		t.Fatalf("before: %v", err)
	}
	if _, err := w.svc.Update(t.Context(), w.workspace, w.owner, WorkspaceUpdate{
		Permissions: PermissionsUpdate{AttachFiles: off(), CreateChannels: on()},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := w.svc.RequirePermission(t.Context(), w.workspace, w.member, PermissionAttachFiles); !errors.Is(err, ErrForbidden) {
		t.Errorf("attach after switching it off: err = %v, want ErrForbidden", err)
	}
	if _, err := w.svc.RequirePermission(t.Context(), w.workspace, w.member, PermissionCreateChannels); err != nil {
		t.Errorf("create channels after switching it on: %v", err)
	}

	// Connected clients hear about it, with the new flags in the payload.
	published := watchers.updates()
	if len(published) != 1 {
		t.Fatalf("workspace.updated events = %d, want 1", len(published))
	}
	payload, ok := published[0].Payload.(api.Workspace)
	if !ok {
		t.Fatalf("payload = %T, want api.Workspace", published[0].Payload)
	}
	want := api.WorkspacePermissions{CanInvite: false, CanCreateChannels: true, CanAttachFiles: false}
	if payload.MemberPermissions != want {
		t.Errorf("payload permissions = %+v, want %+v", payload.MemberPermissions, want)
	}
}

// The flags of one server say nothing about another, even for the same person.
func TestPermissionsArePerServer(t *testing.T) {
	w := newWorld(t)
	open, err := w.svc.Create(t.Context(), NewWorkspace{
		Name:        "Открытая",
		Permissions: PermissionsUpdate{Invite: on(), CreateChannels: on()},
	}, w.outsider)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The member of the strict server is a member of the open one too.
	if _, err := w.store.AddMember(t.Context(), open.Workspace.ID, w.member, RoleMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	for _, permission := range []Permission{PermissionInvite, PermissionCreateChannels} {
		if _, err := w.svc.RequirePermission(t.Context(), open.Workspace.ID, w.member, permission); err != nil {
			t.Errorf("%s on the open server: %v", permission, err)
		}
		if _, err := w.svc.RequirePermission(t.Context(), w.workspace, w.member, permission); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s on the strict server: err = %v, want ErrForbidden", permission, err)
		}
	}

	// Being owner of the open server grants nothing on the strict one.
	if _, err := w.svc.RequirePermission(t.Context(), w.workspace, w.outsider, PermissionInvite); !errors.Is(err, ErrNotMember) {
		t.Errorf("owner elsewhere: err = %v, want ErrNotMember", err)
	}
	// And an admin of the strict server cannot open it up for the other one.
	if _, err := w.svc.Update(t.Context(), open.Workspace.ID, w.admin, WorkspaceUpdate{
		Permissions: PermissionsUpdate{Invite: off()},
	}); !errors.Is(err, ErrNotMember) {
		t.Errorf("admin of another server: err = %v, want ErrNotMember", err)
	}
}

// A demoted admin loses the always-allowed rights at once.
func TestDemotedAdminFollowsMemberPermissions(t *testing.T) {
	w := newWorld(t)
	if _, err := w.svc.RequirePermission(t.Context(), w.workspace, w.admin, PermissionInvite); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if _, err := w.svc.SetRole(t.Context(), w.workspace, w.owner, w.admin, RoleMember); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	if _, err := w.svc.RequirePermission(t.Context(), w.workspace, w.admin, PermissionInvite); !errors.Is(err, ErrForbidden) {
		t.Errorf("demoted admin: err = %v, want ErrForbidden", err)
	}
}
