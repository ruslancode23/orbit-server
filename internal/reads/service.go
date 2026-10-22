// Package reads owns how far each person has read: one cursor per channel,
// the unread and mention counts built from it, and keeping a person's devices
// in step (FR-04).
package reads

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

// ErrNotFound covers a channel or server the caller may not read, and one
// that does not exist.
var ErrNotFound = errors.New("reads: not found")

// Unread is the state of one channel for one account.
type Unread struct {
	ChannelID   uuid.UUID
	LastReadSeq int64
	// Unread counts messages after the cursor, the account's own excluded.
	Unread int
	// Mentions counts how many of those name this account.
	Mentions int
}

type Store interface {
	// Mark moves the cursor forward and returns the state of that channel.
	Mark(ctx context.Context, channelID, userID uuid.UUID, seq int64) (Unread, error)
	// Channel is the state of one channel for one account.
	Channel(ctx context.Context, channelID, userID uuid.UUID) (Unread, error)
	// Workspace is the state of every channel of a server, in one query.
	Workspace(ctx context.Context, workspaceID, userID uuid.UUID) ([]Unread, error)
}

// ChannelAccess is what this package needs from internal/channels.
type ChannelAccess interface {
	Get(ctx context.Context, channelID, callerID uuid.UUID) (channels.View, error)
	// Visible is the channels of a server the account may see.
	Visible(ctx context.Context, workspaceID, userID uuid.UUID) (map[uuid.UUID]bool, error)
}

// Members is what this package needs from internal/servers.
type Members interface {
	RequireAtLeast(ctx context.Context, workspaceID, userID uuid.UUID, least servers.Role) (servers.Role, error)
}

// Watchers tells the other devices of the same account that its cursor moved.
// Optional: with nil, the counters still work, they just arrive on refresh.
type Watchers interface {
	PublishToUser(userID uuid.UUID, event events.Event)
}

type Service struct {
	store    Store
	channels ChannelAccess
	members  Members
	watchers Watchers
}

func NewService(store Store, access ChannelAccess, members Members, watchers Watchers) *Service {
	return &Service{store: store, channels: access, members: members, watchers: watchers}
}

// Mark moves the cursor of one channel. The store keeps it moving forward only,
// so a device coming back from offline cannot make messages unread again.
func (s *Service) Mark(ctx context.Context, channelID, userID uuid.UUID, seq int64) (Unread, error) {
	view, err := s.channels.Get(ctx, channelID, userID)
	if err != nil {
		return Unread{}, mapAccessError(err)
	}
	if view.Channel.Kind == channels.KindSecret {
		// A secret chat has no read counters: the server does not know what was read.
		return Unread{}, ErrNotFound
	}
	if seq < 0 {
		seq = 0
	}

	state, err := s.store.Mark(ctx, channelID, userID, seq)
	if err != nil {
		return Unread{}, err
	}

	// The other devices of this account hear it at once; nobody else does.
	if s.watchers != nil {
		s.watchers.PublishToUser(userID, events.Event{
			Kind:        events.KindReadChanged,
			WorkspaceID: view.Channel.WorkspaceID,
			ChannelID:   channelID,
			Seq:         state.LastReadSeq,
			Payload: map[string]any{
				"channelId":   channelID,
				"lastReadSeq": state.LastReadSeq,
				"unread":      state.Unread,
				"mentions":    state.Mentions,
			},
		})
	}
	return state, nil
}

// Channel is where one channel stands for this account.
func (s *Service) Channel(ctx context.Context, channelID, userID uuid.UUID) (Unread, error) {
	view, err := s.channels.Get(ctx, channelID, userID)
	if err != nil {
		return Unread{}, mapAccessError(err)
	}
	if view.Channel.Kind == channels.KindSecret {
		return Unread{}, ErrNotFound
	}
	return s.store.Channel(ctx, channelID, userID)
}

// Workspace is the whole sidebar in one request.
func (s *Service) Workspace(ctx context.Context, workspaceID, userID uuid.UUID) ([]Unread, error) {
	if _, err := s.members.RequireAtLeast(ctx, workspaceID, userID, servers.RoleMember); err != nil {
		if errors.Is(err, servers.ErrNotMember) || errors.Is(err, servers.ErrForbidden) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	all, err := s.store.Workspace(ctx, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	// A channel hidden from this account counts for nothing, and is not named.
	visible, err := s.channels.Visible(ctx, workspaceID, userID)
	if err != nil {
		return nil, mapAccessError(err)
	}
	out := make([]Unread, 0, len(all))
	for _, unread := range all {
		if visible[unread.ChannelID] {
			out = append(out, unread)
		}
	}
	return out, nil
}

func mapAccessError(err error) error {
	if errors.Is(err, channels.ErrNotFound) || errors.Is(err, channels.ErrForbidden) {
		return ErrNotFound
	}
	return err
}
