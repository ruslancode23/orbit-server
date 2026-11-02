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

	"github.com/ruslancode23/orbit-server/internal/audit"
	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/perms"
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
	// ErrInvalidAutomute means the automute of new members is out of range.
	ErrInvalidAutomute = errors.New("servers: automute must be 0 to 604800 seconds")
	// ErrInvalidMentionLimit means the mention cap is out of range.
	ErrInvalidMentionLimit = errors.New("servers: the mention limit must be 0 to 100")
	// ErrInvalidLinkLimit means the link cap is out of range.
	ErrInvalidLinkLimit = errors.New("servers: the link limit must be 0 to 100")
	// ErrInvalidMuteDuration means the timeout is longer than a mute may last.
	ErrInvalidMuteDuration = errors.New("servers: a mute lasts 0 to 2419200 seconds")
	// ErrInvalidMuteReason means the reason is longer than a mute reason may be.
	ErrInvalidMuteReason = errors.New("servers: the mute reason must be at most 500 characters")
)

const (
	maxNameLength        = 64
	maxDescriptionLength = 500
	// maxAutomuteSeconds is the longest automute a server may give a newcomer:
	// a week, after which they are simply a member.
	maxAutomuteSeconds = 604800
	// maxMentionsPerMessage and maxLinksPerMessage are the largest caps a server
	// may set; 0 turns a cap off.
	maxMentionsPerMessage = 100
	maxLinksPerMessage    = 100
	// maxMuteDuration is the longest timeout a moderator may set: 28 days, the
	// same bound the API schema states in seconds.
	maxMuteDuration = 28 * 24 * time.Hour
	maxMuteReason   = 500
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
	// MemberPermissions is what @everyone may do here, in the old API's flags.
	MemberPermissions Permissions
	// Everyone is what @everyone may do here.
	Everyone perms.Set
	// AutomuteNewMembersSeconds silences a new member for this long on joining;
	// 0 turns it off.
	AutomuteNewMembersSeconds int
	// MaxMentionsPerMessage and MaxLinksPerMessage cap one message; 0 turns the
	// cap off. They exist because mentions and links are what spam is made of.
	MaxMentionsPerMessage int
	MaxLinksPerMessage    int
	OwnerID               uuid.UUID
	CreatedAt             time.Time
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
	// The moderation settings (SCRUM-163); nil keeps the current value.
	AutomuteNewMembersSeconds *int
	MaxMentionsPerMessage     *int
	MaxLinksPerMessage        *int
}

func (u WorkspaceUpdate) empty() bool {
	return u.Name == nil && u.Description == nil && u.Permissions.empty() &&
		u.AutomuteNewMembersSeconds == nil && u.MaxMentionsPerMessage == nil && u.MaxLinksPerMessage == nil
}

// Membership is a server plus the role of the account that asked for it.
type Membership struct {
	Workspace Workspace
	Role      Role
	// Permissions is what the account may do on the server before any channel.
	Permissions perms.Set
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
	// Permissions is what the member may do on the server before any channel.
	Permissions perms.Set
	// RoleIDs are the roles granted to the member; @everyone is implied.
	RoleIDs []uuid.UUID
	// MutedUntil is when a timeout on the member ends; nil means they may write
	// now. A past moment reads as no mute: nothing has to clean it up.
	MutedUntil *time.Time
}

type Ban struct {
	User      profile.Public
	Reason    string
	BannedBy  uuid.UUID // uuid.Nil when the account that banned is gone
	CreatedAt time.Time
}

// SendRules is what a server imposes on one member's next message: whether the
// member is in a timeout, and how many mentions and links one message may
// carry. The caps are the server's, decided by whoever manages it; the timeout
// belongs to the member. internal/messages asks for this once per send.
type SendRules struct {
	// MutedUntil is when the timeout ends; nil, or a moment already past, means
	// the member may write.
	MutedUntil *time.Time
	// MaxMentions and MaxLinks are the caps; 0 means no cap.
	MaxMentions int
	MaxLinks    int
}

// Muted reports whether the timeout is still running at this moment.
func (r SendRules) Muted(now time.Time) bool {
	return r.MutedUntil != nil && now.Before(*r.MutedUntil)
}

// Store is the persistence this package needs.
type Store interface {
	// CreateWorkspace creates the server and its owner membership in one transaction.
	// The new server gets its @everyone role with these permissions.
	CreateWorkspace(ctx context.Context, n NewWorkspace, everyone perms.Set, ownerID uuid.UUID) (Workspace, error)
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
	// Access is the roles of the server and the ones the account holds; it
	// returns ErrNotMember like Role.
	Access(ctx context.Context, workspaceID, userID uuid.UUID) (Access, error)
	// SetEveryone replaces what @everyone may do.
	SetEveryone(ctx context.Context, workspaceID uuid.UUID, permissions perms.Set) error
	// GrantAdmin gives the member the server's administrator role, making one
	// when there is none yet; RevokeAdmin takes away every role that grants
	// administrator. The old API's «admin» lives on through them.
	GrantAdmin(ctx context.Context, workspaceID, userID uuid.UUID) error
	RevokeAdmin(ctx context.Context, workspaceID, userID uuid.UUID) error
	// Roles are the roles of a server, highest first, @everyone last.
	Roles(ctx context.Context, workspaceID uuid.UUID) ([]ServerRole, error)
	// RoleByID returns ErrRoleNotFound when there is no such role.
	RoleByID(ctx context.Context, roleID uuid.UUID) (ServerRole, error)
	// CreateRole puts the role just above @everyone and moves the rest up.
	CreateRole(ctx context.Context, workspaceID uuid.UUID, n NewRole) (ServerRole, error)
	UpdateRole(ctx context.Context, roleID uuid.UUID, u RoleUpdate) (ServerRole, error)
	// DeleteRole removes the role and closes the gap in the order.
	DeleteRole(ctx context.Context, workspaceID, roleID uuid.UUID) error
	// SetRoleOrder writes the order of every role but @everyone, highest first.
	SetRoleOrder(ctx context.Context, workspaceID uuid.UUID, ids []uuid.UUID) error
	GrantRole(ctx context.Context, workspaceID, userID, roleID uuid.UUID) error
	RevokeRole(ctx context.Context, workspaceID, userID, roleID uuid.UUID) error
	SetRole(ctx context.Context, workspaceID, userID uuid.UUID, role Role) (Member, error)
	AddMember(ctx context.Context, workspaceID, userID uuid.UUID, role Role) (Member, error)
	RemoveMember(ctx context.Context, workspaceID, userID uuid.UUID) error
	ListMembers(ctx context.Context, workspaceID uuid.UUID) ([]Member, error)
	// SendRules is the timeout and the per-message caps that apply to one
	// member; ErrNotMember when the account is not on the server.
	SendRules(ctx context.Context, workspaceID, userID uuid.UUID) (SendRules, error)
	// SetMute writes a member's timeout; nil until lifts it and leaves no
	// reason behind. ErrNotMember when there is no such membership.
	SetMute(ctx context.Context, workspaceID, userID uuid.UUID, until *time.Time, reason string) error
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

// Auditor records moderation actions in the journal (internal/audit). Optional:
// with nil nothing is written, and a failure is never allowed to undo the
// action it describes — see record.
type Auditor interface {
	Record(ctx context.Context, entry audit.Entry) error
}

// Service holds the rules. It knows nothing about HTTP.
type Service struct {
	store    Store
	watchers Watchers
	icons    Icons
	// profiles is optional at construction; main wires it in. Without it a member
	// carries only the id, which is still enough for every permission check.
	profiles Profiles
	auditor  Auditor
	auditLog AuditLog
}

func NewService(store Store, watchers Watchers, icons Icons) *Service {
	return &Service{store: store, watchers: watchers, icons: icons}
}

// SetAuditor wires the journal in after construction.
func (s *Service) SetAuditor(a Auditor) { s.auditor = a }

// record appends one moderation action to the journal. A missing journal or a
// failed insert is deliberately ignored here: the ban, kick, role or channel
// change has already been committed, and the journal's own error is logged by
// internal/audit. Undoing a moderation action because its second record could
// not be written would be worse than a gap that shows up in the log.
func (s *Service) record(ctx context.Context, actorID uuid.UUID, entry audit.Entry) {
	if s.auditor == nil {
		return
	}
	entry.ActorID = &actorID
	_ = s.auditor.Record(ctx, entry)
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
// the server. The timeout travels with it, so a client shows «you are muted»
// without asking again; the reason behind it never does.
func (s *Service) announce(ctx context.Context, workspaceID, userID uuid.UUID, role Role, present bool, mutedUntil *time.Time) {
	payload := map[string]any{
		"userId":  userID,
		"role":    string(role),
		"present": present,
		// Always present, null when the member may write: a client replaces the
		// whole entry and must not keep a timeout that has just been lifted.
		"mutedUntil": isoOrNil(mutedUntil),
	}
	s.Announce(ctx, events.Event{
		Kind:        events.KindMemberChanged,
		WorkspaceID: workspaceID,
		Payload:     payload,
	}, userID)
}

// isoOrNil is a moment in the API's format, or nil for "no moment".
func isoOrNil(moment *time.Time) *string {
	if moment == nil {
		return nil
	}
	formatted := moment.UTC().Format(time.RFC3339)
	return &formatted
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
	access, err := s.store.Access(ctx, workspaceID, userID)
	if err != nil {
		return "", err
	}
	return access.Role(), nil
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

// RequireAtLeast reports whether the caller reaches `least` in the old API's
// words: member is any member, admin is administrator (by role), owner is the
// owner. New code asks Require for the permission it needs instead.
func (s *Service) RequireAtLeast(ctx context.Context, workspaceID, userID uuid.UUID, least Role) (Role, error) {
	access, err := s.store.Access(ctx, workspaceID, userID)
	if err != nil {
		return "", err
	}
	role := access.Role()
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
	everyone := n.Permissions.apply(DefaultPermissions()).everyoneOf()
	workspace, err := s.store.CreateWorkspace(ctx, NewWorkspace{Name: name, Description: description}, everyone, ownerID)
	if err != nil {
		return Membership{}, err
	}
	return Membership{Workspace: s.present(ctx, workspace), Role: RoleOwner, Permissions: perms.All}, nil
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
	access, err := s.store.Access(ctx, workspaceID, callerID)
	if err != nil {
		return Membership{}, err
	}
	workspace, err := s.store.Workspace(ctx, workspaceID)
	if err != nil {
		return Membership{}, err
	}
	return Membership{Workspace: s.present(ctx, workspace), Role: access.Role(), Permissions: access.Base()}, nil
}

// Update changes the name, the description or what @everyone may do; it takes
// manageServer. Nothing is cached, so new permissions bind the next request of
// everyone; the event tells connected clients to redraw what they offer.
func (s *Service) Update(ctx context.Context, workspaceID, callerID uuid.UUID, u WorkspaceUpdate) (Membership, error) {
	access, err := s.Require(ctx, workspaceID, callerID, perms.ManageServer)
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
	if u.AutomuteNewMembersSeconds != nil &&
		(*u.AutomuteNewMembersSeconds < 0 || *u.AutomuteNewMembersSeconds > maxAutomuteSeconds) {
		return Membership{}, ErrInvalidAutomute
	}
	if u.MaxMentionsPerMessage != nil && (*u.MaxMentionsPerMessage < 0 || *u.MaxMentionsPerMessage > maxMentionsPerMessage) {
		return Membership{}, ErrInvalidMentionLimit
	}
	if u.MaxLinksPerMessage != nil && (*u.MaxLinksPerMessage < 0 || *u.MaxLinksPerMessage > maxLinksPerMessage) {
		return Membership{}, ErrInvalidLinkLimit
	}

	if !u.Permissions.empty() {
		current, err := s.store.Workspace(ctx, workspaceID)
		if err != nil {
			return Membership{}, err
		}
		flags := u.Permissions.apply(current.MemberPermissions).everyoneOf() & memberFlags
		if err := s.store.SetEveryone(ctx, workspaceID, current.Everyone&^memberFlags|flags); err != nil {
			return Membership{}, err
		}
	}
	workspace, err := s.store.UpdateWorkspace(ctx, workspaceID, u)
	if err != nil {
		return Membership{}, err
	}
	s.changed(ctx, workspace)
	return Membership{Workspace: s.present(ctx, workspace), Role: access.Role(), Permissions: access.Base()}, nil
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
// account cannot come back through a link. A server that automutes its
// newcomers gives the account a timeout in the same breath, so it cannot slip a
// message in between joining and being silenced.
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
	workspace, err := s.store.Workspace(ctx, workspaceID)
	if err != nil {
		return Member{}, err
	}
	if workspace.AutomuteNewMembersSeconds > 0 {
		until := time.Now().Add(time.Duration(workspace.AutomuteNewMembersSeconds) * time.Second)
		// The membership is written; a failed automute is a missing timeout, not
		// a member left outside, so it is reported rather than rolled back.
		if err := s.store.SetMute(ctx, workspaceID, userID, &until, ""); err != nil {
			return Member{}, err
		}
		member.MutedUntil = &until
	}
	if err := s.withProfile(ctx, &member.User); err != nil {
		return Member{}, err
	}
	s.announce(ctx, workspaceID, userID, member.Role, true, member.MutedUntil)
	return member, nil
}

// SetRole moves a member between the old API's roles. Handing over ownership
// is the owner's alone, and the server keeps an owner: the old owner becomes an
// administrator in the same breath. Making someone an admin grants the server's
// administrator role, making them a member takes every role that grants
// administrator; both take manageRoles, a rank above the target and above the
// administrator role, since nobody hands out what lies above them.
func (s *Service) SetRole(ctx context.Context, workspaceID, callerID, targetID uuid.UUID, role Role) (Member, error) {
	if !role.valid() {
		return Member{}, ErrInvalidRole
	}
	caller, err := s.store.Access(ctx, workspaceID, callerID)
	if err != nil {
		return Member{}, err
	}
	target, err := s.store.Access(ctx, workspaceID, targetID)
	if err != nil {
		return Member{}, err
	}
	if target.Role() == role {
		return s.memberOf(ctx, workspaceID, targetID)
	}

	if role == RoleOwner || target.Member.Owner {
		if !caller.Member.Owner {
			return Member{}, ErrForbidden
		}
		return s.moveOwnership(ctx, workspaceID, callerID, targetID, role, target)
	}

	if callerID != targetID && !caller.Outranks(target.Member) {
		return Member{}, ErrForbidden
	}
	if !caller.Member.Owner && (!caller.Base().Has(perms.ManageRoles | perms.Administrator)) {
		return Member{}, ErrForbidden
	}
	if !caller.Member.Owner && !caller.Server.MayManageRole(caller.Member, perms.Role{Position: adminPosition(caller.Server)}) {
		return Member{}, ErrForbidden
	}
	switch role {
	case RoleAdmin:
		err = s.store.GrantAdmin(ctx, workspaceID, targetID)
	default:
		err = s.store.RevokeAdmin(ctx, workspaceID, targetID)
	}
	if err != nil {
		return Member{}, err
	}
	action := audit.ActionMemberRoleRevoke
	if role == RoleAdmin {
		action = audit.ActionMemberRoleGrant
	}
	s.record(ctx, callerID, audit.Entry{
		WorkspaceID: workspaceID,
		Action:      action,
		TargetType:  audit.TargetUser,
		TargetID:    audit.Target(targetID),
		// The old API's roles are the only names this path has: the server's
		// administrator role is created on demand and has no id to cite here.
		Metadata: map[string]any{"role": string(role)},
	})
	member, err := s.memberOf(ctx, workspaceID, targetID)
	if err != nil {
		return Member{}, err
	}
	s.announce(ctx, workspaceID, targetID, role, true, member.MutedUntil)
	return member, nil
}

// moveOwnership hands the owner over, or takes it from an owner when the
// server has another one. The previous owner stays as an administrator.
func (s *Service) moveOwnership(ctx context.Context, workspaceID, callerID, targetID uuid.UUID, role Role, target Access) (Member, error) {
	if target.Member.Owner && role != RoleOwner {
		owners, err := s.store.CountOwners(ctx, workspaceID)
		if err != nil {
			return Member{}, err
		}
		if owners <= 1 {
			return Member{}, ErrLastOwner
		}
		if _, err := s.store.SetRole(ctx, workspaceID, targetID, RoleMember); err != nil {
			return Member{}, err
		}
		if role == RoleAdmin {
			if err := s.store.GrantAdmin(ctx, workspaceID, targetID); err != nil {
				return Member{}, err
			}
		}
		// Taking ownership away from an owner is a role change like any other;
		// the new owner is not named, because there is none in this branch.
		s.record(ctx, callerID, audit.Entry{
			WorkspaceID: workspaceID,
			Action:      audit.ActionMemberRoleRevoke,
			TargetType:  audit.TargetUser,
			TargetID:    audit.Target(targetID),
			Metadata:    map[string]any{"role": string(RoleOwner)},
		})
		member, err := s.memberOf(ctx, workspaceID, targetID)
		if err != nil {
			return Member{}, err
		}
		s.announce(ctx, workspaceID, targetID, role, true, member.MutedUntil)
		return member, nil
	}

	if _, err := s.store.SetRole(ctx, workspaceID, targetID, RoleOwner); err != nil {
		return Member{}, err
	}
	member, err := s.memberOf(ctx, workspaceID, targetID)
	if err != nil {
		return Member{}, err
	}
	s.announce(ctx, workspaceID, targetID, RoleOwner, true, member.MutedUntil)
	if callerID != targetID {
		if _, err := s.store.SetRole(ctx, workspaceID, callerID, RoleMember); err != nil {
			return Member{}, err
		}
		if err := s.store.GrantAdmin(ctx, workspaceID, callerID); err != nil {
			return Member{}, err
		}
		previous, err := s.memberOf(ctx, workspaceID, callerID)
		if err != nil {
			return Member{}, err
		}
		s.announce(ctx, workspaceID, callerID, RoleAdmin, true, previous.MutedUntil)
	}
	s.record(ctx, callerID, audit.Entry{
		WorkspaceID: workspaceID,
		Action:      audit.ActionOwnershipTransfer,
		TargetType:  audit.TargetUser,
		TargetID:    audit.Target(targetID),
		Metadata:    map[string]any{"previousOwnerId": callerID},
	})
	return member, nil
}

// adminPosition is where the server's administrator role sits, or just above
// every role when there is none yet (it would be made there).
func adminPosition(server perms.Server) int {
	position := 0
	for _, role := range server.Roles {
		if !role.Default && role.Permissions == perms.Administrator {
			return role.Position
		}
		if role.Position > position {
			position = role.Position
		}
	}
	return position + 1
}

// memberOf is one member with their public profile.
func (s *Service) memberOf(ctx context.Context, workspaceID, userID uuid.UUID) (Member, error) {
	members, err := s.store.ListMembers(ctx, workspaceID)
	if err != nil {
		return Member{}, err
	}
	for _, member := range members {
		if member.User.ID == userID {
			if err := s.withProfile(ctx, &member.User); err != nil {
				return Member{}, err
			}
			return member, nil
		}
	}
	return Member{}, ErrNotMember
}

// Remove kicks a member: kickMembers and a rank above the target. Anyone may
// leave by removing themselves — except the last owner.
func (s *Service) Remove(ctx context.Context, workspaceID, callerID, targetID uuid.UUID) error {
	caller, err := s.store.Access(ctx, workspaceID, callerID)
	if err != nil {
		return err
	}
	target, err := s.store.Access(ctx, workspaceID, targetID)
	if err != nil {
		return err
	}

	if callerID == targetID {
		if target.Member.Owner {
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

	if !caller.Base().Has(perms.KickMembers) || !caller.Outranks(target.Member) {
		return ErrForbidden
	}
	if err := s.removeMember(ctx, workspaceID, targetID); err != nil {
		return err
	}
	s.record(ctx, callerID, audit.Entry{
		WorkspaceID: workspaceID,
		Action:      audit.ActionMemberKick,
		TargetType:  audit.TargetUser,
		TargetID:    audit.Target(targetID),
	})
	return nil
}

// removeMember drops the membership and ends the live connections of that account.
func (s *Service) removeMember(ctx context.Context, workspaceID, userID uuid.UUID) error {
	if err := s.store.RemoveMember(ctx, workspaceID, userID); err != nil {
		return err
	}
	s.announce(ctx, workspaceID, userID, "", false, nil)
	s.cutOff(workspaceID, userID)
	return nil
}

// Ban removes someone and keeps them out. Admin or owner, and never against an
// equal or higher role.
func (s *Service) Ban(ctx context.Context, workspaceID, callerID, targetID uuid.UUID, reason string) (Ban, error) {
	caller, err := s.Require(ctx, workspaceID, callerID, perms.BanMembers)
	if err != nil {
		return Ban{}, err
	}
	if callerID == targetID {
		return Ban{}, ErrForbidden
	}

	// A target who already left is banned anyway, so a link cannot bring them back.
	target, err := s.store.Access(ctx, workspaceID, targetID)
	switch {
	case err == nil:
		if !caller.Outranks(target.Member) {
			return Ban{}, ErrForbidden
		}
	case !errors.Is(err, ErrNotMember):
		return Ban{}, err
	}

	ban, err := s.store.Ban(ctx, workspaceID, targetID, callerID, strings.TrimSpace(reason))
	if err != nil {
		return Ban{}, err
	}
	s.record(ctx, callerID, audit.Entry{
		WorkspaceID: workspaceID,
		Action:      audit.ActionMemberBan,
		TargetType:  audit.TargetUser,
		TargetID:    audit.Target(targetID),
	})
	if err := s.withProfile(ctx, &ban.User); err != nil {
		return Ban{}, err
	}
	s.announce(ctx, workspaceID, targetID, "", false, nil)
	s.AnnounceToStaff(ctx, events.Event{Kind: events.KindBansChanged, WorkspaceID: workspaceID})
	s.cutOff(workspaceID, targetID)
	return ban, nil
}

func (s *Service) Unban(ctx context.Context, workspaceID, callerID, targetID uuid.UUID) error {
	if _, err := s.Require(ctx, workspaceID, callerID, perms.BanMembers); err != nil {
		return err
	}
	if err := s.store.Unban(ctx, workspaceID, targetID); err != nil {
		return err
	}
	s.record(ctx, callerID, audit.Entry{
		WorkspaceID: workspaceID,
		Action:      audit.ActionMemberUnban,
		TargetType:  audit.TargetUser,
		TargetID:    audit.Target(targetID),
	})
	s.AnnounceToStaff(ctx, events.Event{Kind: events.KindBansChanged, WorkspaceID: workspaceID})
	return nil
}

func (s *Service) ListBans(ctx context.Context, workspaceID, callerID uuid.UUID) ([]Ban, error) {
	if _, err := s.Require(ctx, workspaceID, callerID, perms.BanMembers); err != nil {
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

// SendRules reports what this server imposes on one member's next message: the
// timeout still running, if any, and the caps on mentions and links.
// internal/messages asks before it stores anything.
func (s *Service) SendRules(ctx context.Context, workspaceID, userID uuid.UUID) (SendRules, error) {
	return s.store.SendRules(ctx, workspaceID, userID)
}

// Mute puts a timeout on a member, or lifts it when the duration is zero.
// ManageMessages is the right over other people's writing, so it is the right
// to stop it for a while; the actor must outrank the target, and nobody may
// silence themselves — a mute one could lift at will is not a mute. Reading,
// events and voice are untouched: only sending is refused while it runs. The
// reason is stored with the mute and goes to the journal, never to an event.
func (s *Service) Mute(ctx context.Context, workspaceID, callerID, targetID uuid.UUID, duration time.Duration, reason string) (Member, error) {
	if duration < 0 || duration > maxMuteDuration {
		return Member{}, ErrInvalidMuteDuration
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > maxMuteReason {
		return Member{}, ErrInvalidMuteReason
	}
	caller, err := s.Require(ctx, workspaceID, callerID, perms.ManageMessages)
	if err != nil {
		return Member{}, err
	}
	if callerID == targetID {
		return Member{}, ErrForbidden
	}
	target, err := s.store.Access(ctx, workspaceID, targetID)
	if err != nil {
		return Member{}, err
	}
	// Only downwards: an equal or higher role, and the owner, are out of reach.
	if !caller.Outranks(target.Member) {
		return Member{}, ErrForbidden
	}

	var until *time.Time
	if duration > 0 {
		moment := time.Now().Add(duration)
		until = &moment
	}
	if err := s.store.SetMute(ctx, workspaceID, targetID, until, reason); err != nil {
		return Member{}, err
	}
	if until == nil {
		s.record(ctx, callerID, audit.Entry{
			WorkspaceID: workspaceID,
			Action:      audit.ActionMemberUnmute,
			TargetType:  audit.TargetUser,
			TargetID:    audit.Target(targetID),
		})
	} else {
		metadata := map[string]any{"durationSeconds": int(duration / time.Second)}
		if reason != "" {
			metadata["reason"] = reason
		}
		s.record(ctx, callerID, audit.Entry{
			WorkspaceID: workspaceID,
			Action:      audit.ActionMemberMute,
			TargetType:  audit.TargetUser,
			TargetID:    audit.Target(targetID),
			Metadata:    metadata,
		})
	}
	member, err := s.memberOf(ctx, workspaceID, targetID)
	if err != nil {
		return Member{}, err
	}
	s.announce(ctx, workspaceID, targetID, member.Role, true, member.MutedUntil)
	return member, nil
}

// canActOn allows moderation only downwards: an admin may act on members, the
// owner on everyone below.
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
