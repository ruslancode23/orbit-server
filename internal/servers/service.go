// Package servers owns servers (a workspace), their members and roles (FR-02).
// Every action is checked against the caller's current role, read for that request:
// nothing is cached, so a role change or a ban applies to the next request and to the
// next event subscription of an already connected client.
package servers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Role is what someone may do on one server. Ordered: owner > admin > member.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

func (r Role) valid() bool {
	return r == RoleOwner || r == RoleAdmin || r == RoleMember
}

// rank orders the roles so that "may act on" is one comparison.
func (r Role) rank() int {
	switch r {
	case RoleOwner:
		return 3
	case RoleAdmin:
		return 2
	case RoleMember:
		return 1
	default:
		return 0
	}
}

var (
	// ErrNotMember means the caller is not on this server, or it does not exist.
	// The two are the same answer on purpose: otherwise the API tells outsiders
	// which servers exist.
	ErrNotMember = errors.New("servers: not a member")
	// ErrForbidden means the caller is a member but their role is not enough.
	ErrForbidden = errors.New("servers: role is not enough")
	// ErrBanned means the account is banned from this server.
	ErrBanned = errors.New("servers: banned from this server")
	// ErrLastOwner means the change would leave the server without an owner.
	ErrLastOwner = errors.New("servers: a server keeps an owner")
	// ErrInvalidRole means the role is not one of owner, admin, member.
	ErrInvalidRole = errors.New("servers: unknown role")
	// ErrNotBanned means there was no ban to lift.
	ErrNotBanned = errors.New("servers: not banned")
	// ErrInvalidName means the name is empty or too long.
	ErrInvalidName = errors.New("servers: name must be 1 to 64 characters")
)

const maxNameLength = 64

type Workspace struct {
	ID        uuid.UUID
	Name      string
	OwnerID   uuid.UUID
	CreatedAt time.Time
}

// Membership is a server plus the role of the account that asked for it.
type Membership struct {
	Workspace Workspace
	Role      Role
}

// Account is the part of a user this package shows; the auth module owns the rest.
type Account struct {
	ID        uuid.UUID
	Nickname  string
	Email     string
	IsGuest   bool
	CreatedAt time.Time
}

type Member struct {
	User     Account
	Role     Role
	JoinedAt time.Time
}

type Ban struct {
	User      Account
	Reason    string
	BannedBy  uuid.UUID // uuid.Nil when the account that banned is gone
	CreatedAt time.Time
}

// Store is the persistence this package needs.
type Store interface {
	// CreateWorkspace creates the server and its owner membership in one transaction.
	CreateWorkspace(ctx context.Context, name string, ownerID uuid.UUID) (Workspace, error)
	Workspace(ctx context.Context, id uuid.UUID) (Workspace, error)
	RenameWorkspace(ctx context.Context, id uuid.UUID, name string) (Workspace, error)
	DeleteWorkspace(ctx context.Context, id uuid.UUID) error
	ListMemberships(ctx context.Context, userID uuid.UUID) ([]Membership, error)
	// Role returns ErrNotMember when the account is not on the server.
	Role(ctx context.Context, workspaceID, userID uuid.UUID) (Role, error)
	SetRole(ctx context.Context, workspaceID, userID uuid.UUID, role Role) (Member, error)
	AddMember(ctx context.Context, workspaceID, userID uuid.UUID, role Role) (Member, error)
	RemoveMember(ctx context.Context, workspaceID, userID uuid.UUID) error
	ListMembers(ctx context.Context, workspaceID uuid.UUID) ([]Member, error)
	CountOwners(ctx context.Context, workspaceID uuid.UUID) (int, error)
	// Ban bans the account and removes its membership in one transaction.
	Ban(ctx context.Context, workspaceID, userID, bannedBy uuid.UUID, reason string) (Ban, error)
	Unban(ctx context.Context, workspaceID, userID uuid.UUID) error
	IsBanned(ctx context.Context, workspaceID, userID uuid.UUID) (bool, error)
	ListBans(ctx context.Context, workspaceID uuid.UUID) ([]Ban, error)
}

// Service holds the rules. It knows nothing about HTTP.
type Service struct {
	store Store
}

func NewService(store Store) *Service { return &Service{store: store} }

// Authorize reports the caller's role on a server, or ErrNotMember. Other modules
// (channels, messages, the event gateway) call it before they act, so the rule
// "rights are checked on the server" has one implementation.
func (s *Service) Authorize(ctx context.Context, workspaceID, userID uuid.UUID) (Role, error) {
	return s.store.Role(ctx, workspaceID, userID)
}

// RequireAtLeast reports whether the caller's role on the server reaches `least`.
func (s *Service) RequireAtLeast(ctx context.Context, workspaceID, userID uuid.UUID, least Role) (Role, error) {
	role, err := s.store.Role(ctx, workspaceID, userID)
	if err != nil {
		return "", err
	}
	if role.rank() < least.rank() {
		return role, ErrForbidden
	}
	return role, nil
}

func (s *Service) Create(ctx context.Context, name string, ownerID uuid.UUID) (Membership, error) {
	name, err := cleanName(name)
	if err != nil {
		return Membership{}, err
	}
	workspace, err := s.store.CreateWorkspace(ctx, name, ownerID)
	if err != nil {
		return Membership{}, err
	}
	return Membership{Workspace: workspace, Role: RoleOwner}, nil
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	return s.store.ListMemberships(ctx, userID)
}

func (s *Service) Get(ctx context.Context, workspaceID, callerID uuid.UUID) (Membership, error) {
	role, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleMember)
	if err != nil {
		return Membership{}, err
	}
	workspace, err := s.store.Workspace(ctx, workspaceID)
	if err != nil {
		return Membership{}, err
	}
	return Membership{Workspace: workspace, Role: role}, nil
}

func (s *Service) Rename(ctx context.Context, workspaceID, callerID uuid.UUID, name string) (Membership, error) {
	role, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleAdmin)
	if err != nil {
		return Membership{}, err
	}
	name, err = cleanName(name)
	if err != nil {
		return Membership{}, err
	}
	workspace, err := s.store.RenameWorkspace(ctx, workspaceID, name)
	if err != nil {
		return Membership{}, err
	}
	return Membership{Workspace: workspace, Role: role}, nil
}

func (s *Service) Delete(ctx context.Context, workspaceID, callerID uuid.UUID) error {
	if _, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleOwner); err != nil {
		return err
	}
	return s.store.DeleteWorkspace(ctx, workspaceID)
}

func (s *Service) ListMembers(ctx context.Context, workspaceID, callerID uuid.UUID) ([]Member, error) {
	if _, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleMember); err != nil {
		return nil, err
	}
	return s.store.ListMembers(ctx, workspaceID)
}

// Join adds an account as a member. Used by invites (task 2.7); a banned
// account cannot come back through a link.
func (s *Service) Join(ctx context.Context, workspaceID, userID uuid.UUID) (Member, error) {
	banned, err := s.store.IsBanned(ctx, workspaceID, userID)
	if err != nil {
		return Member{}, err
	}
	if banned {
		return Member{}, ErrBanned
	}
	return s.store.AddMember(ctx, workspaceID, userID, RoleMember)
}

// SetRole moves a member between roles. Only the owner may, and the server keeps
// an owner: handing the role over demotes the old owner to admin in the same breath.
func (s *Service) SetRole(ctx context.Context, workspaceID, callerID, targetID uuid.UUID, role Role) (Member, error) {
	if !role.valid() {
		return Member{}, ErrInvalidRole
	}
	if _, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleOwner); err != nil {
		return Member{}, err
	}
	target, err := s.store.Role(ctx, workspaceID, targetID)
	if err != nil {
		return Member{}, err
	}
	if target == role {
		return s.store.SetRole(ctx, workspaceID, targetID, role)
	}

	// Taking the owner role away from the last owner would leave the server without one.
	if target == RoleOwner && role != RoleOwner {
		owners, err := s.store.CountOwners(ctx, workspaceID)
		if err != nil {
			return Member{}, err
		}
		if owners <= 1 {
			return Member{}, ErrLastOwner
		}
	}

	member, err := s.store.SetRole(ctx, workspaceID, targetID, role)
	if err != nil {
		return Member{}, err
	}
	// Handing over ownership: the previous owner stays as an admin, so the server
	// never has two owners and the caller does not lock themselves out of the room.
	if role == RoleOwner && callerID != targetID {
		if _, err := s.store.SetRole(ctx, workspaceID, callerID, RoleAdmin); err != nil {
			return Member{}, err
		}
	}
	return member, nil
}

// Remove kicks a member. An admin cannot kick another admin or the owner, and
// anyone may leave by removing themselves — except the last owner.
func (s *Service) Remove(ctx context.Context, workspaceID, callerID, targetID uuid.UUID) error {
	caller, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleMember)
	if err != nil {
		return err
	}
	target, err := s.store.Role(ctx, workspaceID, targetID)
	if err != nil {
		return err
	}

	if callerID == targetID {
		if target == RoleOwner {
			owners, err := s.store.CountOwners(ctx, workspaceID)
			if err != nil {
				return err
			}
			if owners <= 1 {
				return ErrLastOwner
			}
		}
		return s.store.RemoveMember(ctx, workspaceID, targetID)
	}

	if err := canActOn(caller, target); err != nil {
		return err
	}
	return s.store.RemoveMember(ctx, workspaceID, targetID)
}

// Ban removes someone and keeps them out. Admin or owner, and never against an
// equal or higher role.
func (s *Service) Ban(ctx context.Context, workspaceID, callerID, targetID uuid.UUID, reason string) (Ban, error) {
	caller, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleAdmin)
	if err != nil {
		return Ban{}, err
	}
	if callerID == targetID {
		return Ban{}, ErrForbidden
	}

	// A target who already left is banned anyway, so a link cannot bring them back.
	target, err := s.store.Role(ctx, workspaceID, targetID)
	switch {
	case err == nil:
		if err := canActOn(caller, target); err != nil {
			return Ban{}, err
		}
	case !errors.Is(err, ErrNotMember):
		return Ban{}, err
	}

	return s.store.Ban(ctx, workspaceID, targetID, callerID, strings.TrimSpace(reason))
}

func (s *Service) Unban(ctx context.Context, workspaceID, callerID, targetID uuid.UUID) error {
	if _, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleAdmin); err != nil {
		return err
	}
	return s.store.Unban(ctx, workspaceID, targetID)
}

func (s *Service) ListBans(ctx context.Context, workspaceID, callerID uuid.UUID) ([]Ban, error) {
	if _, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleAdmin); err != nil {
		return nil, err
	}
	return s.store.ListBans(ctx, workspaceID)
}

// canActOn allows moderation only downwards: an admin may act on members, the
// owner on everyone below.
func canActOn(caller, target Role) error {
	if caller.rank() <= target.rank() {
		return ErrForbidden
	}
	return nil
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > maxNameLength {
		return "", ErrInvalidName
	}
	return name, nil
}
