package servers

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/perms"
)

var (
	// ErrRoleNotFound means there is no such role on a server the caller is on.
	ErrRoleNotFound = errors.New("servers: no such role")
	// ErrInvalidRoleName means the name is empty or longer than 32 characters.
	ErrInvalidRoleName = errors.New("servers: a role name must be 1 to 32 characters")
	// ErrInvalidColor means the colour is not one of the accent tokens.
	ErrInvalidColor = errors.New("servers: unknown role colour")
	// ErrDefaultRole means the change does not apply to @everyone: it keeps its
	// name, colour and place, and it is never granted, taken or deleted.
	ErrDefaultRole = errors.New("servers: @everyone cannot change that way")
	// ErrIncompleteRoleOrder means the order does not name every role once.
	ErrIncompleteRoleOrder = errors.New("servers: the order must list every role but @everyone once")
)

const maxRoleNameLength = 32

// accentColors are the tokens a role colour may take, the same as a profile's.
var accentColors = []string{"slate", "red", "amber", "green", "teal", "sky", "indigo", "violet", "pink"}

// ServerRole is one role of a server as the API shows it.
type ServerRole struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Name        string
	// Color is an accent token; empty is no colour.
	Color       string
	Hoist       bool
	Position    int
	Permissions perms.Set
	Default     bool
}

func (r ServerRole) rule() perms.Role {
	return perms.Role{ID: r.ID, Position: r.Position, Permissions: r.Permissions, Default: r.Default}
}

// NewRole is what creating a role takes.
type NewRole struct {
	Name        string
	Color       string
	Hoist       bool
	Permissions perms.Set
}

// RoleUpdate carries only what the caller asked to change.
type RoleUpdate struct {
	Name        *string
	Color       *string
	ClearColor  bool
	Hoist       *bool
	Permissions *perms.Set
}

func (u RoleUpdate) touchesLooks() bool {
	return u.Name != nil || u.Color != nil || u.ClearColor || u.Hoist != nil
}

// ListRoles is every role of the server, highest first. Any member may read
// them: colours and groups are drawn for everyone.
func (s *Service) ListRoles(ctx context.Context, workspaceID, callerID uuid.UUID) ([]ServerRole, error) {
	if _, err := s.store.Access(ctx, workspaceID, callerID); err != nil {
		return nil, err
	}
	return s.store.Roles(ctx, workspaceID)
}

// CreateRole makes a role just above @everyone. It takes manageRoles, and the
// role grants only what the caller has.
func (s *Service) CreateRole(ctx context.Context, workspaceID, callerID uuid.UUID, n NewRole) (ServerRole, error) {
	access, err := s.Require(ctx, workspaceID, callerID, perms.ManageRoles)
	if err != nil {
		return ServerRole{}, err
	}
	name, err := cleanRoleName(n.Name)
	if err != nil {
		return ServerRole{}, err
	}
	if err := checkColor(n.Color); err != nil {
		return ServerRole{}, err
	}
	if !n.Permissions.Valid() || !access.Server.MayGrant(access.Member, n.Permissions) {
		return ServerRole{}, ErrForbidden
	}
	role, err := s.store.CreateRole(ctx, workspaceID, NewRole{Name: name, Color: n.Color, Hoist: n.Hoist, Permissions: n.Permissions})
	if err != nil {
		return ServerRole{}, err
	}
	s.rolesChanged(ctx, workspaceID)
	return role, nil
}

// UpdateRole changes a role below the caller's highest. Permissions it adds
// must be the caller's own; @everyone changes its permissions only.
func (s *Service) UpdateRole(ctx context.Context, roleID, callerID uuid.UUID, u RoleUpdate) (ServerRole, error) {
	role, access, err := s.managedRole(ctx, roleID, callerID)
	if err != nil {
		return ServerRole{}, err
	}
	if role.Default && u.touchesLooks() {
		return ServerRole{}, ErrDefaultRole
	}
	if u.Name != nil {
		name, err := cleanRoleName(*u.Name)
		if err != nil {
			return ServerRole{}, err
		}
		u.Name = &name
	}
	if u.Color != nil {
		if err := checkColor(*u.Color); err != nil {
			return ServerRole{}, err
		}
	}
	if u.Permissions != nil {
		added := *u.Permissions &^ role.Permissions
		if !u.Permissions.Valid() || !access.Server.MayGrant(access.Member, added) {
			return ServerRole{}, ErrForbidden
		}
	}
	updated, err := s.store.UpdateRole(ctx, roleID, u)
	if err != nil {
		return ServerRole{}, err
	}
	s.rolesChanged(ctx, role.WorkspaceID)
	return updated, nil
}

// DeleteRole removes a role below the caller's highest from everyone who held
// it. @everyone stays.
func (s *Service) DeleteRole(ctx context.Context, roleID, callerID uuid.UUID) error {
	role, _, err := s.managedRole(ctx, roleID, callerID)
	if err != nil {
		return err
	}
	if role.Default {
		return ErrDefaultRole
	}
	if err := s.store.DeleteRole(ctx, role.WorkspaceID, roleID); err != nil {
		return err
	}
	s.rolesChanged(ctx, role.WorkspaceID)
	return nil
}

// ReorderRoles sets the order of every role but @everyone, highest first. A
// role the caller may not manage keeps its place, and no role they manage may
// end up at or above their own highest.
func (s *Service) ReorderRoles(ctx context.Context, workspaceID, callerID uuid.UUID, ids []uuid.UUID) ([]ServerRole, error) {
	access, err := s.Require(ctx, workspaceID, callerID, perms.ManageRoles)
	if err != nil {
		return nil, err
	}
	current, err := s.store.Roles(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	byID := map[uuid.UUID]ServerRole{}
	for _, role := range current {
		if !role.Default {
			byID[role.ID] = role
		}
	}
	if len(ids) != len(byID) {
		return nil, ErrIncompleteRoleOrder
	}
	seen := map[uuid.UUID]bool{}
	top := access.Server.Top(access.Member)
	for i, id := range ids {
		role, ok := byID[id]
		if !ok || seen[id] {
			return nil, ErrIncompleteRoleOrder
		}
		seen[id] = true
		position := len(ids) - i
		if position == role.Position {
			continue
		}
		if access.Member.Owner {
			continue
		}
		if !access.Server.MayManageRole(access.Member, role.rule()) || position >= top {
			return nil, ErrForbidden
		}
	}
	if err := s.store.SetRoleOrder(ctx, workspaceID, ids); err != nil {
		return nil, err
	}
	s.rolesChanged(ctx, workspaceID)
	return s.store.Roles(ctx, workspaceID)
}

// GrantRole gives a member a role below the caller's highest; the member must
// rank below the caller, or be the caller.
func (s *Service) GrantRole(ctx context.Context, workspaceID, callerID, targetID, roleID uuid.UUID) (Member, error) {
	if err := s.checkGrant(ctx, workspaceID, callerID, targetID, roleID); err != nil {
		return Member{}, err
	}
	if err := s.store.GrantRole(ctx, workspaceID, targetID, roleID); err != nil {
		return Member{}, err
	}
	return s.afterGrant(ctx, workspaceID, targetID)
}

// RevokeRole takes a role from a member; the same rules as giving it.
func (s *Service) RevokeRole(ctx context.Context, workspaceID, callerID, targetID, roleID uuid.UUID) (Member, error) {
	if err := s.checkGrant(ctx, workspaceID, callerID, targetID, roleID); err != nil {
		return Member{}, err
	}
	if err := s.store.RevokeRole(ctx, workspaceID, targetID, roleID); err != nil {
		return Member{}, err
	}
	return s.afterGrant(ctx, workspaceID, targetID)
}

func (s *Service) checkGrant(ctx context.Context, workspaceID, callerID, targetID, roleID uuid.UUID) error {
	role, access, err := s.managedRole(ctx, roleID, callerID)
	if err != nil {
		return err
	}
	if role.WorkspaceID != workspaceID {
		return ErrRoleNotFound
	}
	if role.Default {
		return ErrDefaultRole
	}
	target, err := s.store.Access(ctx, workspaceID, targetID)
	if err != nil {
		return err
	}
	if targetID != callerID && !access.Outranks(target.Member) {
		return ErrForbidden
	}
	return nil
}

func (s *Service) afterGrant(ctx context.Context, workspaceID, targetID uuid.UUID) (Member, error) {
	member, err := s.memberOf(ctx, workspaceID, targetID)
	if err != nil {
		return Member{}, err
	}
	s.announce(ctx, workspaceID, targetID, member.Role, true)
	return member, nil
}

// managedRole reads a role and the caller's standing on its server, and checks
// that the caller may manage it: manageRoles and a rank above it.
func (s *Service) managedRole(ctx context.Context, roleID, callerID uuid.UUID) (ServerRole, Access, error) {
	role, err := s.store.RoleByID(ctx, roleID)
	if err != nil {
		return ServerRole{}, Access{}, err
	}
	access, err := s.store.Access(ctx, role.WorkspaceID, callerID)
	if err != nil {
		if errors.Is(err, ErrNotMember) {
			// Somebody else's server: the role does not exist for the caller.
			return ServerRole{}, Access{}, ErrRoleNotFound
		}
		return ServerRole{}, Access{}, err
	}
	if !access.Server.MayManageRole(access.Member, role.rule()) {
		return ServerRole{}, Access{}, ErrForbidden
	}
	return role, access, nil
}

// rolesChanged tells every member that the roles changed: colours, groups and
// what each of them may do.
func (s *Service) rolesChanged(ctx context.Context, workspaceID uuid.UUID) {
	s.Announce(ctx, events.Event{Kind: events.KindRolesChanged, WorkspaceID: workspaceID})
}

func cleanRoleName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > maxRoleNameLength {
		return "", ErrInvalidRoleName
	}
	return name, nil
}

func checkColor(color string) error {
	if color != "" && !slices.Contains(accentColors, color) {
		return ErrInvalidColor
	}
	return nil
}
