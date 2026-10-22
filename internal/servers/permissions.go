package servers

import (
	"context"

	"github.com/google/uuid"
)

// Permission is an action the old API let a server open to its members. Each
// maps to a permission of internal/perms (bitsOf); new code asks Require.
type Permission string

const (
	// PermissionInvite is creating invite links.
	PermissionInvite Permission = "invite"
	// PermissionCreateChannels is creating channels.
	PermissionCreateChannels Permission = "createChannels"
	// PermissionAttachFiles is uploading files and sending messages with them.
	PermissionAttachFiles Permission = "attachFiles"
)

// Permissions is what @everyone may do on one server, in the words of the old
// API: three flags read from and written to the @everyone role.
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
// the server: ErrNotMember for an outsider, ErrForbidden for a member whose
// roles do not allow it. The roles are read for this call, so a change binds
// the very next action of an already connected client.
func (s *Service) RequirePermission(ctx context.Context, workspaceID, userID uuid.UUID, permission Permission) (Role, error) {
	access, err := s.requireAny(ctx, workspaceID, userID, bitsOf(permission))
	if err != nil {
		return access.Role(), err
	}
	return access.Role(), nil
}
