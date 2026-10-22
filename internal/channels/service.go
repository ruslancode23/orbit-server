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

	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/perms"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

// Kind is the sort of channel: text has messages, voice is a LiveKit room.
type Kind string

const (
	KindText   Kind = "text"
	KindVoice  Kind = "voice"
	KindSecret Kind = "secret"
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
	Kind        Kind
	Name        string
	Topic       string
	Position    int
	// IsReadOnly is the old API's «only admins write»: @everyone may not write
	// or speak here. Read from the overrides, written as one.
	IsReadOnly bool
	// Overrides change what roles may do in this channel.
	Overrides []perms.Override
	CreatedAt time.Time
}

// canWrite reports whether these permissions let one post here. A voice
// channel has no messages at all, and a secret one carries only ciphertext
// envelopes, which have their own endpoints.
func canWrite(c Channel, set perms.Set) bool {
	if c.Kind == KindVoice || c.Kind == KindSecret {
		return false
	}
	return set.Has(perms.ViewChannel | perms.SendMessages)
}

// canSpeak reports whether these permissions let one publish audio and screen
// in a voice channel.
func canSpeak(c Channel, set perms.Set) bool {
	return c.Kind == KindVoice && set.Has(perms.ViewChannel|perms.Speak)
}

// NewChannel is what Store.Create writes.
type NewChannel struct {
	WorkspaceID uuid.UUID
	Kind        Kind
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
	// SetReadOnly turns the @everyone «no writing, no speaking» override on or off.
	SetReadOnly(ctx context.Context, channel Channel, readOnly bool) error
}

// Participants answers who is in a conversation — a channel without a server
// (task 2.10). Optional: without it, conversations are simply not reachable.
type Participants interface {
	IsParticipant(ctx context.Context, channelID, userID uuid.UUID) (bool, error)
}

// Authorizer is the part of internal/servers this package needs: the roles of
// a server and the caller's, read for this request. Declared by the consumer.
type Authorizer interface {
	Access(ctx context.Context, workspaceID, userID uuid.UUID) (servers.Access, error)
}

// Watchers tells the members of a server what changed in it (internal/servers
// fans it out to each member's own stream). Optional: with nil the API behaves
// the same and the other members see the change on their next load.
type Watchers interface {
	Announce(ctx context.Context, event events.Event, also ...uuid.UUID)
}

type Service struct {
	store    Store
	roles    Authorizer
	watchers Watchers
	// participants resolves access to conversations; see Participants.
	participants Participants
}

func NewService(store Store, roles Authorizer) *Service {
	return &Service{store: store, roles: roles}
}

// SetParticipants wires conversations in after construction: the two modules
// point at each other, so one of them is told about the other once both exist.
func (s *Service) SetParticipants(p Participants) { s.participants = p }

// SetWatchers wires in the live stream, so the other members of the server see
// a channel appear, change, move or go without reloading.
func (s *Service) SetWatchers(w Watchers) { s.watchers = w }

// announce tells the server that its channel list changed. The payload names
// the channel only: the channel as the API shows it carries canWrite, which
// depends on who reads it, so each client asks for its own view. A zero
// channelID means the whole list changed — a new order.
func (s *Service) announce(ctx context.Context, kind events.Kind, workspaceID, channelID uuid.UUID) {
	if s.watchers == nil {
		return
	}
	event := events.Event{Kind: kind, WorkspaceID: workspaceID, ChannelID: channelID}
	if channelID != uuid.Nil {
		event.Payload = map[string]any{"channelId": channelID}
	}
	s.watchers.Announce(ctx, event)
}

// View is a channel as one caller sees it: the channel, what the caller may do
// in it and, for the old API, whether they may write.
type View struct {
	Channel     Channel
	CanWrite    bool
	Permissions perms.Set
}

func view(channel Channel, set perms.Set) View {
	return View{Channel: channel, CanWrite: canWrite(channel, set), Permissions: set}
}

// List is the channels of a server the caller may see, with what they may do
// in each. A channel hidden from them is simply not there.
func (s *Service) List(ctx context.Context, workspaceID, callerID uuid.UUID) ([]View, error) {
	access, err := s.access(ctx, workspaceID, callerID)
	if err != nil {
		return nil, err
	}
	list, err := s.store.List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return visible(list, access), nil
}

func (s *Service) Get(ctx context.Context, channelID, callerID uuid.UUID) (View, error) {
	channel, set, err := s.channelFor(ctx, channelID, callerID, perms.ViewChannel)
	if err != nil {
		return View{}, err
	}
	return view(channel, set), nil
}

// Create adds a channel: createChannels or manageChannels. A read-only channel
// is a moderation tool, so making one takes manageChannels.
func (s *Service) Create(ctx context.Context, workspaceID, callerID uuid.UUID, c NewChannel) (View, error) {
	access, err := s.access(ctx, workspaceID, callerID)
	if err != nil {
		return View{}, err
	}
	base := access.Base()
	if base&(perms.CreateChannels|perms.ManageChannels) == 0 {
		return View{}, ErrForbidden
	}
	if c.IsReadOnly && !base.Has(perms.ManageChannels) {
		return View{}, ErrForbidden
	}
	name, err := cleanName(c.Name)
	if err != nil {
		return View{}, err
	}
	topic, err := cleanTopic(c.Topic)
	if err != nil {
		return View{}, err
	}

	kind, err := cleanKind(c.Kind)
	if err != nil {
		return View{}, err
	}

	created, err := s.store.Create(ctx, NewChannel{
		WorkspaceID: workspaceID,
		Kind:        kind,
		Name:        name,
		Topic:       topic,
	})
	if err != nil {
		return View{}, err
	}
	if c.IsReadOnly {
		if err := s.store.SetReadOnly(ctx, created, true); err != nil {
			return View{}, err
		}
		if created, err = s.store.Channel(ctx, created.ID); err != nil {
			return View{}, err
		}
	}
	s.announce(ctx, events.KindChannelChanged, workspaceID, created.ID)
	return view(created, inChannel(access, created)), nil
}

// Update renames a channel, changes its topic or makes it read-only:
// manageChannels in this channel.
func (s *Service) Update(ctx context.Context, channelID, callerID uuid.UUID, u Update) (View, error) {
	channel, _, err := s.channelFor(ctx, channelID, callerID, perms.ManageChannels)
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

	// The name may be taken: write it first, so a refusal leaves nothing half done.
	updated, err := s.store.Update(ctx, channelID, u)
	if err != nil {
		return View{}, err
	}
	if u.IsReadOnly != nil && *u.IsReadOnly != channel.IsReadOnly {
		if err := s.store.SetReadOnly(ctx, updated, *u.IsReadOnly); err != nil {
			return View{}, err
		}
		if updated, err = s.store.Channel(ctx, channelID); err != nil {
			return View{}, err
		}
	}
	access, err := s.access(ctx, updated.WorkspaceID, callerID)
	if err != nil {
		return View{}, err
	}
	s.announce(ctx, events.KindChannelChanged, updated.WorkspaceID, updated.ID)
	return view(updated, inChannel(access, updated)), nil
}

// Delete removes the channel; the database takes its messages and attachments
// with it, so nothing is left pointing at a channel that is gone.
func (s *Service) Delete(ctx context.Context, channelID, callerID uuid.UUID) error {
	channel, _, err := s.channelFor(ctx, channelID, callerID, perms.ManageChannels)
	if err != nil {
		return err
	}
	if err := s.store.Delete(ctx, channelID); err != nil {
		return err
	}
	s.announce(ctx, events.KindChannelDeleted, channel.WorkspaceID, channelID)
	return nil
}

// Reorder sets the order of the whole list: manageChannels. The new order must
// name every channel of the server exactly once: a partial list would leave
// positions half applied.
func (s *Service) Reorder(ctx context.Context, workspaceID, callerID uuid.UUID, ids []uuid.UUID) ([]View, error) {
	access, err := s.access(ctx, workspaceID, callerID)
	if err != nil {
		return nil, err
	}
	if !access.Base().Has(perms.ManageChannels) {
		return nil, ErrForbidden
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
	s.announce(ctx, events.KindChannelChanged, workspaceID, uuid.Nil)
	list, err := s.store.List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return visible(list, access), nil
}

// Lookup returns a channel without checking who asks. Internal modules use it
// after they have checked access themselves (a webhook has no session).
func (s *Service) Lookup(ctx context.Context, channelID uuid.UUID) (Channel, error) {
	return s.store.Channel(ctx, channelID)
}

// ListVoice returns the voice channels of a server, in their sidebar order.
func (s *Service) ListVoice(ctx context.Context, workspaceID uuid.UUID) ([]Channel, error) {
	list, err := s.store.List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	voice := make([]Channel, 0, len(list))
	for _, channel := range list {
		if channel.Kind == KindVoice {
			voice = append(voice, channel)
		}
	}
	return voice, nil
}

// VoiceAccess returns whether the caller may speak in this voice channel. The
// channel must be a voice channel the caller may see: anything else is
// ErrNotFound, the same answer as a channel that does not exist.
func (s *Service) VoiceAccess(ctx context.Context, channelID, callerID uuid.UUID) (bool, error) {
	channel, set, err := s.channelFor(ctx, channelID, callerID, perms.ViewChannel)
	if err != nil {
		return false, err
	}
	if channel.Kind != KindVoice {
		return false, ErrNotFound
	}
	return canSpeak(channel, set), nil
}

// MayWrite answers "can this account post in this channel" for other modules
// (messages, the event gateway): one place for the rule.
func (s *Service) MayWrite(ctx context.Context, channelID, callerID uuid.UUID) (Channel, error) {
	channel, set, err := s.channelFor(ctx, channelID, callerID, perms.ViewChannel)
	if err != nil {
		return Channel{}, err
	}
	if !canWrite(channel, set) {
		return Channel{}, ErrForbidden
	}
	return channel, nil
}

// Permissions is what the caller may do in this channel, for other modules
// with rules of their own: deleting others' messages, attaching files.
// ErrNotFound when the caller may not see it.
func (s *Service) Permissions(ctx context.Context, channelID, callerID uuid.UUID) (Channel, perms.Set, error) {
	return s.channelFor(ctx, channelID, callerID, perms.ViewChannel)
}

// Visible reports which channels of a server the account may see, for the
// readers of a whole server at once: unread counts, the live stream.
func (s *Service) Visible(ctx context.Context, workspaceID, userID uuid.UUID) (map[uuid.UUID]bool, error) {
	access, err := s.access(ctx, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	list, err := s.store.List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(list))
	for _, channel := range list {
		if inChannel(access, channel).Has(perms.ViewChannel) {
			out[channel.ID] = true
		}
	}
	return out, nil
}

// conversationPermissions are what every participant may do in a conversation:
// its people are equals, and nobody manages it.
const conversationPermissions = perms.ViewChannel | perms.SendMessages | perms.AttachFiles

// channelFor reads the channel and checks that the caller may do `want` in it.
// A channel they may not see is ErrNotFound, never ErrForbidden: whether a
// hidden channel exists is nobody's business.
func (s *Service) channelFor(ctx context.Context, channelID, callerID uuid.UUID, want perms.Set) (Channel, perms.Set, error) {
	channel, err := s.store.Channel(ctx, channelID)
	if err != nil {
		return Channel{}, 0, err
	}

	// A conversation belongs to no server: being in it is the whole rule.
	if channel.WorkspaceID == uuid.Nil {
		inside, err := s.isParticipant(ctx, channelID, callerID)
		if err != nil {
			return Channel{}, 0, err
		}
		if !inside {
			return Channel{}, 0, ErrNotFound
		}
		if !conversationPermissions.Has(want) {
			// Renaming or deleting a conversation is not a thing anyone may do.
			return Channel{}, 0, ErrForbidden
		}
		return channel, conversationPermissions, nil
	}

	access, err := s.access(ctx, channel.WorkspaceID, callerID)
	if err != nil {
		return Channel{}, 0, err
	}
	set := inChannel(access, channel)
	if !set.Has(perms.ViewChannel) {
		return Channel{}, 0, ErrNotFound
	}
	if !set.Has(want) {
		return Channel{}, 0, ErrForbidden
	}
	return channel, set, nil
}

func (s *Service) isParticipant(ctx context.Context, channelID, userID uuid.UUID) (bool, error) {
	if s.participants == nil {
		return false, nil
	}
	return s.participants.IsParticipant(ctx, channelID, userID)
}

// access reads the caller's standing on a server; an outsider must not be
// able to tell "no such server" from "not your server".
func (s *Service) access(ctx context.Context, workspaceID, callerID uuid.UUID) (servers.Access, error) {
	access, err := s.roles.Access(ctx, workspaceID, callerID)
	if errors.Is(err, servers.ErrNotMember) {
		return servers.Access{}, ErrNotFound
	}
	return access, err
}

func inChannel(access servers.Access, channel Channel) perms.Set {
	return access.Server.InChannel(access.Member, channel.Overrides)
}

// visible is the channels the caller may see, as they see them.
func visible(list []Channel, access servers.Access) []View {
	out := make([]View, 0, len(list))
	for _, channel := range list {
		set := inChannel(access, channel)
		if set.Has(perms.ViewChannel) {
			out = append(out, view(channel, set))
		}
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

// ErrInvalidKind means the request named a channel kind the API does not know.
var ErrInvalidKind = errors.New("channels: kind must be text or voice")

func cleanKind(kind Kind) (Kind, error) {
	switch kind {
	case "":
		return KindText, nil
	case KindText, KindVoice:
		return kind, nil
	default:
		return "", ErrInvalidKind
	}
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
