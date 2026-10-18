package invites

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

// fakeStore keeps links in memory and enforces what the queries enforce:
// a link that expired, was revoked or is used up is simply not there.
type fakeStore struct {
	invites map[uuid.UUID]*Invite
	now     func() time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{invites: map[uuid.UUID]*Invite{}, now: time.Now}
}

func (s *fakeStore) Create(_ context.Context, workspaceID, _ uuid.UUID, code string, expiresAt *time.Time, maxUses *int) (Invite, error) {
	invite := Invite{
		ID: uuid.New(), WorkspaceID: workspaceID, Code: code,
		ExpiresAt: expiresAt, MaxUses: maxUses, CreatedAt: s.now(),
	}
	s.invites[invite.ID] = &invite
	return invite, nil
}

func (s *fakeStore) live(invite *Invite) bool {
	switch {
	case invite.ExpiresAt != nil && !invite.ExpiresAt.After(s.now()):
		return false
	case invite.MaxUses != nil && invite.Uses >= *invite.MaxUses:
		return false
	}
	return true
}

func (s *fakeStore) Live(_ context.Context, code string) (Invite, error) {
	for _, invite := range s.invites {
		if invite.Code == code && s.live(invite) {
			return *invite, nil
		}
	}
	return Invite{}, ErrNotFound
}

func (s *fakeStore) Use(_ context.Context, id uuid.UUID) (Invite, error) {
	invite, ok := s.invites[id]
	if !ok || !s.live(invite) {
		return Invite{}, ErrNotFound
	}
	invite.Uses++
	return *invite, nil
}

func (s *fakeStore) Revoke(_ context.Context, id uuid.UUID) error {
	invite, ok := s.invites[id]
	if !ok {
		return ErrNotFound
	}
	// Revoking is modelled as "no uses left", like the query's revoked_at.
	zero := 0
	invite.MaxUses = &zero
	return nil
}

func (s *fakeStore) Invite(_ context.Context, id uuid.UUID) (Invite, error) {
	invite, ok := s.invites[id]
	if !ok {
		return Invite{}, ErrNotFound
	}
	return *invite, nil
}

func (s *fakeStore) List(_ context.Context, workspaceID uuid.UUID) ([]Invite, error) {
	var out []Invite
	for _, invite := range s.invites {
		if invite.WorkspaceID == workspaceID {
			out = append(out, *invite)
		}
	}
	return out, nil
}

// fakeMembers answers like internal/servers.
type fakeMembers struct {
	workspace servers.Workspace
	roles     map[uuid.UUID]servers.Role
	banned    map[uuid.UUID]bool
	joins     int
}

func (m *fakeMembers) RequireAtLeast(_ context.Context, workspaceID, userID uuid.UUID, least servers.Role) (servers.Role, error) {
	if workspaceID != m.workspace.ID {
		return "", servers.ErrNotMember
	}
	role, ok := m.roles[userID]
	if !ok {
		return "", servers.ErrNotMember
	}
	rank := map[servers.Role]int{servers.RoleMember: 1, servers.RoleAdmin: 2, servers.RoleOwner: 3}
	if rank[role] < rank[least] {
		return role, servers.ErrForbidden
	}
	return role, nil
}

func (m *fakeMembers) Authorize(_ context.Context, workspaceID, userID uuid.UUID) (servers.Role, error) {
	if workspaceID != m.workspace.ID {
		return "", servers.ErrNotMember
	}
	role, ok := m.roles[userID]
	if !ok {
		return "", servers.ErrNotMember
	}
	return role, nil
}

func (m *fakeMembers) Join(_ context.Context, _, userID uuid.UUID) (servers.Member, error) {
	if m.banned[userID] {
		return servers.Member{}, servers.ErrBanned
	}
	m.joins++
	m.roles[userID] = servers.RoleMember
	return servers.Member{Role: servers.RoleMember}, nil
}

func (m *fakeMembers) Workspace(_ context.Context, workspaceID uuid.UUID) (servers.Workspace, error) {
	if workspaceID != m.workspace.ID {
		return servers.Workspace{}, servers.ErrNotMember
	}
	return m.workspace, nil
}

func (m *fakeMembers) MemberCount(_ context.Context, _ uuid.UUID) (int, error) {
	return len(m.roles), nil
}

// fakeGuests answers like internal/auth.
type fakeGuests struct {
	taken map[string]bool
	made  []auth.User
}

func (g *fakeGuests) RegisterGuest(_ context.Context, nickname string) (auth.User, auth.Credentials, error) {
	if g.taken[strings.ToLower(nickname)] {
		return auth.User{}, auth.Credentials{}, auth.ErrTaken
	}
	g.taken[strings.ToLower(nickname)] = true
	user := auth.User{ID: uuid.New(), Nickname: nickname, IsGuest: true, CreatedAt: time.Now()}
	g.made = append(g.made, user)
	return user, auth.Credentials{Token: "token-" + nickname, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type world struct {
	svc      *Service
	store    *fakeStore
	members  *fakeMembers
	guests   *fakeGuests
	owner    uuid.UUID
	member   uuid.UUID
	stranger uuid.UUID
	code     string
}

func newWorld(t *testing.T) world {
	t.Helper()
	w := world{
		store: newFakeStore(),
		owner: uuid.New(), member: uuid.New(), stranger: uuid.New(),
		guests: &fakeGuests{taken: map[string]bool{}},
	}
	w.members = &fakeMembers{
		workspace: servers.Workspace{ID: uuid.New(), Name: "Команда Orbit", OwnerID: w.owner},
		banned:    map[uuid.UUID]bool{},
	}
	w.members.roles = map[uuid.UUID]servers.Role{w.owner: servers.RoleOwner, w.member: servers.RoleMember}
	w.svc = NewService(w.store, w.members, w.guests)

	invite, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	w.code = invite.Code
	return w
}

func TestCreateNeedsAdminAndGivesAnUnguessableCode(t *testing.T) {
	w := newWorld(t)

	first, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if first.Code == second.Code {
		t.Error("two links got the same code")
	}
	if len(first.Code) < 16 {
		t.Errorf("code = %q, want at least 96 bits of entropy", first.Code)
	}

	if _, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.member, New{}); !errors.Is(err, ErrForbidden) {
		t.Errorf("as a member: err = %v, want ErrForbidden", err)
	}
	if _, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.stranger, New{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("as an outsider: err = %v, want ErrNotFound", err)
	}
}

func TestCreateRejectsAnImpossibleLifetimeOrLimit(t *testing.T) {
	w := newWorld(t)
	tooLong := maxLifetime + time.Hour
	zero := 0
	tooMany := maxUses + 1

	if _, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{ExpiresIn: &tooLong}); !errors.Is(err, ErrInvalidLifetime) {
		t.Errorf("err = %v, want ErrInvalidLifetime", err)
	}
	for _, limit := range []*int{&zero, &tooMany} {
		if _, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{MaxUses: limit}); !errors.Is(err, ErrInvalidUses) {
			t.Errorf("limit %d: err = %v, want ErrInvalidUses", *limit, err)
		}
	}
}

// The page behind a link may show the server before anyone signs in.
func TestPreviewWorksWithoutASession(t *testing.T) {
	w := newWorld(t)

	preview, err := w.svc.Preview(t.Context(), w.code)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if preview.WorkspaceName != "Команда Orbit" || preview.MemberCount != 2 {
		t.Errorf("preview = %+v, want the server name and its two members", preview)
	}

	if _, err := w.svc.Preview(t.Context(), "не-ссылка"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAcceptAddsTheAccountAndSpendsOneUse(t *testing.T) {
	w := newWorld(t)
	newcomer := uuid.New()

	membership, err := w.svc.Accept(t.Context(), w.code, newcomer)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if membership.Role != servers.RoleMember || membership.Workspace.Name != "Команда Orbit" {
		t.Errorf("membership = %+v, want member of the server", membership)
	}

	invite, err := w.store.Live(t.Context(), w.code)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if invite.Uses != 1 {
		t.Errorf("uses = %d, want 1", invite.Uses)
	}
}

// Coming back by a link you already used does not spend another use.
func TestAcceptByAnExistingMemberIsFree(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.Accept(t.Context(), w.code, w.member); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	invite, err := w.store.Live(t.Context(), w.code)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if invite.Uses != 0 {
		t.Errorf("uses = %d, want 0", invite.Uses)
	}
	if w.members.joins != 0 {
		t.Errorf("joins = %d, want 0: the member was already in", w.members.joins)
	}
}

func TestDeadLinksAreRefused(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		w := newWorld(t)
		hour := time.Hour
		invite, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{ExpiresIn: &hour})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		w.store.now = func() time.Time { return time.Now().Add(2 * time.Hour) }

		if _, err := w.svc.Accept(t.Context(), invite.Code, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("used up", func(t *testing.T) {
		w := newWorld(t)
		once := 1
		invite, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{MaxUses: &once})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		if _, err := w.svc.Accept(t.Context(), invite.Code, uuid.New()); err != nil {
			t.Fatalf("the first person could not join: %v", err)
		}
		if _, err := w.svc.Accept(t.Context(), invite.Code, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Errorf("the second person got in: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("revoked", func(t *testing.T) {
		w := newWorld(t)
		invite, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := w.svc.Revoke(t.Context(), invite.ID, w.owner); err != nil {
			t.Fatalf("Revoke: %v", err)
		}

		if _, err := w.svc.Accept(t.Context(), invite.Code, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

// A ban holds even with a working link (FR-02, FR-03).
func TestBannedAccountCannotComeBackByLink(t *testing.T) {
	w := newWorld(t)
	banned := uuid.New()
	w.members.banned[banned] = true

	if _, err := w.svc.Accept(t.Context(), w.code, banned); !errors.Is(err, ErrBanned) {
		t.Errorf("err = %v, want ErrBanned", err)
	}
}

func TestRevokeNeedsAdmin(t *testing.T) {
	w := newWorld(t)
	invite, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := w.svc.Revoke(t.Context(), invite.ID, w.member); !errors.Is(err, ErrForbidden) {
		t.Errorf("as a member: err = %v, want ErrForbidden", err)
	}
	if err := w.svc.Revoke(t.Context(), invite.ID, w.stranger); !errors.Is(err, ErrNotFound) {
		t.Errorf("as an outsider: err = %v, want ErrNotFound", err)
	}
	if err := w.svc.Revoke(t.Context(), uuid.New(), w.owner); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown link: err = %v, want ErrNotFound", err)
	}
}

func TestListInvitesNeedsAdmin(t *testing.T) {
	w := newWorld(t)

	list, err := w.svc.List(t.Context(), w.members.workspace.ID, w.owner)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("invites = %d, want 1", len(list))
	}

	if _, err := w.svc.List(t.Context(), w.members.workspace.ID, w.member); !errors.Is(err, ErrForbidden) {
		t.Errorf("as a member: err = %v, want ErrForbidden", err)
	}
}

// Name, link, room: a guest needs no account, and gets a live session at once.
func TestJoinAsGuest(t *testing.T) {
	w := newWorld(t)

	user, creds, membership, err := w.svc.JoinAsGuest(t.Context(), w.code, "Аня")
	if err != nil {
		t.Fatalf("JoinAsGuest: %v", err)
	}

	if !user.IsGuest || user.Nickname != "Аня" {
		t.Errorf("user = %+v, want a guest named Аня", user)
	}
	if creds.Token == "" {
		t.Error("the guest got no session")
	}
	if membership.Role != servers.RoleMember {
		t.Errorf("role = %q, want member", membership.Role)
	}

	invite, err := w.store.Live(t.Context(), w.code)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if invite.Uses != 1 {
		t.Errorf("uses = %d, want 1", invite.Uses)
	}
}

func TestJoinAsGuestRefusesATakenNameAndADeadLink(t *testing.T) {
	w := newWorld(t)
	if _, _, _, err := w.svc.JoinAsGuest(t.Context(), w.code, "Аня"); err != nil {
		t.Fatalf("JoinAsGuest: %v", err)
	}

	if _, _, _, err := w.svc.JoinAsGuest(t.Context(), w.code, "аня"); !errors.Is(err, ErrNicknameTaken) {
		t.Errorf("err = %v, want ErrNicknameTaken", err)
	}
	if _, _, _, err := w.svc.JoinAsGuest(t.Context(), "не-ссылка", "Боб"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	// The refused guest did not spend a use of the link.
	invite, err := w.store.Live(t.Context(), w.code)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if invite.Uses != 1 {
		t.Errorf("uses = %d, want 1", invite.Uses)
	}
}

// The last use cannot let two people in: the use is spent before the join.
func TestTheLastUseGoesToOnePerson(t *testing.T) {
	w := newWorld(t)
	once := 1
	invite, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{MaxUses: &once})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, _, _, err := w.svc.JoinAsGuest(t.Context(), invite.Code, "Аня"); err != nil {
		t.Fatalf("JoinAsGuest: %v", err)
	}
	if _, _, _, err := w.svc.JoinAsGuest(t.Context(), invite.Code, "Боб"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the second guest got in: err = %v, want ErrNotFound", err)
	}
	if w.members.joins != 1 {
		t.Errorf("joins = %d, want 1", w.members.joins)
	}
}
