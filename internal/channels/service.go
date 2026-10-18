// Package channels owns the text channels of a server: creating, renaming,
// ordering, deleting and who may write in them. Reading a channel follows
// membership in its server; the rules live here, the checks in internal/servers.
package channels

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/servers"
)

var (
	// ErrNotFound covers a missing channel and one in a server the caller is not
	// in: the two are the same answer, or the API reveals what exists elsewhere.
	ErrNotFound = errors.New("channels: not found")
	// ErrForbidden means the caller is a member but their role is not enough.
	ErrForbidden = errors.New("channels: role is not enough")
	// ErrNameTaken means the server already has a channel with this name.
	ErrNameTaken = errors.New("channels: the server already has a channel with this name")
	// ErrInvalidName means the name is empty or too long.
	ErrInvalidName = errors.New("channels: name must be 1 to 64 characters")
	// ErrInvalidTopic means the topic is longer than the limit.
	ErrInvalidTopic = errors.New("channels: topic must be at most 500 characters")
	// ErrIncompleteOrder means the new order is not exactly the channels of the server.
	ErrIncompleteOrder = errors.New("channels: the order must list every channel of the server once")
)

const (
	maxNameLength  = 64
	maxTopicLength = 500
)

type Channel struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Name        string
	Topic       string
	Position    int
	IsReadOnly  bool
	CreatedAt   time.Time
}

// CanWrite reports whether a role may post in this channel. A read-only channel
// is for announcements: members read it, admins and the owner write.
func (c Channel) CanWrite(role servers.Role) bool {
	if !c.IsReadOnly {
		return true
	}
	return role == servers.RoleAdmin || role == servers.RoleOwner
}

// NewChannel is what Store.Create writes.
type NewChannel struct {
	WorkspaceID uuid.UUID
	Name        string
	Topic       string
	IsReadOnly  bool
}

// Update carries only what the caller asked to change.
type Update struct {
	Name       *string
	Topic      *string
	IsReadOnly *bool
}

type Store interface {
	// Create returns ErrNameTaken when the server already has that name.
	Create(ctx context.Context, c NewChannel) (Channel, error)
	// Channel returns ErrNotFound when there is no such channel.
	Channel(ctx context.Context, id uuid.UUID) (Channel, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]Channel, error)
	Update(ctx context.Context, id uuid.UUID, u Update) (Channel, error)
	Delete(ctx context.Context, id uuid.UUID) error
	// Reorder writes the positions of every listed channel in one transaction.
	Reorder(ctx context.Context, workspaceID uuid.UUID, ids []uuid.UUID) error
}

// Authorizer is the part of internal/servers this package needs: the caller's
// role on a server, read for this request. Declared here, by the consumer.
type Authorizer interface {
	RequireAtLeast(ctx context.Context, workspaceID, userID uuid.UUID, least servers.Role) (servers.Role, error)
}

type Service struct {
	store Store
	roles Authorizer
}

func NewService(store Store, roles Authorizer) *Service {
	return &Service{store: store, roles: roles}
}

// View is a channel as one caller sees it: the channel plus whether they may write.
type View struct {
	Channel  Channel
	CanWrite bool
}

func (s *Service) List(ctx context.Context, workspaceID, callerID uuid.UUID) ([]View, error) {
	role, err := s.role(ctx, workspaceID, callerID, servers.RoleMember)
	if err != nil {
		return nil, err
	}
	list, err := s.store.List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return views(list, role), nil
}

func (s *Service) Get(ctx context.Context, channelID, callerID uuid.UUID) (View, error) {
	channel, role, err := s.channelFor(ctx, channelID, callerID, servers.RoleMember)
	if err != nil {
		return View{}, err
	}
	return View{Channel: channel, CanWrite: channel.CanWrite(role)}, nil
}

func (s *Service) Create(ctx context.Context, workspaceID, callerID uuid.UUID, c NewChannel) (View, error) {
	role, err := s.role(ctx, workspaceID, callerID, servers.RoleAdmin)
	if err != nil {
		return View{}, err
	}
	name, err := cleanName(c.Name)
	if err != nil {
		return View{}, err
	}
	topic, err := cleanTopic(c.Topic)
	if err != nil {
		return View{}, err
	}

	created, err := s.store.Create(ctx, NewChannel{
		WorkspaceID: workspaceID,
		Name:        name,
		Topic:       topic,
		IsReadOnly:  c.IsReadOnly,
	})
	if err != nil {
		return View{}, err
	}
	return View{Channel: created, CanWrite: created.CanWrite(role)}, nil
}

func (s *Service) Update(ctx context.Context, channelID, callerID uuid.UUID, u Update) (View, error) {
	_, role, err := s.channelFor(ctx, channelID, callerID, servers.RoleAdmin)
	if err != nil {
		return View{}, err
	}

	if u.Name != nil {
		name, err := cleanName(*u.Name)
		if err != nil {
			return View{}, err
		}
		u.Name = &name
	}
	if u.Topic != nil {
		topic, err := cleanTopic(*u.Topic)
		if err != nil {
			return View{}, err
		}
		u.Topic = &topic
	}

	updated, err := s.store.Update(ctx, channelID, u)
	if err != nil {
		return View{}, err
	}
	return View{Channel: updated, CanWrite: updated.CanWrite(role)}, nil
}

// Delete removes the channel; the database takes its messages and attachments
// with it, so nothing is left pointing at a channel that is gone.
func (s *Service) Delete(ctx context.Context, channelID, callerID uuid.UUID) error {
	if _, _, err := s.channelFor(ctx, channelID, callerID, servers.RoleAdmin); err != nil {
		return err
	}
	return s.store.Delete(ctx, channelID)
}

// Reorder sets the order of the whole list. The new order must name every channel
// of the server exactly once: a partial list would leave positions half applied.
func (s *Service) Reorder(ctx context.Context, workspaceID, callerID uuid.UUID, ids []uuid.UUID) ([]View, error) {
	role, err := s.role(ctx, workspaceID, callerID, servers.RoleAdmin)
	if err != nil {
		return nil, err
	}

	current, err := s.store.List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if !sameSet(current, ids) {
		return nil, ErrIncompleteOrder
	}

	if err := s.store.Reorder(ctx, workspaceID, ids); err != nil {
		return nil, err
	}
	list, err := s.store.List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return views(list, role), nil
}

// MayWrite answers "can this account post in this channel" for other modules
// (messages, the event gateway): one place for the rule.
func (s *Service) MayWrite(ctx context.Context, channelID, callerID uuid.UUID) (Channel, error) {
	channel, role, err := s.channelFor(ctx, channelID, callerID, servers.RoleMember)
	if err != nil {
		return Channel{}, err
	}
	if !channel.CanWrite(role) {
		return Channel{}, ErrForbidden
	}
	return channel, nil
}

// channelFor reads the channel and checks the caller's role on its server.
func (s *Service) channelFor(ctx context.Context, channelID, callerID uuid.UUID, least servers.Role) (Channel, servers.Role, error) {
	channel, err := s.store.Channel(ctx, channelID)
	if err != nil {
		return Channel{}, "", err
	}
	role, err := s.role(ctx, channel.WorkspaceID, callerID, least)
	if err != nil {
		return Channel{}, "", err
	}
	return channel, role, nil
}

// role maps the server's answers to this package's: an outsider must not be able
// to tell "no such server" from "not your server".
func (s *Service) role(ctx context.Context, workspaceID, callerID uuid.UUID, least servers.Role) (servers.Role, error) {
	role, err := s.roles.RequireAtLeast(ctx, workspaceID, callerID, least)
	switch {
	case errors.Is(err, servers.ErrNotMember):
		return "", ErrNotFound
	case errors.Is(err, servers.ErrForbidden):
		return role, ErrForbidden
	case err != nil:
		return "", err
	}
	return role, nil
}

func views(list []Channel, role servers.Role) []View {
	out := make([]View, 0, len(list))
	for _, c := range list {
		out = append(out, View{Channel: c, CanWrite: c.CanWrite(role)})
	}
	return out
}

// sameSet reports whether ids names every channel of the server exactly once.
func sameSet(current []Channel, ids []uuid.UUID) bool {
	if len(current) != len(ids) {
		return false
	}
	known := make(map[uuid.UUID]bool, len(current))
	for _, c := range current {
		known[c.ID] = true
	}
	seen := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		if !known[id] || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > maxNameLength {
		return "", ErrInvalidName
	}
	return name, nil
}

func cleanTopic(topic string) (string, error) {
	topic = strings.TrimSpace(topic)
	if len([]rune(topic)) > maxTopicLength {
		return "", ErrInvalidTopic
	}
	return topic, nil
}
