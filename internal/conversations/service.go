// Package conversations owns direct messages and small group chats: a channel
// without a server, whose participants are listed instead of inherited.
// Messages, attachments and read cursors work for it through the same code.
package conversations

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/events"
)

var (
	// ErrNotFound covers an unknown conversation and one the caller is not in.
	ErrNotFound = errors.New("conversations: not found")
	// ErrTooManyMembers means the group is over this instance's limit.
	ErrTooManyMembers = errors.New("conversations: too many people for one group")
	// ErrNoMembers means nobody was named to talk to.
	ErrNoMembers = errors.New("conversations: name at least one other person")
	// ErrUnknownMember means one of the named accounts does not exist.
	ErrUnknownMember = errors.New("conversations: no such account")
)

// Kind tells a pair apart from a group.
type Kind string

const (
	KindDirect Kind = "direct"
	KindGroup  Kind = "group"
	// KindSecret is a pair of devices whose messages only they can read. It is a
	// channel so that it has an id and participants, but the messages endpoints
	// refuse it: neither text nor keys ever reach the server.
	KindSecret Kind = "secret"
)

type Conversation struct {
	ID        uuid.UUID
	Kind      Kind
	Members   []Member
	CreatedAt time.Time
}

type Member struct {
	ID       uuid.UUID
	Nickname string
	IsGuest  bool
	JoinedAt time.Time
}

type Store interface {
	// Create makes the conversation and puts everyone in it, in one transaction.
	Create(ctx context.Context, kind Kind, directKey string, memberIDs []uuid.UUID) (Conversation, error)
	// Direct finds the pair's conversation, or ErrNotFound.
	Direct(ctx context.Context, directKey string) (Conversation, error)
	// Secret finds the secret chat of a pair of devices, or ErrNotFound.
	Secret(ctx context.Context, key string) (Conversation, error)
	Conversation(ctx context.Context, id uuid.UUID) (Conversation, error)
	List(ctx context.Context, userID uuid.UUID) ([]Conversation, error)
	IsParticipant(ctx context.Context, channelID, userID uuid.UUID) (bool, error)
	// MemberIDs is who belongs to a conversation: messages go to each of them
	// personally, because a conversation has no server to announce to.
	MemberIDs(ctx context.Context, channelID uuid.UUID) ([]uuid.UUID, error)
	// KnownUsers reports which of the ids are real accounts.
	KnownUsers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
}

// Publisher tells a person's other connections what happened. Optional: with
// nil, everything is stored and nothing arrives live.
type Publisher interface {
	PublishToUser(userID uuid.UUID, event events.Event)
}

type Service struct {
	store Store
	// maxMembers caps a group chat; a pair is always two.
	maxMembers int
	// publisher is optional; see Publisher.
	publisher Publisher
}

func NewService(store Store, maxMembers int) *Service {
	return &Service{store: store, maxMembers: maxMembers}
}

// SetPublisher wires the live stream in after construction; conversations and
// the gateway do not depend on each other's packages.
func (s *Service) SetPublisher(p Publisher) { s.publisher = p }

// Start opens a conversation with the named people, or returns the one that
// already exists for a pair: two people have one dialogue, not a new one per
// click.
func (s *Service) Start(ctx context.Context, callerID uuid.UUID, withIDs []uuid.UUID) (Conversation, error) {
	members := unique(append([]uuid.UUID{callerID}, withIDs...))
	if len(members) < 2 {
		return Conversation{}, ErrNoMembers
	}
	if len(members) > s.maxMembers {
		return Conversation{}, ErrTooManyMembers
	}

	known, err := s.store.KnownUsers(ctx, members)
	if err != nil {
		return Conversation{}, err
	}
	for _, id := range members {
		if !known[id] {
			return Conversation{}, ErrUnknownMember
		}
	}

	kind := KindGroup
	directKey := ""
	if len(members) == 2 {
		kind = KindDirect
		directKey = pairKey(members)

		existing, err := s.store.Direct(ctx, directKey)
		switch {
		case err == nil:
			return existing, nil
		case !errors.Is(err, ErrNotFound):
			return Conversation{}, err
		}
	}

	created, err := s.store.Create(ctx, kind, directKey, members)
	if err != nil {
		return Conversation{}, err
	}
	s.publish(events.KindConversationCreated, created)
	return created, nil
}

// EnsureSecret returns the secret chat of a pair of devices, creating it when it
// is the first time. The key names the pair of devices, so the same two devices
// always land in one chat, whichever side starts it.
func (s *Service) EnsureSecret(ctx context.Context, key string, memberIDs []uuid.UUID) (Conversation, bool, error) {
	members := unique(memberIDs)
	if len(members) != 2 {
		return Conversation{}, false, ErrNoMembers
	}

	// A secret chat is looked up by its own key; the direct one is not it.
	existing, err := s.store.Secret(ctx, key)
	switch {
	case err == nil:
		return existing, false, nil
	case !errors.Is(err, ErrNotFound):
		return Conversation{}, false, err
	}

	created, err := s.store.Create(ctx, KindSecret, key, members)
	if err != nil {
		return Conversation{}, false, err
	}
	return created, true, nil
}

// MemberIDs is who is in a conversation, for publishing personal events.
func (s *Service) MemberIDs(ctx context.Context, channelID uuid.UUID) ([]uuid.UUID, error) {
	return s.store.MemberIDs(ctx, channelID)
}

// publish tells each participant's own connections about a new conversation.
func (s *Service) publish(kind events.Kind, conversation Conversation) {
	if s.publisher == nil {
		return
	}
	payload := toAPIConversation(conversation)
	for _, member := range conversation.Members {
		s.publisher.PublishToUser(member.ID, events.Event{
			Kind:      kind,
			ChannelID: conversation.ID,
			Payload:   payload,
		})
	}
}

// Get is one conversation the caller is in.
func (s *Service) Get(ctx context.Context, id, callerID uuid.UUID) (Conversation, error) {
	inside, err := s.store.IsParticipant(ctx, id, callerID)
	if err != nil {
		return Conversation{}, err
	}
	if !inside {
		return Conversation{}, ErrNotFound
	}
	conversation, err := s.store.Conversation(ctx, id)
	if err != nil {
		return Conversation{}, err
	}
	// A secret chat is reached through its own endpoints, which check that the
	// caller's device takes part in it.
	if conversation.Kind == KindSecret {
		return Conversation{}, ErrNotFound
	}
	return conversation, nil
}

// List is the caller's conversations, the liveliest first.
func (s *Service) List(ctx context.Context, callerID uuid.UUID) ([]Conversation, error) {
	return s.store.List(ctx, callerID)
}

// IsParticipant answers internal/channels, which routes access to a channel
// without a server through this module.
func (s *Service) IsParticipant(ctx context.Context, channelID, userID uuid.UUID) (bool, error) {
	return s.store.IsParticipant(ctx, channelID, userID)
}

// pairKey is the sorted pair of ids, so (anna, bob) and (bob, anna) name one
// conversation and the unique index does the rest.
func pairKey(members []uuid.UUID) string {
	ids := make([]string, 0, len(members))
	for _, id := range members {
		ids = append(ids, id.String())
	}
	sort.Strings(ids)
	return strings.Join(ids, ":")
}

func unique(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if seen[id] || id == uuid.Nil {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
