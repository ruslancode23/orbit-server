// Package notifysettings owns how loudly each server, channel and conversation
// speaks for one account. The rows live on the server, so a choice made in one
// device is the same on the next; a change reaches the account's other devices
// as `notification_settings.updated`.
//
// The effective level is what both the in-tab notifications and Web Push read:
// a channel inherits its server's level unless it has its own, a server channel
// falls back to mentions and a conversation to every message. A mute deadline
// silences the target until it passes, whatever the level says.
package notifysettings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

var (
	// ErrNotFound covers a missing target and one the caller may not see: the
	// two are the same answer, or the API would reveal hidden channels.
	ErrNotFound = errors.New("notifysettings: not found")
	// ErrInvalidScope means the scope is not workspace, channel or conversation.
	ErrInvalidScope = errors.New("notifysettings: unknown scope")
	// ErrInvalidLevel means the level is not all, mentions or none.
	ErrInvalidLevel = errors.New("notifysettings: unknown level")
	// ErrLimit means the account already keeps as many rows as it may.
	ErrLimit = errors.New("notifysettings: too many settings")
)

// MaxPerUser is how many rows one account may keep: a channel muted for a while
// is worth a row, a whole server of them is not.
const MaxPerUser = 500

// Scope is what a setting is about.
type Scope string

const (
	ScopeWorkspace    Scope = "workspace"
	ScopeChannel      Scope = "channel"
	ScopeConversation Scope = "conversation"
)

// Valid reports whether the scope is one this package knows.
func (s Scope) Valid() bool {
	return s == ScopeWorkspace || s == ScopeChannel || s == ScopeConversation
}

// Level is how much of a target's traffic reaches one account.
type Level string

const (
	// LevelAll speaks on every message.
	LevelAll Level = "all"
	// LevelMentions speaks only when the account is named.
	LevelMentions Level = "mentions"
	// LevelNone never speaks.
	LevelNone Level = "none"
)

// Valid reports whether the level is one this package knows.
func (l Level) Valid() bool {
	return l == LevelAll || l == LevelMentions || l == LevelNone
}

// Setting is one row: what one account chose for one target. A nil Level means
// "inherit"; a nil MutedUntil means no deadline.
type Setting struct {
	UserID     uuid.UUID
	Scope      Scope
	TargetID   uuid.UUID
	Level      *Level
	MutedUntil *time.Time
	UpdatedAt  time.Time
}

// Store is the persistence this package needs.
type Store interface {
	List(ctx context.Context, userID uuid.UUID) ([]Setting, error)
	// Get reports whether the account has a row for this target.
	Get(ctx context.Context, userID uuid.UUID, scope Scope, targetID uuid.UUID) (Setting, bool, error)
	Upsert(ctx context.Context, s Setting) (Setting, error)
	Delete(ctx context.Context, userID uuid.UUID, scope Scope, targetID uuid.UUID) (bool, error)
	Count(ctx context.Context, userID uuid.UUID) (int, error)
	// ForTargets returns the rows of every account for these target ids: one
	// query for a whole channel audience.
	ForTargets(ctx context.Context, targetIDs []uuid.UUID) ([]Setting, error)
	// ForUserTargets returns one account's rows for these target ids.
	ForUserTargets(ctx context.Context, userID uuid.UUID, targetIDs []uuid.UUID) ([]Setting, error)
}

// ChannelAccess is the part of internal/channels this package needs. Get checks
// that the caller may read the channel or is in the conversation, and answers
// ErrNotFound for a hidden one.
type ChannelAccess interface {
	Get(ctx context.Context, channelID, callerID uuid.UUID) (channels.View, error)
}

// WorkspaceAccess is the part of internal/servers this package needs: being a
// member is the whole rule for a server-wide setting.
type WorkspaceAccess interface {
	Authorize(ctx context.Context, workspaceID, userID uuid.UUID) (servers.Role, error)
}

// Publisher reaches the account's other devices. Optional: without it the API
// works the same and other devices learn on their next request.
type Publisher interface {
	PublishToUser(userID uuid.UUID, event events.Event)
}

// Service holds the rules. It knows nothing about HTTP.
type Service struct {
	store      Store
	channels   ChannelAccess
	workspaces WorkspaceAccess
	publisher  Publisher
	// now is the clock a mute deadline is compared with; tests replace it.
	now func() time.Time
}

// NewService returns a service over a store, the channels it checks access in
// and the servers whose membership it checks.
func NewService(store Store, channelAccess ChannelAccess, workspaces WorkspaceAccess) *Service {
	return &Service{store: store, channels: channelAccess, workspaces: workspaces, now: time.Now}
}

// SetPublisher wires the live stream in after construction.
func (s *Service) SetPublisher(p Publisher) { s.publisher = p }

// List returns every row of the account, ordered by scope and target.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Setting, error) {
	rows, err := s.store.List(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list notification settings: %w", err)
	}
	return rows, nil
}

// Set replaces the whole row of one target after checking that the caller may
// see it. A brand-new row counts against MaxPerUser; updating one does not.
func (s *Service) Set(ctx context.Context, userID uuid.UUID, scope Scope, targetID uuid.UUID, level *Level, mutedUntil *time.Time) (Setting, error) {
	if !scope.Valid() {
		return Setting{}, ErrInvalidScope
	}
	if level != nil && !level.Valid() {
		return Setting{}, ErrInvalidLevel
	}
	if err := s.authorize(ctx, userID, scope, targetID); err != nil {
		return Setting{}, err
	}
	_, exists, err := s.store.Get(ctx, userID, scope, targetID)
	if err != nil {
		return Setting{}, fmt.Errorf("read notification setting: %w", err)
	}
	if !exists {
		count, err := s.store.Count(ctx, userID)
		if err != nil {
			return Setting{}, fmt.Errorf("count notification settings: %w", err)
		}
		if count >= MaxPerUser {
			return Setting{}, ErrLimit
		}
	}
	stored, err := s.store.Upsert(ctx, Setting{
		UserID:     userID,
		Scope:      scope,
		TargetID:   targetID,
		Level:      level,
		MutedUntil: mutedUntil,
	})
	if err != nil {
		return Setting{}, fmt.Errorf("store notification setting: %w", err)
	}
	s.publish(userID, scope, targetID)
	return stored, nil
}

// Delete drops one row of the account. A row that is not there is ErrNotFound.
func (s *Service) Delete(ctx context.Context, userID uuid.UUID, scope Scope, targetID uuid.UUID) error {
	if !scope.Valid() {
		return ErrInvalidScope
	}
	deleted, err := s.store.Delete(ctx, userID, scope, targetID)
	if err != nil {
		return fmt.Errorf("delete notification setting: %w", err)
	}
	if !deleted {
		return ErrNotFound
	}
	s.publish(userID, scope, targetID)
	return nil
}

// Effective is how loudly this channel speaks to one account: its own level,
// then its server's, then the default; a live mute deadline silences it all.
func (s *Service) Effective(ctx context.Context, userID uuid.UUID, channel channels.Channel) (Level, error) {
	rows, err := s.store.ForUserTargets(ctx, userID, targets(channel))
	if err != nil {
		return "", fmt.Errorf("read notification levels: %w", err)
	}
	return evaluate(rows, channel, s.now()), nil
}

// Levels is Effective for a whole audience at once: it returns a level for
// every account that has a row about this channel or its server. An account
// that is not in the map never chose one and keeps the default — mentions in a
// server channel, every message in a conversation. The price is one query, not
// one per member.
func (s *Service) Levels(ctx context.Context, channel channels.Channel) (map[uuid.UUID]Level, error) {
	rows, err := s.store.ForTargets(ctx, targets(channel))
	if err != nil {
		return nil, fmt.Errorf("read notification levels: %w", err)
	}
	byUser := make(map[uuid.UUID][]Setting, len(rows))
	for _, row := range rows {
		byUser[row.UserID] = append(byUser[row.UserID], row)
	}
	out := make(map[uuid.UUID]Level, len(byUser))
	now := s.now()
	for userID, own := range byUser {
		out[userID] = evaluate(own, channel, now)
	}
	return out, nil
}

// targets are the rows that decide the level of one channel: the channel (or
// conversation) itself and, for a server channel, the server behind it.
func targets(channel channels.Channel) []uuid.UUID {
	if channel.WorkspaceID == uuid.Nil {
		return []uuid.UUID{channel.ID}
	}
	return []uuid.UUID{channel.ID, channel.WorkspaceID}
}

// evaluate applies the inheritance rules to the rows of one account.
func evaluate(rows []Setting, channel channels.Channel, now time.Time) Level {
	// A secret chat never notifies: its text does not even reach the server.
	if channel.Kind == channels.KindSecret {
		return LevelNone
	}
	scope := ScopeChannel
	inherited := LevelMentions
	if channel.WorkspaceID == uuid.Nil {
		scope = ScopeConversation
		inherited = LevelAll
	}

	var own, server *Setting
	var deadline time.Time
	for i := range rows {
		row := &rows[i]
		switch {
		case row.Scope == scope && row.TargetID == channel.ID:
			own = row
		case channel.WorkspaceID != uuid.Nil && row.Scope == ScopeWorkspace && row.TargetID == channel.WorkspaceID:
			server = row
		}
		if row.MutedUntil != nil && row.MutedUntil.After(deadline) {
			deadline = *row.MutedUntil
		}
	}

	// A deadline anywhere on the way silences the target; the latest one wins,
	// so muting a server covers its channels and a channel mute extends it.
	if deadline.After(now) {
		return LevelNone
	}
	if own != nil && own.Level != nil {
		return *own.Level
	}
	if server != nil && server.Level != nil {
		return *server.Level
	}
	return inherited
}

// authorize checks that the caller may see the target. A server needs the
// caller to be a member; a channel must be visible, and a conversation must be
// one the caller is in. Everything else is ErrNotFound, never ErrForbidden.
func (s *Service) authorize(ctx context.Context, userID uuid.UUID, scope Scope, targetID uuid.UUID) error {
	switch scope {
	case ScopeWorkspace:
		if _, err := s.workspaces.Authorize(ctx, targetID, userID); err != nil {
			if errors.Is(err, servers.ErrNotMember) {
				return ErrNotFound
			}
			return fmt.Errorf("authorize notification setting: %w", err)
		}
		return nil
	case ScopeChannel, ScopeConversation:
		view, err := s.channels.Get(ctx, targetID, userID)
		if err != nil {
			if errors.Is(err, channels.ErrNotFound) || errors.Is(err, channels.ErrForbidden) {
				return ErrNotFound
			}
			return fmt.Errorf("authorize notification setting: %w", err)
		}
		if view.Channel.Kind == channels.KindSecret {
			return ErrNotFound
		}
		conversation := view.Channel.WorkspaceID == uuid.Nil
		if scope == ScopeChannel && conversation {
			return ErrNotFound
		}
		if scope == ScopeConversation && !conversation {
			return ErrNotFound
		}
		return nil
	default:
		return ErrInvalidScope
	}
}

// publish tells the account's other devices that a row changed. They ask for
// the list again; the row itself is not trusted from an event.
func (s *Service) publish(userID uuid.UUID, scope Scope, targetID uuid.UUID) {
	if s.publisher == nil {
		return
	}
	s.publisher.PublishToUser(userID, events.Event{
		Kind:     events.KindNotificationSettingsUpdated,
		Personal: true,
		Payload:  map[string]any{"scope": string(scope), "targetId": targetID.String()},
	})
}
