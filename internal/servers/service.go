// Package servers owns servers (a workspace), their members and roles (FR-02).
// Every action is checked against the caller's current role, read for that request:
// nothing is cached, so a role change or a ban applies to the next request and to the
// next event subscription of an already connected client.
package servers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/profile"
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

// Rank orders the roles so that "may act on" is one comparison.
func (r Role) Rank() int {
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
	// ErrInvalidDescription means the description is longer than the limit.
	ErrInvalidDescription = errors.New("servers: description must be at most 500 characters")
	// ErrNothingToUpdate means an update named no field.
	ErrNothingToUpdate = errors.New("servers: nothing to update")
)

const (
	maxNameLength        = 64
	maxDescriptionLength = 500
)

type Workspace struct {
	ID          uuid.UUID
	Name        string
	Description string // empty when there is none
	// IconKey is where the icon lives in the object store; empty without one.
	// It never leaves the server: members get IconURL instead.
	IconKey string
	// IconURL is a signed link to the icon, filled in for members only.
	IconURL string
	// MemberPermissions is what the member role may do here.
	MemberPermissions Permissions
	OwnerID           uuid.UUID
	CreatedAt         time.Time
}

// NewWorkspace is what creating a server takes. Permissions not sent keep
// their defaults.
type NewWorkspace struct {
	Name        string
	Description string
	Permissions PermissionsUpdate
}

// WorkspaceUpdate carries only what the caller asked to change. An empty
// description clears it.
type WorkspaceUpdate struct {
	Name        *string
	Description *string
	Permissions PermissionsUpdate
}

func (u WorkspaceUpdate) empty() bool {
	return u.Name == nil && u.Description == nil && u.Permissions.empty()
}

// Membership is a server plus the role of the account that asked for it.
type Membership struct {
	Workspace Workspace
	Role      Role
}

// SharedServer is one server two accounts are on, as the caller sees it. The
// role is the caller's own; IconKey stays in this module and the service turns
// it into a signed IconURL.
type SharedServer struct {
	ID      uuid.UUID
	Name    string
	IconKey string
	IconURL string
	Role    Role
}

// Member is one person on a server. User is the public profile: this module
// reads it from internal/profile in one batch, never from its own SQL.
type Member struct {
	User     profile.Public
	Role     Role
	JoinedAt time.Time
}

type Ban struct {
	User      profile.Public
	Reason    string
	BannedBy  uuid.UUID // uuid.Nil when the account that banned is gone
	CreatedAt time.Time
}

// Store is the persistence this package needs.
type Store interface {
	// CreateWorkspace creates the server and its owner membership in one transaction.
	CreateWorkspace(ctx context.Context, n NewWorkspace, permissions Permissions, ownerID uuid.UUID) (Workspace, error)
	Workspace(ctx context.Context, id uuid.UUID) (Workspace, error)
	UpdateWorkspace(ctx context.Context, id uuid.UUID, u WorkspaceUpdate) (Workspace, error)
	// SetIcon stores the new icon key (empty removes it) and returns the one it replaced.
	SetIcon(ctx context.Context, id uuid.UUID, key string) (previous string, err error)
	DeleteWorkspace(ctx context.Context, id uuid.UUID) error
	ListMemberships(ctx context.Context, userID uuid.UUID) ([]Membership, error)
	// MutualServers lists the servers both accounts are on, with the caller's
	// role. It is one query for the whole list, never one per server.
	MutualServers(ctx context.Context, callerID, otherID uuid.UUID) ([]SharedServer, error)
	// Role returns ErrNotMember when the account is not on the server.
	Role(ctx context.Context, workspaceID, userID uuid.UUID) (Role, error)
	// Access is the role together with what the server lets members do; it
	// returns ErrNotMember like Role.
	Access(ctx context.Context, workspaceID, userID uuid.UUID) (Role, Permissions, error)
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

// Watchers is the live stream this module keeps honest: it hears about
// membership changes, and it ends the connections of someone who lost access.
// Optional: with nil, the HTTP API behaves the same and nobody is told live.
type Watchers interface {
	Publish(event events.Event)
	// PublishToUser reaches every connection of one account, whatever server it
	// watches: a deleted server must leave rails where it was not open too.
	PublishToUser(userID uuid.UUID, event events.Event)
	Revoke(workspaceID, userID uuid.UUID)
}

// Profiles is the public part of an account, owned by internal/profile. One
// call fills a whole member list, so the list never asks per member.
type Profiles interface {
	Profiles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]profile.Public, error)
}

// Service holds the rules. It knows nothing about HTTP.
type Service struct {
	store    Store
	watchers Watchers
	icons    Icons
	// profiles is optional at construction; main wires it in. Without it a member
	// carries only the id, which is still enough for every permission check.
	profiles Profiles
}

func NewService(store Store, watchers Watchers, icons Icons) *Service {
	return &Service{store: store, watchers: watchers, icons: icons}
}

// SetProfiles wires the profile module in after both exist.
func (s *Service) SetProfiles(profiles Profiles) { s.profiles = profiles }

// withProfiles fills the public part of a whole list in one call. A nil profile
// module leaves the ids as they are; anything else propagates the failure rather
// than silently showing bare ids.
func (s *Service) withProfiles(ctx context.Context, members []Member) error {
	if s.profiles == nil || len(members) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.User.ID)
	}
	profiles, err := s.profiles.Profiles(ctx, ids)
	if err != nil {
		return fmt.Errorf("load member profiles: %w", err)
	}
	for i := range members {
		if loaded, ok := profiles[members[i].User.ID]; ok {
			members[i].User = loaded
		}
	}
	return nil
}

// withProfile fills the public part of one member or ban.
func (s *Service) withProfile(ctx context.Context, account *profile.Public) error {
	if s.profiles == nil {
		return nil
	}
	members := []Member{{User: *account}}
	if err := s.withProfiles(ctx, members); err != nil {
		return err
	}
	*account = members[0].User
	return nil
}

// changed tells every member that the server itself changed. The payload is
// the server as a member sees it, icon link included.
func (s *Service) changed(ctx context.Context, workspace Workspace) {
	s.Announce(ctx, events.Event{
		Kind:        events.KindWorkspaceUpdated,
		WorkspaceID: workspace.ID,
		Payload:     APIWorkspace(s.present(ctx, workspace)),
	})
}

// announce tells every member that the membership of one account changed. The
// account itself hears it too, even when it has just left: its rail must lose
// the server.
func (s *Service) announce(ctx context.Context, workspaceID, userID uuid.UUID, role Role, present bool) {
	s.Announce(ctx, events.Event{
		Kind:        events.KindMemberChanged,
		WorkspaceID: workspaceID,
		Payload: map[string]any{
			"userId":  userID,
			"role":    string(role),
			"present": present,
		},
	}, userID)
}

// AnnounceToStaff sends an event about a server to its admins and owner only:
// the invite links and the ban list are theirs, and nobody else may even learn
// that they changed.
func (s *Service) AnnounceToStaff(ctx context.Context, event events.Event) {
	if s.watchers == nil {
		return
	}
	members, err := s.store.ListMembers(ctx, event.WorkspaceID)
	if err != nil {
		// No shared-stream fallback here: it would reach plain members too.
		return
	}
	for _, member := range members {
		if member.Role.Rank() >= RoleAdmin.Rank() {
			s.watchers.PublishToUser(member.User.ID, event)
		}
	}
}

// Announce sends an event about a server to the personal stream of each of
// its members (and of anyone in also). The server's own stream reaches only
// the connections watching it right now, and most members watch another
// server or none: a rename, a new channel or a new member must reach their
// rails too. Each connection hears it once. Other modules (channels) announce
// through here, so the audience of a server event has one definition.
func (s *Service) Announce(ctx context.Context, event events.Event, also ...uuid.UUID) {
	if s.watchers == nil {
		return
	}
	members, err := s.store.ListMembers(ctx, event.WorkspaceID)
	if err != nil {
		// Better the watchers of the server than nobody.
		s.watchers.Publish(event)
		return
	}
	told := make(map[uuid.UUID]bool, len(members)+len(also))
	for _, member := range members {
		told[member.User.ID] = true
		s.watchers.PublishToUser(member.User.ID, event)
	}
	for _, id := range also {
		if !told[id] {
			told[id] = true
			s.watchers.PublishToUser(id, event)
		}
	}
}

// cutOff ends the live connections of an account that is no longer on the
// server, so a ban or a kick applies to an open connection, not only to the
// next request.
func (s *Service) cutOff(workspaceID, userID uuid.UUID) {
	if s.watchers == nil {
		return
	}
	s.watchers.Revoke(workspaceID, userID)
}

// Authorize reports the caller's role on a server, or ErrNotMember. Other modules
// (channels, messages, the event gateway) call it before they act, so the rule
// "rights are checked on the server" has one implementation.
func (s *Service) Authorize(ctx context.Context, workspaceID, userID uuid.UUID) (Role, error) {
	return s.store.Role(ctx, workspaceID, userID)
}

// Workspace reads a server without checking membership. For callers that have
// their own reason to see it — an invite link, for one.
func (s *Service) Workspace(ctx context.Context, workspaceID uuid.UUID) (Workspace, error) {
	return s.store.Workspace(ctx, workspaceID)
}

// MemberCount is how many people are on a server.
func (s *Service) MemberCount(ctx context.Context, workspaceID uuid.UUID) (int, error) {
	members, err := s.store.ListMembers(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	return len(members), nil
}

// IsMember reports whether the account is on the server. Other modules use it
// to decide, for example, whether a mention reaches someone who can read it.
func (s *Service) IsMember(ctx context.Context, workspaceID, userID uuid.UUID) (bool, error) {
	_, err := s.store.Role(ctx, workspaceID, userID)
	switch {
	case errors.Is(err, ErrNotMember):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// RequireAtLeast reports whether the caller's role on the server reaches `least`.
func (s *Service) RequireAtLeast(ctx context.Context, workspaceID, userID uuid.UUID, least Role) (Role, error) {
	role, err := s.store.Role(ctx, workspaceID, userID)
	if err != nil {
		return "", err
	}
	if role.Rank() < least.Rank() {
		return role, ErrForbidden
	}
	return role, nil
}

func (s *Service) Create(ctx context.Context, n NewWorkspace, ownerID uuid.UUID) (Membership, error) {
	name, err := cleanName(n.Name)
	if err != nil {
		return Membership{}, err
	}
	description, err := cleanDescription(n.Description)
	if err != nil {
		return Membership{}, err
	}
	permissions := n.Permissions.apply(DefaultPermissions())
	workspace, err := s.store.CreateWorkspace(ctx, NewWorkspace{Name: name, Description: description}, permissions, ownerID)
	if err != nil {
		return Membership{}, err
	}
	return Membership{Workspace: s.present(ctx, workspace), Role: RoleOwner}, nil
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	list, err := s.store.ListMemberships(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Workspace = s.present(ctx, list[i].Workspace)
	}
	return list, nil
}

// Servers lists the ids of the servers an account is on. Presence announces to
// them, so it needs the ids and nothing else: no icons are signed, unlike List.
func (s *Service) Servers(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	memberships, err := s.store.ListMemberships(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(memberships))
	for _, membership := range memberships {
		out = append(out, membership.Workspace.ID)
	}
	return out, nil
}

// MutualServers lists the servers both accounts are on, with the caller's role
// and a signed icon link. It is one query for the whole list: the mutual endpoint
// asks this module instead of reading its tables, and no per-server read happens.
func (s *Service) MutualServers(ctx context.Context, callerID, otherID uuid.UUID) ([]profile.MutualServer, error) {
	if callerID == uuid.Nil || otherID == uuid.Nil || callerID == otherID {
		return nil, nil
	}
	shared, err := s.store.MutualServers(ctx, callerID, otherID)
	if err != nil {
		return nil, fmt.Errorf("list mutual servers: %w", err)
	}
	out := make([]profile.MutualServer, 0, len(shared))
	for _, server := range shared {
		out = append(out, profile.MutualServer{
			ID:      server.ID,
			Name:    server.Name,
			IconURL: s.signIconURL(ctx, server.IconKey),
			Role:    string(server.Role),
		})
	}
	return out, nil
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
	return Membership{Workspace: s.present(ctx, workspace), Role: role}, nil
}

// Update changes the name, the description or what members may do. Admin or
// owner. Nothing is cached, so new permissions bind the next request of
// everyone; the event tells connected clients to redraw what they offer.
func (s *Service) Update(ctx context.Context, workspaceID, callerID uuid.UUID, u WorkspaceUpdate) (Membership, error) {
	role, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleAdmin)
	if err != nil {
		return Membership{}, err
	}
	if u.empty() {
		return Membership{}, ErrNothingToUpdate
	}
	if u.Name != nil {
		name, err := cleanName(*u.Name)
		if err != nil {
			return Membership{}, err
		}
		u.Name = &name
	}
	if u.Description != nil {
		description, err := cleanDescription(*u.Description)
		if err != nil {
			return Membership{}, err
		}
		u.Description = &description
	}

	workspace, err := s.store.UpdateWorkspace(ctx, workspaceID, u)
	if err != nil {
		return Membership{}, err
	}
	s.changed(ctx, workspace)
	return Membership{Workspace: s.present(ctx, workspace), Role: role}, nil
}

// Delete removes the server with everything in it, then its icon.
func (s *Service) Delete(ctx context.Context, workspaceID, callerID uuid.UUID) error {
	if _, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleOwner); err != nil {
		return err
	}
	workspace, err := s.store.Workspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	// Who to tell is read before the rows go with the server.
	members, err := s.store.ListMembers(ctx, workspaceID)
	if err != nil {
		return err
	}
	if err := s.store.DeleteWorkspace(ctx, workspaceID); err != nil {
		return err
	}
	// The server is gone either way; an icon left behind is only wasted space.
	s.dropIcon(ctx, workspace.IconKey)
	s.gone(workspaceID, members)
	return nil
}

// gone tells every former member that the server no longer exists and ends
// their subscriptions to it. The event goes to each personal stream, not to
// the server's: most members are not watching this server at the moment.
func (s *Service) gone(workspaceID uuid.UUID, members []Member) {
	if s.watchers == nil {
		return
	}
	for _, member := range members {
		s.watchers.PublishToUser(member.User.ID, events.Event{
			Kind:        events.KindWorkspaceDeleted,
			WorkspaceID: workspaceID,
			Payload:     map[string]any{"workspaceId": workspaceID},
		})
		s.watchers.Revoke(workspaceID, member.User.ID)
	}
}

func (s *Service) ListMembers(ctx context.Context, workspaceID, callerID uuid.UUID) ([]Member, error) {
	if _, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleMember); err != nil {
		return nil, err
	}
	members, err := s.store.ListMembers(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if err := s.withProfiles(ctx, members); err != nil {
		return nil, err
	}
	return members, nil
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
	member, err := s.store.AddMember(ctx, workspaceID, userID, RoleMember)
	if err != nil {
		return Member{}, err
	}
	if err := s.withProfile(ctx, &member.User); err != nil {
		return Member{}, err
	}
	s.announce(ctx, workspaceID, userID, RoleMember, true)
	return member, nil
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
		member, err := s.store.SetRole(ctx, workspaceID, targetID, role)
		if err != nil {
			return Member{}, err
		}
		if err := s.withProfile(ctx, &member.User); err != nil {
			return Member{}, err
		}
		return member, nil
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
	if err := s.withProfile(ctx, &member.User); err != nil {
		return Member{}, err
	}
	s.announce(ctx, workspaceID, targetID, role, true)
	// Handing over ownership: the previous owner stays as an admin, so the server
	// never has two owners and the caller does not lock themselves out of the room.
	if role == RoleOwner && callerID != targetID {
		if _, err := s.store.SetRole(ctx, workspaceID, callerID, RoleAdmin); err != nil {
			return Member{}, err
		}
		s.announce(ctx, workspaceID, callerID, RoleAdmin, true)
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
		return s.removeMember(ctx, workspaceID, targetID)
	}

	if err := canActOn(caller, target); err != nil {
		return err
	}
	return s.removeMember(ctx, workspaceID, targetID)
}

// removeMember drops the membership and ends the live connections of that account.
func (s *Service) removeMember(ctx context.Context, workspaceID, userID uuid.UUID) error {
	if err := s.store.RemoveMember(ctx, workspaceID, userID); err != nil {
		return err
	}
	s.announce(ctx, workspaceID, userID, "", false)
	s.cutOff(workspaceID, userID)
	return nil
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

	ban, err := s.store.Ban(ctx, workspaceID, targetID, callerID, strings.TrimSpace(reason))
	if err != nil {
		return Ban{}, err
	}
	if err := s.withProfile(ctx, &ban.User); err != nil {
		return Ban{}, err
	}
	s.announce(ctx, workspaceID, targetID, "", false)
	s.AnnounceToStaff(ctx, events.Event{Kind: events.KindBansChanged, WorkspaceID: workspaceID})
	s.cutOff(workspaceID, targetID)
	return ban, nil
}

func (s *Service) Unban(ctx context.Context, workspaceID, callerID, targetID uuid.UUID) error {
	if _, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleAdmin); err != nil {
		return err
	}
	if err := s.store.Unban(ctx, workspaceID, targetID); err != nil {
		return err
	}
	s.AnnounceToStaff(ctx, events.Event{Kind: events.KindBansChanged, WorkspaceID: workspaceID})
	return nil
}

func (s *Service) ListBans(ctx context.Context, workspaceID, callerID uuid.UUID) ([]Ban, error) {
	if _, err := s.RequireAtLeast(ctx, workspaceID, callerID, RoleAdmin); err != nil {
		return nil, err
	}
	bans, err := s.store.ListBans(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	// One batch for the whole ban list, the same shape the member list uses.
	accounts := make([]Member, len(bans))
	for i := range bans {
		accounts[i].User = bans[i].User
	}
	if err := s.withProfiles(ctx, accounts); err != nil {
		return nil, err
	}
	for i := range bans {
		bans[i].User = accounts[i].User
	}
	return bans, nil
}

// canActOn allows moderation only downwards: an admin may act on members, the
// owner on everyone below.
func canActOn(caller, target Role) error {
	if caller.Rank() <= target.Rank() {
		return ErrForbidden
	}
	return nil
}

func cleanDescription(description string) (string, error) {
	description = strings.TrimSpace(description)
	if len([]rune(description)) > maxDescriptionLength {
		return "", ErrInvalidDescription
	}
	return description, nil
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > maxNameLength {
		return "", ErrInvalidName
	}
	return name, nil
}
