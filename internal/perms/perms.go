// Package perms computes what an account may do on a server and in one of its
// channels, from the roles it holds and the channel's overrides. It is pure:
// no database, no HTTP, so the rules have one place and are tested in full.
//
// The model follows Discord. Every server has a default role, @everyone, that
// all members hold. A member's base set is the union of @everyone and their
// roles; Administrator grants everything. In a channel, the @everyone override
// applies first (deny, then allow), then the overrides of the member's roles
// together (all denies, then all allows). The owner may do everything.
package perms

import (
	"slices"

	"github.com/google/uuid"
)

// Set is a set of permissions, one bit each. It is stored as a bigint.
type Set uint64

// The permissions. The order of the bits is part of the stored data: append,
// never reorder.
const (
	// Administrator grants every permission; channels cannot take it away.
	Administrator Set = 1 << iota
	// ManageServer is the name, description, icon and the server's settings.
	ManageServer
	// ManageRoles is creating, editing, ordering and granting the roles below
	// the actor's highest one.
	ManageRoles
	// ManageChannels is editing, deleting and ordering channels and their
	// overrides; it includes creating them.
	ManageChannels
	// CreateChannels is creating channels only.
	CreateChannels
	// KickMembers is removing members ranked below the actor.
	KickMembers
	// BanMembers is banning members ranked below the actor.
	BanMembers
	// ManageMessages is deleting other people's messages.
	ManageMessages
	// CreateInvite is making invite links.
	CreateInvite
	// AttachFiles is sending files.
	AttachFiles
	// ViewChannel is seeing a channel, its history, and joining it by voice.
	ViewChannel
	// SendMessages is writing in a text channel.
	SendMessages
	// Speak is talking in a voice channel.
	Speak

	// lastBit marks the end; keep it last.
	lastBit
)

// All is every permission there is.
const All = lastBit - 1

// ChannelScoped are the permissions an override may change in one channel.
// The rest belong to the server as a whole.
const ChannelScoped = ManageChannels | ManageMessages | CreateInvite | AttachFiles |
	ViewChannel | SendMessages | Speak

// Has reports whether every permission in want is in s.
func (s Set) Has(want Set) bool { return s&want == want }

// Valid reports whether s names only permissions that exist.
func (s Set) Valid() bool { return s&^All == 0 }

// Role is one role of a server as the rules need it.
type Role struct {
	ID          uuid.UUID
	Position    int
	Permissions Set
	// Default marks @everyone: every member holds it without being granted it.
	Default bool
}

// Override changes what one role may do in one channel. Deny and allow are
// both possible; a bit in neither keeps what the server gives.
type Override struct {
	RoleID uuid.UUID
	Allow  Set
	Deny   Set
}

// Member is one account on one server as the rules need it.
type Member struct {
	Owner bool
	// Roles are the roles granted to the member; @everyone is implied.
	Roles []uuid.UUID
}

// Server is the roles of one server, @everyone among them.
type Server struct {
	Roles []Role
}

// everyone is the default role, or the zero Role when a server has none.
func (s Server) everyone() Role {
	for _, role := range s.Roles {
		if role.Default {
			return role
		}
	}
	return Role{}
}

func (s Server) role(id uuid.UUID) (Role, bool) {
	for _, role := range s.Roles {
		if role.ID == id {
			return role, true
		}
	}
	return Role{}, false
}

// Base is what the member may do on the server, before any channel.
func (s Server) Base(member Member) Set {
	if member.Owner {
		return All
	}
	set := s.everyone().Permissions
	for _, id := range member.Roles {
		if role, ok := s.role(id); ok {
			set |= role.Permissions
		}
	}
	if set.Has(Administrator) {
		return All
	}
	return set
}

// InChannel is what the member may do in a channel with these overrides.
func (s Server) InChannel(member Member, overrides []Override) Set {
	base := s.Base(member)
	if base.Has(Administrator) {
		return All
	}
	set := base
	everyone := s.everyone()
	for _, override := range overrides {
		if everyone.ID != uuid.Nil && override.RoleID == everyone.ID {
			set &^= override.Deny & ChannelScoped
			set |= override.Allow & ChannelScoped
		}
	}
	var allow, deny Set
	for _, override := range overrides {
		if slices.Contains(member.Roles, override.RoleID) {
			allow |= override.Allow
			deny |= override.Deny
		}
	}
	set &^= deny & ChannelScoped
	set |= allow & ChannelScoped
	return set
}

// Top is the position of the member's highest role; @everyone counts as 0 and
// the owner is above every role.
func (s Server) Top(member Member) int {
	if member.Owner {
		return int(^uint(0) >> 1)
	}
	top := 0
	for _, id := range member.Roles {
		if role, ok := s.role(id); ok && !role.Default && role.Position > top {
			top = role.Position
		}
	}
	return top
}

// Outranks reports whether actor may act on target: kick, ban, change their
// roles. Only someone strictly higher may; nobody acts on the owner, and the
// owner may act on anyone else.
func (s Server) Outranks(actor, target Member) bool {
	if target.Owner {
		return false
	}
	return s.Top(actor) > s.Top(target)
}

// MayManageRole reports whether actor may edit, grant or take away this role:
// it must lie strictly below the actor's highest role. @everyone is edited by
// whoever may manage roles at all, but it is never granted or taken away.
func (s Server) MayManageRole(actor Member, role Role) bool {
	if !s.Base(actor).Has(ManageRoles) {
		return false
	}
	if role.Default {
		return true
	}
	return role.Position < s.Top(actor)
}

// MayGrant reports whether actor may put these permissions on a role: nobody
// hands out what they do not have themselves.
func (s Server) MayGrant(actor Member, permissions Set) bool {
	return s.Base(actor).Has(permissions)
}

// names are the permissions as the API spells them, in bit order.
var names = []struct {
	bit  Set
	name string
}{
	{Administrator, "administrator"},
	{ManageServer, "manageServer"},
	{ManageRoles, "manageRoles"},
	{ManageChannels, "manageChannels"},
	{CreateChannels, "createChannels"},
	{KickMembers, "kickMembers"},
	{BanMembers, "banMembers"},
	{ManageMessages, "manageMessages"},
	{CreateInvite, "createInvite"},
	{AttachFiles, "attachFiles"},
	{ViewChannel, "viewChannel"},
	{SendMessages, "sendMessages"},
	{Speak, "speak"},
}

// Names spells the set for the API, in a stable order.
func (s Set) Names() []string {
	out := make([]string, 0, len(names))
	for _, entry := range names {
		if s.Has(entry.bit) {
			out = append(out, entry.name)
		}
	}
	return out
}

// Parse reads names from the API; an unknown name is an error, not a no-op,
// so a typo never silently grants less than asked.
func Parse(list []string) (Set, bool) {
	var set Set
	for _, name := range list {
		found := false
		for _, entry := range names {
			if entry.name == name {
				set |= entry.bit
				found = true
				break
			}
		}
		if !found {
			return 0, false
		}
	}
	return set, true
}
