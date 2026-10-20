// Package conversations owns direct messages and small group chats: a channel
// without a server, whose participants are listed instead of inherited.
// Messages, attachments and read cursors work for it through the same code.
package conversations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/profile"
	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

var (
	// ErrNotFound covers an unknown conversation, one the caller is not in, and
	// an account the caller may not talk to. One answer for all of them, so a
	// stranger cannot tell a refusal from an account that does not exist.
	ErrNotFound = errors.New("conversations: not found")
	// ErrTooManyMembers means the group is over this instance's limit.
	ErrTooManyMembers = errors.New("conversations: too many people for one group")
	// ErrNoMembers means nobody was named to talk to.
	ErrNoMembers = errors.New("conversations: name at least one other person")
	// ErrRateLimited means the account opened too many new conversations lately.
	ErrRateLimited = errors.New("conversations: too many new conversations")
)

// RateLimitedError carries how long the caller should wait; the handler puts it
// in Retry-After. It unwraps to ErrRateLimited, so errors.Is stays simple.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }

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
	// Public is the same profile the rest of the API returns, filled in by
	// internal/profile in one batch per conversation list.
	profile.Public
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
	// SharesConversation reports a common conversation: profile visibility asks
	// it instead of reading channel_members itself.
	SharesConversation(ctx context.Context, a, b uuid.UUID) (bool, error)
	// Peers is everyone who shares a conversation with this account: the
	// personal audience of user.updated.
	Peers(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

// Publisher tells a person's other connections what happened. Optional: with
// nil, everything is stored and nothing arrives live.
type Publisher interface {
	PublishToUser(userID uuid.UUID, event events.Event)
}

// Privacy decides whether the caller may open a conversation with someone. The
// internal/privacy module owns the rules; this package only asks, so there is
// one implementation of "who can write to whom".
type Privacy interface {
	// CanStartDirect covers the recipient's policy, a common server, a block in
	// either direction and the caller being a guest.
	CanStartDirect(ctx context.Context, fromID, toID uuid.UUID) (bool, error)
	// CanStartGroup covers the caller being a guest and blocked accounts in the
	// group. The dm_policy is about dialogues, so it does not apply here.
	CanStartGroup(ctx context.Context, callerID uuid.UUID, memberIDs []uuid.UUID) (bool, error)
}

// Profiles is the public part of an account, owned by internal/profile. One
// call fills every participant of a conversation list.
type Profiles interface {
	Profiles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]profile.Public, error)
}

type Service struct {
	store Store
	// maxMembers caps a group chat; a pair is always two.
	maxMembers int
	// privacy is required: without it a conversation would open to anyone.
	privacy Privacy
	// limiter caps new conversations per account; nil means no cap.
	limiter *ratelimit.Limiter
	// publisher is optional; see Publisher.
	publisher Publisher
	// profiles is optional at construction; main wires it in. Without it a
	// participant carries only the id, which is still enough for access checks.
	profiles Profiles
}

func NewService(store Store, maxMembers int, privacy Privacy) *Service {
	return &Service{store: store, maxMembers: maxMembers, privacy: privacy}
}

// SetProfiles wires the profile module in after both exist.
func (s *Service) SetProfiles(profiles Profiles) { s.profiles = profiles }

// withProfiles fills every participant of every conversation in one call, so a
// list of ten dialogues does not cost ten profile reads.
func (s *Service) withProfiles(ctx context.Context, conversations []Conversation) ([]Conversation, error) {
	if s.profiles == nil || len(conversations) == 0 {
		return conversations, nil
	}
	ids := make([]uuid.UUID, 0)
	seen := make(map[uuid.UUID]bool)
	for _, conversation := range conversations {
		for _, member := range conversation.Members {
			if !seen[member.ID] {
				seen[member.ID] = true
				ids = append(ids, member.ID)
			}
		}
	}
	if len(ids) == 0 {
		return conversations, nil
	}
	profiles, err := s.profiles.Profiles(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("load conversation profiles: %w", err)
	}
	out := make([]Conversation, 0, len(conversations))
	for _, conversation := range conversations {
		members := make([]Member, 0, len(conversation.Members))
		for _, member := range conversation.Members {
			if loaded, ok := profiles[member.ID]; ok {
				member.Public = loaded
			}
			members = append(members, member)
		}
		conversation.Members = members
		out = append(out, conversation)
	}
	return out, nil
}

// one fills the participants of a single conversation.
func (s *Service) one(ctx context.Context, conversation Conversation) (Conversation, error) {
	list, err := s.withProfiles(ctx, []Conversation{conversation})
	if err != nil {
		return Conversation{}, err
	}
	return list[0], nil
}

// SetLimiter wires the per-account bucket in after construction. Without it a
// new conversation is not counted; production always sets one.
func (s *Service) SetLimiter(limiter *ratelimit.Limiter) { s.limiter = limiter }

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
	// The rules are required: a service built without them must refuse rather
	// than open a dialogue to anyone.
	if s.privacy == nil {
		return Conversation{}, ErrNotFound
	}

	known, err := s.store.KnownUsers(ctx, members)
	if err != nil {
		return Conversation{}, err
	}
	for _, id := range members {
		if !known[id] {
			// Not "no such account": a refusal must look the same.
			return Conversation{}, ErrNotFound
		}
	}

	kind := KindGroup
	directKey := ""
	if len(members) == 2 {
		kind = KindDirect
		directKey = pairKey(members)

		allowed, err := s.privacy.CanStartDirect(ctx, callerID, members[1])
		if err != nil {
			return Conversation{}, err
		}
		if !allowed {
			return Conversation{}, ErrNotFound
		}

		existing, err := s.store.Direct(ctx, directKey)
		switch {
		case err == nil:
			return s.one(ctx, existing)
		case !errors.Is(err, ErrNotFound):
			return Conversation{}, err
		}
	} else {
		allowed, err := s.privacy.CanStartGroup(ctx, callerID, members[1:])
		if err != nil {
			return Conversation{}, err
		}
		if !allowed {
			return Conversation{}, ErrNotFound
		}
	}

	// Only a conversation that is really new spends a token: reopening the
	// dialogue a pair already has is not what the limit guards against.
	if s.limiter != nil {
		allowed, retryAfter := s.limiter.Allow(callerID)
		if !allowed {
			return Conversation{}, &RateLimitedError{RetryAfter: retryAfter}
		}
	}

	created, err := s.store.Create(ctx, kind, directKey, members)
	if err != nil {
		return Conversation{}, err
	}
	created, err = s.one(ctx, created)
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
		found, err := s.one(ctx, existing)
		if err != nil {
			return Conversation{}, false, err
		}
		return found, false, nil
	case !errors.Is(err, ErrNotFound):
		return Conversation{}, false, err
	}

	created, err := s.store.Create(ctx, KindSecret, key, members)
	if err != nil {
		return Conversation{}, false, err
	}
	created, err = s.one(ctx, created)
	if err != nil {
		return Conversation{}, false, err
	}
	return created, true, nil
}

// MemberIDs is who is in a conversation, for publishing personal events.
func (s *Service) MemberIDs(ctx context.Context, channelID uuid.UUID) ([]uuid.UUID, error) {
	return s.store.MemberIDs(ctx, channelID)
}

// Kind is the sort of conversation a channel is. internal/messages asks, because
// a block freezes a direct dialogue but only marks the author in a group; a
// server channel is not a conversation and answers ErrNotFound.
func (s *Service) Kind(ctx context.Context, channelID uuid.UUID) (Kind, error) {
	conversation, err := s.store.Conversation(ctx, channelID)
	if err != nil {
		return "", err
	}
	return conversation.Kind, nil
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
	return s.one(ctx, conversation)
}

// List is the caller's conversations, the liveliest first.
func (s *Service) List(ctx context.Context, callerID uuid.UUID) ([]Conversation, error) {
	conversations, err := s.store.List(ctx, callerID)
	if err != nil {
		return nil, err
	}
	return s.withProfiles(ctx, conversations)
}

// SharesConversation reports a common conversation, secret chats included:
// internal/profile asks it before it shows a profile, so the rule stays in this
// module's SQL.
func (s *Service) SharesConversation(ctx context.Context, a, b uuid.UUID) (bool, error) {
	if a == b || a == uuid.Nil || b == uuid.Nil {
		return false, nil
	}
	shared, err := s.store.SharesConversation(ctx, a, b)
	if err != nil {
		return false, fmt.Errorf("check shared conversation: %w", err)
	}
	return shared, nil
}

// Peers is everyone who shares a conversation with this account. It is the
// personal audience of a user.updated event.
func (s *Service) Peers(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	peers, err := s.store.Peers(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list conversation peers: %w", err)
	}
	return peers, nil
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
