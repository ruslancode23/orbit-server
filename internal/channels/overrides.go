package channels

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/perms"
)

var (
	// ErrInvalidOverride means the override names a permission a channel
	// cannot change, or the same one in allow and deny.
	ErrInvalidOverride = errors.New("channels: an override may only allow or deny channel permissions, each once")
	// ErrUnknownRole means the role is not one of this channel's server.
	ErrUnknownRole = errors.New("channels: no such role on this server")
)

// Overrides is what roles may do differently in this channel: manageChannels.
func (s *Service) Overrides(ctx context.Context, channelID, callerID uuid.UUID) ([]perms.Override, error) {
	channel, _, err := s.serverChannelFor(ctx, channelID, callerID)
	if err != nil {
		return nil, err
	}
	return channel.Overrides, nil
}

// SetOverride changes what one role may do here. It takes manageChannels in
// the channel; the permissions must be ones a channel may change and the
// caller has; a role other than @everyone must lie below the caller's
// highest. Empty allow and deny remove the override.
func (s *Service) SetOverride(ctx context.Context, channelID, callerID uuid.UUID, override perms.Override) ([]perms.Override, error) {
	if override.Allow&override.Deny != 0 ||
		override.Allow&^perms.ChannelScoped != 0 || override.Deny&^perms.ChannelScoped != 0 {
		return nil, ErrInvalidOverride
	}
	channel, set, err := s.serverChannelFor(ctx, channelID, callerID)
	if err != nil {
		return nil, err
	}
	access, err := s.access(ctx, channel.WorkspaceID, callerID)
	if err != nil {
		return nil, err
	}
	role, ok := roleOf(access.Server, override.RoleID)
	if !ok {
		return nil, ErrUnknownRole
	}
	if !role.Default && !access.Server.MayManageRole(access.Member, role) {
		return nil, ErrForbidden
	}
	if !set.Has(override.Allow | override.Deny) {
		return nil, ErrForbidden
	}
	if err := s.store.SetOverride(ctx, channelID, override); err != nil {
		return nil, err
	}
	s.announce(ctx, events.KindChannelChanged, channel.WorkspaceID, channelID)
	updated, err := s.store.Channel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	return updated.Overrides, nil
}

// serverChannelFor is a server channel the caller may manage. A conversation
// has no roles, so it has no overrides either.
func (s *Service) serverChannelFor(ctx context.Context, channelID, callerID uuid.UUID) (Channel, perms.Set, error) {
	channel, set, err := s.channelFor(ctx, channelID, callerID, perms.ManageChannels)
	if err != nil {
		return Channel{}, 0, err
	}
	if channel.WorkspaceID == uuid.Nil {
		return Channel{}, 0, ErrNotFound
	}
	return channel, set, nil
}

func roleOf(server perms.Server, id uuid.UUID) (perms.Role, bool) {
	for _, role := range server.Roles {
		if role.ID == id {
			return role, true
		}
	}
	return perms.Role{}, false
}
