package servers

import (
	"context"

	"github.com/google/uuid"
)

// Permission is an action a server may open to its members. The owner and
// admins may always do all of them.
type Permission string

const (
	// PermissionInvite is creating invite links.
	PermissionInvite Permission = "invite"
	// PermissionCreateChannels is creating channels.
	PermissionCreateChannels Permission = "createChannels"
	// PermissionAttachFiles is uploading files and sending messages with them.
	PermissionAttachFiles Permission = "attachFiles"
)

// Permissions is what the member role may do on one server.
type Permissions struct {
	Invite         bool
	CreateChannels bool
	AttachFiles    bool
}

// DefaultPermissions keeps what members could do before servers could choose:
// attach files, but neither invite people nor create channels.
func DefaultPermissions() Permissions {
	return Permissions{AttachFiles: true}
}

// Allows reports whether a role may take this action under these permissions.
func (p Permissions) Allows(role Role, permission Permission) bool {
	if role.Rank() >= RoleAdmin.Rank() {
		return true
	}
	if role != RoleMember {
		return false
	}
	switch permission {
	case PermissionInvite:
		return p.Invite
	case PermissionCreateChannels:
		return p.CreateChannels
	case PermissionAttachFiles:
		return p.AttachFiles
	default:
		return false
	}
}

// PermissionsUpdate carries only the flags the caller asked to change.
type PermissionsUpdate struct {
	Invite         *bool
	CreateChannels *bool
	AttachFiles    *bool
}

func (u PermissionsUpdate) empty() bool {
	return u.Invite == nil && u.CreateChannels == nil && u.AttachFiles == nil
}

// apply is p with the flags of u that were sent.
func (u PermissionsUpdate) apply(p Permissions) Permissions {
	if u.Invite != nil {
		p.Invite = *u.Invite
	}
	if u.CreateChannels != nil {
		p.CreateChannels = *u.CreateChannels
	}
	if u.AttachFiles != nil {
		p.AttachFiles = *u.AttachFiles
	}
	return p
}

// RequirePermission reports the caller's role when it may take this action on
// the server: ErrNotMember for an outsider, ErrForbidden for a member the server
// does not allow. Role and permissions are read for this call, together, so a
// change applies to the very next action of an already connected client.
func (s *Service) RequirePermission(ctx context.Context, workspaceID, userID uuid.UUID, permission Permission) (Role, error) {
	role, permissions, err := s.store.Access(ctx, workspaceID, userID)
	if err != nil {
		return "", err
	}
	if !permissions.Allows(role, permission) {
		return role, ErrForbidden
	}
	return role, nil
}
