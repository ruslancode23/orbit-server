package servers

import (
	"context"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/perms"
)

// Access is everything the rules need about one account on one server: the
// roles of the server and the ones the account holds. Read for each action,
// so a new role or permission binds the very next request.
type Access struct {
	Server perms.Server
	Member perms.Member
}

// Base is what the account may do on the server before any channel.
func (a Access) Base() perms.Set { return a.Server.Base(a.Member) }

// Role is the account in the words of the old API: owner, admin when its
// roles grant administrator, member otherwise. Clients that do not read
// permissions yet still draw the right buttons for the people who had them.
func (a Access) Role() Role { return roleOf(a.Member.Owner, a.Base()) }

func roleOf(owner bool, base perms.Set) Role {
	switch {
	case owner:
		return RoleOwner
	case base.Has(perms.Administrator):
		return RoleAdmin
	default:
		return RoleMember
	}
}

// Outranks reports whether this account may act on the other: kick, ban,
// change roles.
func (a Access) Outranks(target perms.Member) bool { return a.Server.Outranks(a.Member, target) }

// Access reads the account's standing on the server; ErrNotMember for an
// outsider or an unknown server, the same answer on purpose.
func (s *Service) Access(ctx context.Context, workspaceID, userID uuid.UUID) (Access, error) {
	return s.store.Access(ctx, workspaceID, userID)
}

// Require is Access when the account has every permission in want, and
// ErrForbidden when it is a member without them.
func (s *Service) Require(ctx context.Context, workspaceID, userID uuid.UUID, want perms.Set) (Access, error) {
	access, err := s.store.Access(ctx, workspaceID, userID)
	if err != nil {
		return Access{}, err
	}
	if !access.Base().Has(want) {
		return access, ErrForbidden
	}
	return access, nil
}

// requireAny is Require for alternatives: any one of the permissions is enough.
func (s *Service) requireAny(ctx context.Context, workspaceID, userID uuid.UUID, anyOf perms.Set) (Access, error) {
	access, err := s.store.Access(ctx, workspaceID, userID)
	if err != nil {
		return Access{}, err
	}
	if access.Base()&anyOf == 0 {
		return access, ErrForbidden
	}
	return access, nil
}

// bitsOf is the permission behind each action a server could open to members
// in the old model; creating channels is also allowed by managing them.
func bitsOf(permission Permission) perms.Set {
	switch permission {
	case PermissionInvite:
		return perms.CreateInvite
	case PermissionCreateChannels:
		return perms.CreateChannels | perms.ManageChannels
	case PermissionAttachFiles:
		return perms.AttachFiles
	default:
		return 0
	}
}

// everyoneOf is what @everyone may do for the old «what members may do»
// flags: view, write and speak always, the rest as the flags say.
func (p Permissions) everyoneOf() perms.Set {
	set := perms.ViewChannel | perms.SendMessages | perms.Speak
	if p.Invite {
		set |= perms.CreateInvite
	}
	if p.CreateChannels {
		set |= perms.CreateChannels
	}
	if p.AttachFiles {
		set |= perms.AttachFiles
	}
	return set
}

// memberFlags are the bits of @everyone the old flags speak about.
const memberFlags = perms.CreateInvite | perms.CreateChannels | perms.AttachFiles

// permissionsOf reads the old flags back from what @everyone may do.
func permissionsOf(everyone perms.Set) Permissions {
	return Permissions{
		Invite:         everyone.Has(perms.CreateInvite),
		CreateChannels: everyone.Has(perms.CreateChannels),
		AttachFiles:    everyone.Has(perms.AttachFiles),
	}
}
