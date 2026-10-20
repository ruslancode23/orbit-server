// Package invites owns the link that brings someone into a server and the
// guest entrance behind it (FR-03): open the link, give a name, be in the room.
package invites

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

var (
	// ErrNotFound covers an unknown, expired, revoked or used-up link, and a
	// server the caller may not see: one answer, so a link cannot be probed.
	ErrNotFound = errors.New("invites: not found")
	// ErrForbidden means the caller's role is not enough to manage links.
	ErrForbidden = errors.New("invites: role is not enough")
	// ErrBanned means this account is banned from the server behind the link.
	ErrBanned = errors.New("invites: banned from this server")
	// ErrNicknameTaken means the guest name is already in use.
	ErrNicknameTaken = errors.New("invites: the nickname is taken")
	// ErrInvalidLifetime means the lifetime or the use limit is out of range.
	ErrInvalidLifetime = errors.New("invites: lifetime must be 1 to 8760 hours")
	// ErrInvalidUses means the use limit is out of range.
	ErrInvalidUses = errors.New("invites: the use limit must be 1 to 1000")
)

const (
	// codeBytes is the entropy of an invite code: guessing one is hopeless,
	// and it still fits in a link people paste into a chat.
	codeBytes   = 12
	maxLifetime = 8760 * time.Hour // a year
	maxUses     = 1000
)

type Invite struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Code        string
	ExpiresAt   *time.Time
	MaxUses     *int
	Uses        int
	CreatedAt   time.Time
}

// Preview is what the page behind a link may show before anyone signs in.
type Preview struct {
	WorkspaceID   uuid.UUID
	WorkspaceName string
	MemberCount   int
}

// New is a link to create.
type New struct {
	ExpiresIn *time.Duration
	MaxUses   *int
}

type Store interface {
	Create(ctx context.Context, workspaceID, createdBy uuid.UUID, code string, expiresAt *time.Time, maxUses *int) (Invite, error)
	// Live returns a link that still works, or ErrNotFound.
	Live(ctx context.Context, code string) (Invite, error)
	// Use counts one more pass through the link, refusing it if it just died.
	Use(ctx context.Context, id uuid.UUID) (Invite, error)
	Revoke(ctx context.Context, id uuid.UUID) error
	Invite(ctx context.Context, id uuid.UUID) (Invite, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]Invite, error)
}

// Members is what this package needs from internal/servers.
type Members interface {
	RequireAtLeast(ctx context.Context, workspaceID, userID uuid.UUID, least servers.Role) (servers.Role, error)
	// RequirePermission is the role when the server lets it take this action.
	RequirePermission(ctx context.Context, workspaceID, userID uuid.UUID, permission servers.Permission) (servers.Role, error)
	Join(ctx context.Context, workspaceID, userID uuid.UUID) (servers.Member, error)
	Workspace(ctx context.Context, workspaceID uuid.UUID) (servers.Workspace, error)
	MemberCount(ctx context.Context, workspaceID uuid.UUID) (int, error)
	// Authorize reports the role of an account on a server, or servers.ErrNotMember.
	Authorize(ctx context.Context, workspaceID, userID uuid.UUID) (servers.Role, error)
}

// Guests is what this package needs from internal/auth to let someone in by
// name alone.
type Guests interface {
	RegisterGuest(ctx context.Context, nickname string) (auth.User, auth.Credentials, error)
}

type Service struct {
	store   Store
	members Members
	guests  Guests
}

func NewService(store Store, members Members, guests Guests) *Service {
	return &Service{store: store, members: members, guests: guests}
}

// Create issues a link. Admins and the owner may, and members too when the
// server lets them invite; listing and revoking links stay with admins.
func (s *Service) Create(ctx context.Context, workspaceID, callerID uuid.UUID, n New) (Invite, error) {
	_, err := s.members.RequirePermission(ctx, workspaceID, callerID, servers.PermissionInvite)
	if err := mapRoleError(err); err != nil {
		return Invite{}, err
	}

	var expiresAt *time.Time
	if n.ExpiresIn != nil {
		if *n.ExpiresIn <= 0 || *n.ExpiresIn > maxLifetime {
			return Invite{}, ErrInvalidLifetime
		}
		moment := time.Now().Add(*n.ExpiresIn)
		expiresAt = &moment
	}
	if n.MaxUses != nil && (*n.MaxUses <= 0 || *n.MaxUses > maxUses) {
		return Invite{}, ErrInvalidUses
	}

	code, err := newCode()
	if err != nil {
		return Invite{}, err
	}
	return s.store.Create(ctx, workspaceID, callerID, code, expiresAt, n.MaxUses)
}

func (s *Service) List(ctx context.Context, workspaceID, callerID uuid.UUID) ([]Invite, error) {
	if err := s.requireAdmin(ctx, workspaceID, callerID); err != nil {
		return nil, err
	}
	return s.store.List(ctx, workspaceID)
}

func (s *Service) Revoke(ctx context.Context, inviteID, callerID uuid.UUID) error {
	invite, err := s.store.Invite(ctx, inviteID)
	if err != nil {
		return err
	}
	if err := s.requireAdmin(ctx, invite.WorkspaceID, callerID); err != nil {
		return err
	}
	return s.store.Revoke(ctx, inviteID)
}

// Preview tells whoever holds the link what it leads to. No session: the page
// has to show the server's name before asking for one.
func (s *Service) Preview(ctx context.Context, code string) (Preview, error) {
	invite, err := s.store.Live(ctx, code)
	if err != nil {
		return Preview{}, err
	}
	workspace, err := s.members.Workspace(ctx, invite.WorkspaceID)
	if err != nil {
		return Preview{}, err
	}
	count, err := s.members.MemberCount(ctx, invite.WorkspaceID)
	if err != nil {
		return Preview{}, err
	}
	return Preview{
		WorkspaceID:   workspace.ID,
		WorkspaceName: workspace.Name,
		MemberCount:   count,
	}, nil
}

// Accept puts an existing account into the server behind the link.
func (s *Service) Accept(ctx context.Context, code string, userID uuid.UUID) (servers.Membership, error) {
	invite, err := s.store.Live(ctx, code)
	if err != nil {
		return servers.Membership{}, err
	}

	// Already a member: the link is simply a way back in, and it is not spent.
	if role, err := s.members.Authorize(ctx, invite.WorkspaceID, userID); err == nil {
		workspace, err := s.members.Workspace(ctx, invite.WorkspaceID)
		if err != nil {
			return servers.Membership{}, err
		}
		return servers.Membership{Workspace: workspace, Role: role}, nil
	} else if !errors.Is(err, servers.ErrNotMember) {
		return servers.Membership{}, err
	}

	return s.join(ctx, invite, userID)
}

// JoinAsGuest creates an account without a password, starts its session and
// puts it into the server: one step from the link to the room.
func (s *Service) JoinAsGuest(ctx context.Context, code, nickname string) (auth.User, auth.Credentials, servers.Membership, error) {
	invite, err := s.store.Live(ctx, code)
	if err != nil {
		return auth.User{}, auth.Credentials{}, servers.Membership{}, err
	}

	user, creds, err := s.guests.RegisterGuest(ctx, nickname)
	if err != nil {
		if errors.Is(err, auth.ErrTaken) || errors.Is(err, auth.ErrNicknameReserved) {
			return auth.User{}, auth.Credentials{}, servers.Membership{}, ErrNicknameTaken
		}
		return auth.User{}, auth.Credentials{}, servers.Membership{}, err
	}

	membership, err := s.join(ctx, invite, user.ID)
	if err != nil {
		return auth.User{}, auth.Credentials{}, servers.Membership{}, err
	}
	return user, creds, membership, nil
}

// join spends one use of the link and adds the account to the server.
func (s *Service) join(ctx context.Context, invite Invite, userID uuid.UUID) (servers.Membership, error) {
	// Spend the use first: two people racing through the last use of a link
	// must not both get in.
	if _, err := s.store.Use(ctx, invite.ID); err != nil {
		return servers.Membership{}, err
	}

	member, err := s.members.Join(ctx, invite.WorkspaceID, userID)
	if err != nil {
		if errors.Is(err, servers.ErrBanned) {
			return servers.Membership{}, ErrBanned
		}
		return servers.Membership{}, err
	}

	workspace, err := s.members.Workspace(ctx, invite.WorkspaceID)
	if err != nil {
		return servers.Membership{}, err
	}
	return servers.Membership{Workspace: workspace, Role: member.Role}, nil
}

func (s *Service) requireAdmin(ctx context.Context, workspaceID, callerID uuid.UUID) error {
	_, err := s.members.RequireAtLeast(ctx, workspaceID, callerID, servers.RoleAdmin)
	return mapRoleError(err)
}

func mapRoleError(err error) error {
	switch {
	case errors.Is(err, servers.ErrNotMember):
		return ErrNotFound
	case errors.Is(err, servers.ErrForbidden):
		return ErrForbidden
	}
	return err
}

// newCode is 96 random bits in a URL-safe form: short enough to paste, far too
// long to guess.
func newCode() (string, error) {
	raw := make([]byte, codeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read code: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
