package servers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore keeps servers, memberships and bans in memory with the same rules the
// database enforces: one row per (workspace, user), and a ban drops the membership.
type fakeStore struct {
	workspaces map[uuid.UUID]Workspace
	members    map[uuid.UUID]map[uuid.UUID]Member
	bans       map[uuid.UUID]map[uuid.UUID]Ban
	accounts   map[uuid.UUID]Account
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		workspaces: map[uuid.UUID]Workspace{},
		members:    map[uuid.UUID]map[uuid.UUID]Member{},
		bans:       map[uuid.UUID]map[uuid.UUID]Ban{},
		accounts:   map[uuid.UUID]Account{},
	}
}

func (s *fakeStore) account(id uuid.UUID) Account {
	if a, ok := s.accounts[id]; ok {
		return a
	}
	a := Account{ID: id, Nickname: id.String()[:8], CreatedAt: time.Now()}
	s.accounts[id] = a
	return a
}

func (s *fakeStore) CreateWorkspace(_ context.Context, name string, ownerID uuid.UUID) (Workspace, error) {
	w := Workspace{ID: uuid.New(), Name: name, OwnerID: ownerID, CreatedAt: time.Now()}
	s.workspaces[w.ID] = w
	s.members[w.ID] = map[uuid.UUID]Member{
		ownerID: {User: s.account(ownerID), Role: RoleOwner, JoinedAt: time.Now()},
	}
	s.bans[w.ID] = map[uuid.UUID]Ban{}
	return w, nil
}

func (s *fakeStore) Workspace(_ context.Context, id uuid.UUID) (Workspace, error) {
	w, ok := s.workspaces[id]
	if !ok {
		return Workspace{}, ErrNotMember
	}
	return w, nil
}

func (s *fakeStore) RenameWorkspace(_ context.Context, id uuid.UUID, name string) (Workspace, error) {
	w, ok := s.workspaces[id]
	if !ok {
		return Workspace{}, ErrNotMember
	}
	w.Name = name
	s.workspaces[id] = w
	return w, nil
}

func (s *fakeStore) DeleteWorkspace(_ context.Context, id uuid.UUID) error {
	delete(s.workspaces, id)
	delete(s.members, id)
	delete(s.bans, id)
	return nil
}

func (s *fakeStore) ListMemberships(_ context.Context, userID uuid.UUID) ([]Membership, error) {
	var out []Membership
	for id, members := range s.members {
		if member, ok := members[userID]; ok {
			out = append(out, Membership{Workspace: s.workspaces[id], Role: member.Role})
		}
	}
	return out, nil
}

func (s *fakeStore) Role(_ context.Context, workspaceID, userID uuid.UUID) (Role, error) {
	member, ok := s.members[workspaceID][userID]
	if !ok {
		return "", ErrNotMember
	}
	return member.Role, nil
}

func (s *fakeStore) SetRole(_ context.Context, workspaceID, userID uuid.UUID, role Role) (Member, error) {
	member, ok := s.members[workspaceID][userID]
	if !ok {
		return Member{}, ErrNotMember
	}
	member.Role = role
	s.members[workspaceID][userID] = member
	return member, nil
}

func (s *fakeStore) AddMember(_ context.Context, workspaceID, userID uuid.UUID, role Role) (Member, error) {
	if _, ok := s.workspaces[workspaceID]; !ok {
		return Member{}, ErrNotMember
	}
	member := Member{User: s.account(userID), Role: role, JoinedAt: time.Now()}
	s.members[workspaceID][userID] = member
	return member, nil
}

func (s *fakeStore) RemoveMember(_ context.Context, workspaceID, userID uuid.UUID) error {
	if _, ok := s.members[workspaceID][userID]; !ok {
		return ErrNotMember
	}
	delete(s.members[workspaceID], userID)
	return nil
}

func (s *fakeStore) ListMembers(_ context.Context, workspaceID uuid.UUID) ([]Member, error) {
	out := make([]Member, 0, len(s.members[workspaceID]))
	for _, m := range s.members[workspaceID] {
		out = append(out, m)
	}
	return out, nil
}

func (s *fakeStore) CountOwners(_ context.Context, workspaceID uuid.UUID) (int, error) {
	count := 0
	for _, m := range s.members[workspaceID] {
		if m.Role == RoleOwner {
			count++
		}
	}
	return count, nil
}

func (s *fakeStore) Ban(_ context.Context, workspaceID, userID, bannedBy uuid.UUID, reason string) (Ban, error) {
	ban := Ban{User: s.account(userID), Reason: reason, BannedBy: bannedBy, CreatedAt: time.Now()}
	s.bans[workspaceID][userID] = ban
	delete(s.members[workspaceID], userID)
	return ban, nil
}

func (s *fakeStore) Unban(_ context.Context, workspaceID, userID uuid.UUID) error {
	if _, ok := s.bans[workspaceID][userID]; !ok {
		return ErrNotBanned
	}
	delete(s.bans[workspaceID], userID)
	return nil
}

func (s *fakeStore) IsBanned(_ context.Context, workspaceID, userID uuid.UUID) (bool, error) {
	_, ok := s.bans[workspaceID][userID]
	return ok, nil
}

func (s *fakeStore) ListBans(_ context.Context, workspaceID uuid.UUID) ([]Ban, error) {
	out := make([]Ban, 0, len(s.bans[workspaceID]))
	for _, b := range s.bans[workspaceID] {
		out = append(out, b)
	}
	return out, nil
}

// world is one server with a person in every role, plus an outsider.
type world struct {
	svc       *Service
	store     *fakeStore
	workspace uuid.UUID
	owner     uuid.UUID
	admin     uuid.UUID
	member    uuid.UUID
	// member2 is a second plain member: the target when one member acts on another.
	member2  uuid.UUID
	outsider uuid.UUID
}

func newWorld(t *testing.T) world {
	t.Helper()
	store := newFakeStore()
	svc := NewService(store, nil)
	owner := uuid.New()

	membership, err := svc.Create(t.Context(), "Команда Orbit", owner)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	w := world{
		svc: svc, store: store, workspace: membership.Workspace.ID, owner: owner,
		admin: uuid.New(), member: uuid.New(), member2: uuid.New(), outsider: uuid.New(),
	}
	if _, err := store.AddMember(t.Context(), w.workspace, w.admin, RoleAdmin); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	for _, id := range []uuid.UUID{w.member, w.member2} {
		if _, err := store.AddMember(t.Context(), w.workspace, id, RoleMember); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}
	return w
}

func TestCreateMakesTheCallerOwner(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, nil)
	owner := uuid.New()

	membership, err := svc.Create(t.Context(), "  Команда Orbit  ", owner)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if membership.Role != RoleOwner {
		t.Errorf("role = %q, want owner", membership.Role)
	}
	if membership.Workspace.Name != "Команда Orbit" {
		t.Errorf("name = %q, want it trimmed", membership.Workspace.Name)
	}
	if membership.Workspace.OwnerID != owner {
		t.Errorf("ownerId = %v, want %v", membership.Workspace.OwnerID, owner)
	}
}

func TestCreateRejectsEmptyAndTooLongName(t *testing.T) {
	svc := NewService(newFakeStore(), nil)

	for _, name := range []string{"", "   ", string(make([]rune, maxNameLength+1))} {
		if _, err := svc.Create(t.Context(), name, uuid.New()); !errors.Is(err, ErrInvalidName) {
			t.Errorf("name %q: err = %v, want ErrInvalidName", name, err)
		}
	}
}

// The permission matrix: who may do what. Each action is tried as every role and
// as an outsider, so a rule cannot be lost by accident.
func TestPermissionMatrix(t *testing.T) {
	type outcome = error // nil means allowed

	actions := []struct {
		name string
		run  func(w world, caller uuid.UUID) error
	}{
		{"read the server", func(w world, caller uuid.UUID) error {
			_, err := w.svc.Get(context.Background(), w.workspace, caller)
			return err
		}},
		{"list members", func(w world, caller uuid.UUID) error {
			_, err := w.svc.ListMembers(context.Background(), w.workspace, caller)
			return err
		}},
		{"rename the server", func(w world, caller uuid.UUID) error {
			_, err := w.svc.Rename(context.Background(), w.workspace, caller, "Другое имя")
			return err
		}},
		{"list bans", func(w world, caller uuid.UUID) error {
			_, err := w.svc.ListBans(context.Background(), w.workspace, caller)
			return err
		}},
		{"ban a member", func(w world, caller uuid.UUID) error {
			_, err := w.svc.Ban(context.Background(), w.workspace, caller, w.member2, "спам")
			return err
		}},
		{"remove a member", func(w world, caller uuid.UUID) error {
			return w.svc.Remove(context.Background(), w.workspace, caller, w.member2)
		}},
		{"change a role", func(w world, caller uuid.UUID) error {
			_, err := w.svc.SetRole(context.Background(), w.workspace, caller, w.member2, RoleAdmin)
			return err
		}},
		{"delete the server", func(w world, caller uuid.UUID) error {
			return w.svc.Delete(context.Background(), w.workspace, caller)
		}},
	}

	// want[action][role]: nil = allowed.
	want := map[string]map[string]outcome{
		"read the server":   {"owner": nil, "admin": nil, "member": nil, "outsider": ErrNotMember},
		"list members":      {"owner": nil, "admin": nil, "member": nil, "outsider": ErrNotMember},
		"rename the server": {"owner": nil, "admin": nil, "member": ErrForbidden, "outsider": ErrNotMember},
		"list bans":         {"owner": nil, "admin": nil, "member": ErrForbidden, "outsider": ErrNotMember},
		"ban a member":      {"owner": nil, "admin": nil, "member": ErrForbidden, "outsider": ErrNotMember},
		// A member removing another member is refused; leaving is a separate test.
		"remove a member": {"owner": nil, "admin": nil, "member": ErrForbidden, "outsider": ErrNotMember},
		"change a role":   {"owner": nil, "admin": ErrForbidden, "member": ErrForbidden, "outsider": ErrNotMember},
		"delete the server": {
			"owner": nil, "admin": ErrForbidden, "member": ErrForbidden, "outsider": ErrNotMember,
		},
	}

	for _, action := range actions {
		for _, role := range []string{"owner", "admin", "member", "outsider"} {
			t.Run(action.name+" as "+role, func(t *testing.T) {
				w := newWorld(t)
				caller := map[string]uuid.UUID{
					"owner": w.owner, "admin": w.admin, "member": w.member, "outsider": w.outsider,
				}[role]

				err := action.run(w, caller)
				expected := want[action.name][role]

				switch {
				case expected == nil && err != nil:
					t.Errorf("err = %v, want it allowed", err)
				case expected != nil && !errors.Is(err, expected):
					t.Errorf("err = %v, want %v", err, expected)
				}
			})
		}
	}
}

// An admin may moderate members, but not another admin and not the owner.
func TestAdminCannotActOnEqualOrHigherRole(t *testing.T) {
	for _, target := range []string{"another admin", "the owner"} {
		t.Run(target, func(t *testing.T) {
			w := newWorld(t)
			other := w.owner
			if target == "another admin" {
				other = uuid.New()
				if _, err := w.store.AddMember(t.Context(), w.workspace, other, RoleAdmin); err != nil {
					t.Fatalf("AddMember: %v", err)
				}
			}

			if err := w.svc.Remove(t.Context(), w.workspace, w.admin, other); !errors.Is(err, ErrForbidden) {
				t.Errorf("Remove: err = %v, want ErrForbidden", err)
			}
			if _, err := w.svc.Ban(t.Context(), w.workspace, w.admin, other, ""); !errors.Is(err, ErrForbidden) {
				t.Errorf("Ban: err = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestMembersMayLeaveButTheLastOwnerMayNot(t *testing.T) {
	w := newWorld(t)

	if err := w.svc.Remove(t.Context(), w.workspace, w.member, w.member); err != nil {
		t.Errorf("a member could not leave: %v", err)
	}
	if err := w.svc.Remove(t.Context(), w.workspace, w.owner, w.owner); !errors.Is(err, ErrLastOwner) {
		t.Errorf("err = %v, want ErrLastOwner", err)
	}
}

func TestSetRoleHandsOwnershipOverAndKeepsOneOwner(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.SetRole(t.Context(), w.workspace, w.owner, w.admin, RoleOwner); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	newOwner, err := w.svc.Authorize(t.Context(), w.workspace, w.admin)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if newOwner != RoleOwner {
		t.Errorf("new owner role = %q, want owner", newOwner)
	}
	// The previous owner stays as an admin, so the server has exactly one owner.
	previous, err := w.svc.Authorize(t.Context(), w.workspace, w.owner)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if previous != RoleAdmin {
		t.Errorf("previous owner role = %q, want admin", previous)
	}
	owners, err := w.store.CountOwners(t.Context(), w.workspace)
	if err != nil {
		t.Fatalf("CountOwners: %v", err)
	}
	if owners != 1 {
		t.Errorf("owners = %d, want 1", owners)
	}
}

func TestSetRoleRefusesToDemoteTheLastOwner(t *testing.T) {
	w := newWorld(t)

	_, err := w.svc.SetRole(t.Context(), w.workspace, w.owner, w.owner, RoleAdmin)
	if !errors.Is(err, ErrLastOwner) {
		t.Errorf("err = %v, want ErrLastOwner", err)
	}
}

func TestSetRoleRejectsUnknownRoleAndOutsiderTarget(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.SetRole(t.Context(), w.workspace, w.owner, w.member, Role("god")); !errors.Is(err, ErrInvalidRole) {
		t.Errorf("err = %v, want ErrInvalidRole", err)
	}
	if _, err := w.svc.SetRole(t.Context(), w.workspace, w.owner, w.outsider, RoleAdmin); !errors.Is(err, ErrNotMember) {
		t.Errorf("err = %v, want ErrNotMember", err)
	}
}

// A ban takes the person out of the server at once and keeps a link from letting
// them back in (FR-02, FR-03).
func TestBanRemovesMemberAndBlocksRejoining(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.Ban(t.Context(), w.workspace, w.admin, w.member, "спам"); err != nil {
		t.Fatalf("Ban: %v", err)
	}

	if _, err := w.svc.Authorize(t.Context(), w.workspace, w.member); !errors.Is(err, ErrNotMember) {
		t.Errorf("the banned member still has a role: %v", err)
	}
	if _, err := w.svc.Join(t.Context(), w.workspace, w.member); !errors.Is(err, ErrBanned) {
		t.Errorf("Join: err = %v, want ErrBanned", err)
	}

	if err := w.svc.Unban(t.Context(), w.workspace, w.admin, w.member); err != nil {
		t.Fatalf("Unban: %v", err)
	}
	if _, err := w.svc.Join(t.Context(), w.workspace, w.member); err != nil {
		t.Errorf("Join after the ban was lifted: %v", err)
	}
}

func TestBanYourselfIsRefused(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.Ban(t.Context(), w.workspace, w.admin, w.admin, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

// Someone who already left can still be banned, or a link would bring them back.
func TestBanWorksForSomeoneWhoLeft(t *testing.T) {
	w := newWorld(t)
	if err := w.svc.Remove(t.Context(), w.workspace, w.member, w.member); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := w.svc.Ban(t.Context(), w.workspace, w.owner, w.member, "ушёл сам"); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	if _, err := w.svc.Join(t.Context(), w.workspace, w.member); !errors.Is(err, ErrBanned) {
		t.Errorf("Join: err = %v, want ErrBanned", err)
	}
}

// Data of one server never shows up in another: a role is read per server.
func TestWorkspacesAreIsolated(t *testing.T) {
	w := newWorld(t)
	other, err := w.svc.Create(t.Context(), "Другая команда", w.outsider)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := w.svc.Get(t.Context(), other.Workspace.ID, w.owner); !errors.Is(err, ErrNotMember) {
		t.Errorf("the owner of one server reads another: %v", err)
	}
	if _, err := w.svc.ListMembers(t.Context(), other.Workspace.ID, w.admin); !errors.Is(err, ErrNotMember) {
		t.Errorf("an admin of one server lists members of another: %v", err)
	}
	// Being an owner elsewhere grants nothing here.
	if _, err := w.svc.Get(t.Context(), w.workspace, w.outsider); !errors.Is(err, ErrNotMember) {
		t.Errorf("an owner of another server reads this one: %v", err)
	}

	mine, err := w.svc.List(t.Context(), w.owner)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(mine) != 1 || mine[0].Workspace.ID != w.workspace {
		t.Errorf("memberships = %+v, want only this server", mine)
	}
}

// Rights are read for every call, so a role change applies to the next action of
// an already connected client — nothing is cached between requests.
func TestRoleChangeAppliesImmediately(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.Rename(t.Context(), w.workspace, w.admin, "Переименовано админом"); err != nil {
		t.Fatalf("Rename as admin: %v", err)
	}

	if _, err := w.svc.SetRole(t.Context(), w.workspace, w.owner, w.admin, RoleMember); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	if _, err := w.svc.Rename(t.Context(), w.workspace, w.admin, "Снова"); !errors.Is(err, ErrForbidden) {
		t.Errorf("the demoted admin can still rename: %v", err)
	}
	if _, err := w.svc.Authorize(t.Context(), w.workspace, w.admin); err != nil {
		t.Errorf("Authorize: %v", err)
	}
}

func TestRequireAtLeast(t *testing.T) {
	w := newWorld(t)

	for _, tc := range []struct {
		name   string
		caller uuid.UUID
		least  Role
		want   error
	}{
		{"member passes member", w.member, RoleMember, nil},
		{"member fails admin", w.member, RoleAdmin, ErrForbidden},
		{"admin passes admin", w.admin, RoleAdmin, nil},
		{"admin fails owner", w.admin, RoleOwner, ErrForbidden},
		{"owner passes owner", w.owner, RoleOwner, nil},
		{"outsider fails member", w.outsider, RoleMember, ErrNotMember},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.svc.RequireAtLeast(t.Context(), w.workspace, tc.caller, tc.least)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
