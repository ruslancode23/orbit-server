// Package conversations owns direct messages and small group chats: a channel
// without a server, whose participants are listed instead of inherited.
// Messages, attachments and read cursors work for it through the same code.
package conversations

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	// ErrNotGroup means a group-only operation was asked of a direct dialogue.
	// A dialogue cannot turn into a group; a client creates a new one instead.
	ErrNotGroup = errors.New("conversations: a direct dialogue cannot become a group")
	// ErrInvalidName means the group title is empty or longer than its limit.
	ErrInvalidName = errors.New("conversations: a group name must be 1 to 100 characters")
	// ErrInvalidCursor means the page cursor was not one this server issued.
	ErrInvalidCursor = errors.New("conversations: invalid page cursor")
)

const (
	// defaultPageSize is one screen of a sidebar; maxPageSize caps what a client
	// may ask for in one request.
	defaultPageSize = 20
	maxPageSize     = 100
	// maxNameLength is the group title limit the API schema states too.
	maxNameLength = 100
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

// MessageKind is what a message in a conversation is. A system line is written
// by the server when the group changes; it takes a seq and an author like any
// message, but nobody typed it and it mentions nobody.
type MessageKind string

const (
	MessageKindUser   MessageKind = "user"
	MessageKindSystem MessageKind = "system"
)

// SystemType says what a system line reports. The client renders its own
// language from this; Content carries a readable fallback.
type SystemType string

const (
	SystemMemberAdded         SystemType = "member_added"
	SystemMemberRemoved       SystemType = "member_removed"
	SystemConversationRenamed SystemType = "conversation_renamed"
)

// SystemMessage is the structured part of a system line.
type SystemMessage struct {
	Type     SystemType
	TargetID *uuid.UUID
	Name     string
}

// SystemEvent is what a group operation asks internal/messages to record. The
// conversations module builds the readable text because it already has the
// profiles; the messages module owns the row, the seq and the live event.
type SystemEvent struct {
	ActorID  uuid.UUID
	Type     SystemType
	TargetID *uuid.UUID
	Name     string
	Content  string
}

// LastMessage is the newest message of a conversation, shortened for the list.
type LastMessage struct {
	ID      uuid.UUID
	Seq     int64
	Author  *profile.Public
	Content string
	Kind    MessageKind
	System  *SystemMessage
	// CreatedAt is when the message was sent.
	CreatedAt time.Time
	DeletedAt *time.Time
}

type Conversation struct {
	ID        uuid.UUID
	Kind      Kind
	Name      string
	Members   []Member
	CreatedAt time.Time
	// The fields below are filled by List, the sidebar summary. A single
	// conversation (Get, Start) leaves them zero.
	LastMessage *LastMessage
	LastReadSeq int64
	Unread      int
	Mentions    int
}

type Member struct {
	// Public is the same profile the rest of the API returns, filled in by
	// internal/profile in one batch per conversation list.
	profile.Public
	JoinedAt time.Time
}

// UnreadSummary is the badge over every ordinary conversation of one account.
type UnreadSummary struct {
	Unread   int
	Mentions int
}

// Cursor is the last row of a page: the ordering key of the conversation list.
// The seq orders it, the creation time and the id break ties between
// conversations that never had a message, so the boundary is stable.
type Cursor struct {
	Seq       int64
	CreatedAt time.Time
	ID        uuid.UUID
}

// Page is one page of the sidebar and the cursor for the next one.
type Page struct {
	Conversations []Conversation
	NextCursor    string
}

type Store interface {
	// Create makes the conversation and puts everyone in it, in one transaction.
	Create(ctx context.Context, kind Kind, directKey string, memberIDs []uuid.UUID) (Conversation, error)
	// Direct finds the pair's conversation, or ErrNotFound.
	Direct(ctx context.Context, directKey string) (Conversation, error)
	// Secret finds the secret chat of a pair of devices, or ErrNotFound.
	Secret(ctx context.Context, key string) (Conversation, error)
	Conversation(ctx context.Context, id uuid.UUID) (Conversation, error)
	// List is one page of the caller's ordinary conversations with their newest
	// message, unread counts and participants, loaded without a query per row.
	List(ctx context.Context, userID uuid.UUID, before *Cursor, limit int) ([]Conversation, error)
	// Unread sums the unread and mention counts over every visible conversation.
	Unread(ctx context.Context, userID uuid.UUID) (UnreadSummary, error)
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
	// Rename sets the title of a group.
	Rename(ctx context.Context, channelID uuid.UUID, name string) error
	// AddMembers inserts only the people who are new, provided the group stays
	// within maxMembers. It returns the ids that were actually added.
	AddMembers(ctx context.Context, channelID uuid.UUID, userIDs []uuid.UUID, maxMembers int) ([]uuid.UUID, error)
	// RemoveMember drops one participant; false means they were not in it.
	RemoveMember(ctx context.Context, channelID, userID uuid.UUID) (bool, error)
	// Hide marks the conversation hidden as of its newest message.
	Hide(ctx context.Context, channelID, userID uuid.UUID) error
	// Unhide shows a hidden conversation again.
	Unhide(ctx context.Context, channelID, userID uuid.UUID) error
}

// Publisher tells a person's other connections what happened. Optional: with
// nil, everything is stored and nothing arrives live.
type Publisher interface {
	PublishToUser(userID uuid.UUID, event events.Event)
}

// SystemMessages records a server-written line in a conversation feed. Optional:
// without it a group change is still applied, it just has no line in history.
type SystemMessages interface {
	RecordSystem(ctx context.Context, channelID uuid.UUID, event SystemEvent) error
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
	// system records group changes in the feed; optional.
	system SystemMessages
	// profiles is optional at construction; main wires it in. Without it a
	// participant carries only the id, which is still enough for access checks.
	profiles Profiles
}

func NewService(store Store, maxMembers int, privacy Privacy) *Service {
	return &Service{store: store, maxMembers: maxMembers, privacy: privacy}
}

// SetProfiles wires the profile module in after both exist.
func (s *Service) SetProfiles(profiles Profiles) { s.profiles = profiles }

// SetSystemMessages wires the feed in after both modules exist. A group change
// is applied whether or not a system line can be written afterwards.
func (s *Service) SetSystemMessages(system SystemMessages) { s.system = system }

// withProfiles fills every participant of every conversation in one call, so a
// list of ten dialogues does not cost ten profile reads. The author of the last
// message is filled from the same batch.
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
		if conversation.LastMessage != nil && conversation.LastMessage.Author != nil {
			id := conversation.LastMessage.Author.ID
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
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
		if conversation.LastMessage != nil && conversation.LastMessage.Author != nil {
			if loaded, ok := profiles[conversation.LastMessage.Author.ID]; ok {
				author := loaded
				conversation.LastMessage.Author = &author
			}
		}
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
// click. The second result says whether it was created, which is what separates
// 201 from 200.
func (s *Service) Start(ctx context.Context, callerID uuid.UUID, withIDs []uuid.UUID) (Conversation, bool, error) {
	members := unique(append([]uuid.UUID{callerID}, withIDs...))
	if len(members) < 2 {
		return Conversation{}, false, ErrNoMembers
	}
	if len(members) > s.maxMembers {
		return Conversation{}, false, ErrTooManyMembers
	}
	// The rules are required: a service built without them must refuse rather
	// than open a dialogue to anyone.
	if s.privacy == nil {
		return Conversation{}, false, ErrNotFound
	}

	known, err := s.store.KnownUsers(ctx, members)
	if err != nil {
		return Conversation{}, false, err
	}
	for _, id := range members {
		if !known[id] {
			// Not "no such account": a refusal must look the same.
			return Conversation{}, false, ErrNotFound
		}
	}

	kind := KindGroup
	directKey := ""
	if len(members) == 2 {
		kind = KindDirect
		directKey = pairKey(members)

		allowed, err := s.privacy.CanStartDirect(ctx, callerID, members[1])
		if err != nil {
			return Conversation{}, false, err
		}
		if !allowed {
			return Conversation{}, false, ErrNotFound
		}

		existing, err := s.store.Direct(ctx, directKey)
		switch {
		case err == nil:
			found, err := s.one(ctx, existing)
			return found, false, err
		case !errors.Is(err, ErrNotFound):
			return Conversation{}, false, err
		}
	} else {
		allowed, err := s.privacy.CanStartGroup(ctx, callerID, members[1:])
		if err != nil {
			return Conversation{}, false, err
		}
		if !allowed {
			return Conversation{}, false, ErrNotFound
		}
	}

	// Only a conversation that is really new spends a token: reopening the
	// dialogue a pair already has is not what the limit guards against.
	if s.limiter != nil {
		allowed, retryAfter := s.limiter.Allow(callerID)
		if !allowed {
			return Conversation{}, false, &RateLimitedError{RetryAfter: retryAfter}
		}
	}

	created, err := s.store.Create(ctx, kind, directKey, members)
	if err != nil {
		return Conversation{}, false, err
	}
	created, err = s.one(ctx, created)
	if err != nil {
		return Conversation{}, false, err
	}
	s.publish(events.KindConversationCreated, created, memberIDs(created))
	return created, true, nil
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

// publish tells each recipient's own connections what happened.
func (s *Service) publish(kind events.Kind, conversation Conversation, recipients []uuid.UUID) {
	if s.publisher == nil || len(recipients) == 0 {
		return
	}
	payload := toAPIConversation(conversation)
	seen := make(map[uuid.UUID]bool, len(recipients))
	for _, memberID := range recipients {
		if seen[memberID] {
			continue
		}
		seen[memberID] = true
		s.publisher.PublishToUser(memberID, events.Event{
			Kind:      kind,
			ChannelID: conversation.ID,
			Payload:   payload,
		})
	}
}

// recordSystem writes a line about a group change. A failed line must not undo
// the change itself: the group is already renamed, extended or left.
func (s *Service) recordSystem(ctx context.Context, channelID uuid.UUID, event SystemEvent) {
	if s.system == nil {
		return
	}
	_ = s.system.RecordSystem(ctx, channelID, event)
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

// List is one page of the caller's conversations, the liveliest first. A hidden
// conversation comes back by itself once it gets a message newer than the
// moment it was hidden; the store's query applies that rule.
func (s *Service) List(ctx context.Context, callerID uuid.UUID, rawCursor string, limit int) (Page, error) {
	switch {
	case limit <= 0:
		limit = defaultPageSize
	case limit > maxPageSize:
		limit = maxPageSize
	}
	before, err := decodeCursor(rawCursor)
	if err != nil {
		return Page{}, err
	}

	// One extra row tells whether another page exists without counting.
	list, err := s.store.List(ctx, callerID, before, limit+1)
	if err != nil {
		return Page{}, err
	}
	next := ""
	if len(list) > limit {
		list = list[:limit]
		next = encodeCursor(list[len(list)-1])
	}
	list, err = s.withProfiles(ctx, list)
	if err != nil {
		return Page{}, err
	}
	return Page{Conversations: list, NextCursor: next}, nil
}

// Unread is the badge over every ordinary conversation: hidden ones are left
// out exactly as the list leaves them out, and secret chats have no cursor.
func (s *Service) Unread(ctx context.Context, callerID uuid.UUID) (UnreadSummary, error) {
	return s.store.Unread(ctx, callerID)
}

// Rename sets a group title. Any member may; a dialogue has no name to change,
// and a secret chat is not reached through this endpoint at all.
func (s *Service) Rename(ctx context.Context, id, callerID uuid.UUID, name string) (Conversation, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > maxNameLength {
		return Conversation{}, ErrInvalidName
	}
	conversation, err := s.groupFor(ctx, id, callerID)
	if err != nil {
		return Conversation{}, err
	}
	if conversation.Name == name {
		return conversation, nil
	}
	if err := s.store.Rename(ctx, id, name); err != nil {
		return Conversation{}, err
	}
	reloaded, err := s.store.Conversation(ctx, id)
	if err != nil {
		return Conversation{}, err
	}
	updated, err := s.one(ctx, reloaded)
	if err != nil {
		return Conversation{}, err
	}
	s.publish(events.KindConversationUpdated, updated, memberIDs(updated))
	s.recordSystem(ctx, id, SystemEvent{
		ActorID: callerID,
		Type:    SystemConversationRenamed,
		Name:    name,
		Content: memberName(conversation, callerID) + " renamed the group to " + name,
	})
	return updated, nil
}

// AddMembers puts more people into a group. Any member may add; the group limit
// counts everyone already there. A direct dialogue refuses instead of turning
// into a group, so the client creates a new one.
func (s *Service) AddMembers(ctx context.Context, id, callerID uuid.UUID, userIDs []uuid.UUID) (Conversation, error) {
	conversation, err := s.groupFor(ctx, id, callerID)
	if err != nil {
		return Conversation{}, err
	}
	add := unique(userIDs)
	if len(add) == 0 {
		return Conversation{}, ErrNoMembers
	}
	known, err := s.store.KnownUsers(ctx, add)
	if err != nil {
		return Conversation{}, err
	}
	for _, userID := range add {
		if !known[userID] {
			return Conversation{}, ErrNotFound
		}
	}
	// Adding somebody is the same act as starting a group with them: a guest may
	// not, and nobody may add an account blocked either way.
	if s.privacy == nil {
		return Conversation{}, ErrNotFound
	}
	allowed, err := s.privacy.CanStartGroup(ctx, callerID, add)
	if err != nil {
		return Conversation{}, err
	}
	if !allowed {
		return Conversation{}, ErrNotFound
	}

	added, err := s.store.AddMembers(ctx, id, add, s.maxMembers)
	if err != nil {
		return Conversation{}, err
	}
	reloaded, err := s.store.Conversation(ctx, id)
	if err != nil {
		return Conversation{}, err
	}
	updated, err := s.one(ctx, reloaded)
	if err != nil {
		return Conversation{}, err
	}
	if len(added) == 0 {
		return updated, nil
	}

	// The people who were already there see the group change; the new ones see a
	// conversation they have just joined.
	old := make([]uuid.UUID, 0, len(updated.Members))
	isNew := make(map[uuid.UUID]bool, len(added))
	for _, userID := range added {
		isNew[userID] = true
	}
	for _, member := range updated.Members {
		if !isNew[member.ID] {
			old = append(old, member.ID)
		}
	}
	s.publish(events.KindConversationUpdated, updated, old)
	s.publish(events.KindConversationCreated, updated, added)

	for _, userID := range added {
		target := userID
		s.recordSystem(ctx, id, SystemEvent{
			ActorID:  callerID,
			Type:     SystemMemberAdded,
			TargetID: &target,
			Content:  memberName(conversation, callerID) + " added " + memberName(updated, userID),
		})
	}
	return updated, nil
}

// Leave removes the caller from a group. Every way into the history, its files
// and the live stream goes through membership, so access ends with the row.
func (s *Service) Leave(ctx context.Context, id, callerID uuid.UUID) error {
	conversation, err := s.groupFor(ctx, id, callerID)
	if err != nil {
		return err
	}
	// Everyone who was in it hears about the change, the leaver included: their
	// other devices must drop a conversation that is no longer theirs.
	recipients := memberIDs(conversation)

	removed, err := s.store.RemoveMember(ctx, id, callerID)
	if err != nil {
		return err
	}
	if !removed {
		return ErrNotFound
	}
	reloaded, err := s.store.Conversation(ctx, id)
	if err != nil {
		return err
	}
	updated, err := s.one(ctx, reloaded)
	if err != nil {
		return err
	}
	s.publish(events.KindConversationUpdated, updated, recipients)
	s.recordSystem(ctx, id, SystemEvent{
		ActorID: callerID,
		Type:    SystemMemberRemoved,
		Content: memberName(conversation, callerID) + " left the group",
	})
	return nil
}

// SetHidden hides or shows a conversation for the caller alone. It is not a
// group change: nobody else is told and no system line is written.
func (s *Service) SetHidden(ctx context.Context, id, callerID uuid.UUID, hidden bool) error {
	conversation, err := s.store.Conversation(ctx, id)
	if err != nil {
		return err
	}
	if conversation.Kind == KindSecret {
		return ErrNotFound
	}
	inside, err := s.store.IsParticipant(ctx, id, callerID)
	if err != nil {
		return err
	}
	if !inside {
		return ErrNotFound
	}
	if hidden {
		err = s.store.Hide(ctx, id, callerID)
	} else {
		err = s.store.Unhide(ctx, id, callerID)
	}
	if err != nil {
		return err
	}
	if s.publisher != nil {
		s.publisher.PublishToUser(callerID, events.Event{
			Kind:      events.KindConversationHidden,
			ChannelID: id,
			Payload: map[string]any{
				"conversationId": id,
				"hidden":         hidden,
			},
		})
	}
	return nil
}

// groupFor reads a group the caller belongs to. A secret chat answers not found
// (it has its own endpoints), a dialogue answers ErrNotGroup, and an outsider
// gets the same answer as an unknown id.
func (s *Service) groupFor(ctx context.Context, id, callerID uuid.UUID) (Conversation, error) {
	conversation, err := s.store.Conversation(ctx, id)
	if err != nil {
		return Conversation{}, err
	}
	if conversation.Kind == KindSecret {
		return Conversation{}, ErrNotFound
	}
	inside, err := s.store.IsParticipant(ctx, id, callerID)
	if err != nil {
		return Conversation{}, err
	}
	if !inside {
		return Conversation{}, ErrNotFound
	}
	if conversation.Kind != KindGroup {
		return Conversation{}, ErrNotGroup
	}
	return s.one(ctx, conversation)
}

// memberIDs is the participant list of a conversation in memory.
func memberIDs(conversation Conversation) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(conversation.Members))
	for _, member := range conversation.Members {
		out = append(out, member.ID)
	}
	return out
}

// memberName is what the UI calls a person in a system line.
func memberName(conversation Conversation, userID uuid.UUID) string {
	for _, member := range conversation.Members {
		if member.ID != userID {
			continue
		}
		if member.DisplayName != "" {
			return member.DisplayName
		}
		if member.Nickname != "" {
			return member.Nickname
		}
	}
	return "Someone"
}

// cursorPayload is the JSON inside a page cursor. It is opaque to the client.
type cursorPayload struct {
	Seq       int64     `json:"s"`
	CreatedAt time.Time `json:"c"`
	ID        uuid.UUID `json:"i"`
}

// encodeCursor turns the last row of a page into an opaque cursor. It uses the
// ordering key of the list: seq, creation time and id.
func encodeCursor(conversation Conversation) string {
	payload := cursorPayload{Seq: 0, CreatedAt: conversation.CreatedAt, ID: conversation.ID}
	if conversation.LastMessage != nil {
		payload.Seq = conversation.LastMessage.Seq
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor parses what encodeCursor produced. An empty string is the first
// page. Anything else that does not decode is a client mistake, not a 500.
func decodeCursor(raw string) (*Cursor, error) {
	if raw == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var payload cursorPayload
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return nil, ErrInvalidCursor
	}
	if payload.CreatedAt.IsZero() || payload.ID == uuid.Nil {
		return nil, ErrInvalidCursor
	}
	return &Cursor{Seq: payload.Seq, CreatedAt: payload.CreatedAt, ID: payload.ID}, nil
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
