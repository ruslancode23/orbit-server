// Package messages owns what people write in a channel: sending, history,
// edits, replies, mentions and reactions (FR-04). Who may read or write a
// channel is decided by internal/channels; this package asks it every time.
package messages

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/notifysettings"
	"github.com/ruslancode23/orbit-server/internal/perms"
	"github.com/ruslancode23/orbit-server/internal/profile"
	"github.com/ruslancode23/orbit-server/internal/push"
	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

var (
	// ErrNotFound covers a missing message or channel and one the caller may not
	// read: the same answer, so the API reveals nothing about other servers.
	ErrNotFound = errors.New("messages: not found")
	// ErrForbidden means the caller may read the channel but not do this.
	ErrForbidden = errors.New("messages: not allowed")
	// ErrAttachNotAllowed means the server does not let members attach files.
	ErrAttachNotAllowed = fmt.Errorf("%w: this server does not let members attach files", ErrForbidden)
	// ErrInvalidContent means the text is empty or too long.
	ErrInvalidContent = errors.New("messages: text must be 1 to 4000 characters")
	// ErrInvalidReply means the message replied to is in another channel or gone.
	ErrInvalidReply = errors.New("messages: the message replied to is not in this channel")
	// ErrInvalidEmoji means the reaction is empty or too long.
	ErrInvalidEmoji = errors.New("messages: a reaction must be 1 to 32 characters")
	// ErrRateLimited means the account started too many new dialogues lately.
	ErrRateLimited = errors.New("messages: too many new conversations")
	// ErrInvalidSearch means the search query is empty or too long.
	ErrInvalidSearch = errors.New("messages: the search query must be 1 to 200 characters")
	// ErrSearchRateLimited means the account searched too often lately.
	ErrSearchRateLimited = errors.New("messages: too many searches")
)

// RateLimitedError carries how long the caller should wait; the handler puts it
// in Retry-After. It unwraps to ErrRateLimited, so errors.Is stays simple.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }

const (
	maxContentLength = 4000
	maxEmojiLength   = 32
	defaultPageSize  = 50
	maxPageSize      = 100
)

// Author is who wrote a message, as the API shows them. It is the same public
// profile every other list returns, filled by internal/profile.
type Author = profile.Public

// MessageKind is what a message is: a person's text or a line the server wrote
// when the group changed.
type MessageKind string

const (
	MessageKindUser   MessageKind = "user"
	MessageKindSystem MessageKind = "system"
)

// System is the structured part of a server-written line. Its type lets a
// client render the line in its own language; Content is only a fallback.
type System struct {
	Type     conversations.SystemType
	TargetID *uuid.UUID
	Name     string
}

type Message struct {
	ID        uuid.UUID
	ChannelID uuid.UUID
	Seq       int64
	Kind      MessageKind
	// Author is nil when the account has been deleted; the message stays.
	Author    *Author
	Content   string
	System    *System
	ReplyToID *uuid.UUID
	Mentions  []uuid.UUID
	Reactions []Reaction
	// Attachments are the files this message carries.
	Attachments []Attachment
	CreatedAt   time.Time
	EditedAt    *time.Time
	DeletedAt   *time.Time
	// Blocked is for the viewer only: the author is blocked by them, so the
	// client collapses the message instead of showing it. It never leaves this
	// server as a property of the message itself.
	Blocked bool
}

// Attachment is a file on a message, as the messages module passes it along;
// internal/files owns what it means.
type Attachment struct {
	ID        uuid.UUID
	FileName  string
	MimeType  string
	SizeBytes int64
	Width     *int
	Height    *int
	CreatedAt time.Time
}

type Reaction struct {
	Emoji string
	Count int
	// Reacted says whether the caller is among those who reacted.
	Reacted bool
}

// NewMessage is one send.
type NewMessage struct {
	ChannelID       uuid.UUID
	AuthorID        uuid.UUID
	ClientMessageID string
	Content         string
	ReplyToID       *uuid.UUID
	Mentions        []uuid.UUID
	// AttachmentIDs are files uploaded beforehand that this message carries.
	AttachmentIDs []uuid.UUID
}

// NewSystemMessage is a group-change line the conversations module asks for.
// It has no mentions and no attachments: it is not somebody's text.
type NewSystemMessage struct {
	ChannelID uuid.UUID
	AuthorID  uuid.UUID
	Content   string
	Type      conversations.SystemType
	TargetID  *uuid.UUID
	Name      string
}

// Page is history, newest first.
type Page struct {
	Messages []Message
	// NextBefore is the cursor for the next page; nil at the start of the history.
	NextBefore *int64
}

// SearchQuery is one page of full-text search. ChannelIDs are the only channels
// the caller may see, decided by the service before the query runs.
type SearchQuery struct {
	Term string
	// ChannelIDs is the whole allowed set: visibility is not decided in SQL.
	ChannelIDs []uuid.UUID
	AuthorID   *uuid.UUID
	HasFiles   bool
	Before     *int64
	// ExcludedAuthors are accounts the caller blocked; their messages never surface.
	ExcludedAuthors []uuid.UUID
	Limit           int
}

// SearchHit is one matched message and the fragment to show.
type SearchHit struct {
	Message  Message
	Headline string
}

// SearchParams are the filters of one search request. At most one target names
// what to search: a channel, a conversation or a server.
type SearchParams struct {
	Query          string
	WorkspaceID    *uuid.UUID
	ChannelID      *uuid.UUID
	ConversationID *uuid.UUID
	AuthorID       *uuid.UUID
	HasFiles       bool
	// ConversationsOnly drops the server channels from a search without a
	// target: the personal-messages screen searches dialogues.
	ConversationsOnly bool
	Before            *int64
	Limit             int
}

// SearchResult is a hit with the channel it belongs to, ready for the API.
type SearchResult struct {
	Message     Message
	Headline    string
	ChannelName string
	WorkspaceID *uuid.UUID
}

// SearchPage is one page of results and the cursor for the next one.
type SearchPage struct {
	Results []SearchResult
	// NextCursor is the seq of the last result; nil on the last page.
	NextCursor *int64
}

type Store interface {
	// Create stores the message, or returns the one already stored for this
	// client id — the same send twice must not make two messages.
	Create(ctx context.Context, m NewMessage) (Message, bool, error)
	// CreateSystem stores a server-written line and returns it with its seq.
	CreateSystem(ctx context.Context, m NewSystemMessage) (Message, error)
	Message(ctx context.Context, id uuid.UUID) (Message, error)
	List(ctx context.Context, channelID uuid.UUID, before *int64, limit int, viewerID uuid.UUID) ([]Message, error)
	// Since returns messages after a seq, oldest first: the catch-up after a reconnect.
	Since(ctx context.Context, channelID uuid.UUID, afterSeq int64, limit int, viewerID uuid.UUID) ([]Message, error)
	Edit(ctx context.Context, id, authorID uuid.UUID, content string) (Message, error)
	Delete(ctx context.Context, id, authorID uuid.UUID) (Message, error)
	AddMentions(ctx context.Context, messageID uuid.UUID, userIDs []uuid.UUID) error
	AddReaction(ctx context.Context, messageID, userID uuid.UUID, emoji string) error
	RemoveReaction(ctx context.Context, messageID, userID uuid.UUID, emoji string) error
	// Load fills mentions and reactions of one message for this viewer.
	Load(ctx context.Context, m Message, viewerID uuid.UUID) (Message, error)
	// HasMessages reports whether the channel has any message yet: the first one
	// in a new dialogue is rate-limited like opening the dialogue itself.
	HasMessages(ctx context.Context, channelID uuid.UUID) (bool, error)
	// Search is full-text search over the given channels, newest first.
	Search(ctx context.Context, q SearchQuery) ([]SearchHit, error)
}

// ChannelAccess is the part of internal/channels this package needs.
type ChannelAccess interface {
	// MayWrite returns the channel when the caller may post in it.
	MayWrite(ctx context.Context, channelID, callerID uuid.UUID) (channels.Channel, error)
	// Get returns the channel when the caller may read it.
	Get(ctx context.Context, channelID, callerID uuid.UUID) (channels.View, error)
	// Visible is which channels of a server the caller may see, for search.
	Visible(ctx context.Context, workspaceID, userID uuid.UUID) (map[uuid.UUID]bool, error)
	// LookupMany names a page of channels in one query, after access was checked.
	LookupMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]channels.Channel, error)
}

// Publisher announces what happened to the clients watching the server. A nil
// publisher is allowed: the HTTP API works the same, nobody is just told live.
type Publisher interface {
	Publish(event events.Event)
	// PublishToUser reaches one account's connections, wherever they are: a
	// conversation has no server to announce to.
	PublishToUser(userID uuid.UUID, event events.Event)
}

// Notifier reaches people who have no Orbit tab open (internal/push). Optional:
// without it nobody away from the app hears about a message.
type Notifier interface {
	Notify(ctx context.Context, recipients []uuid.UUID, n push.Notification)
}

// NotificationLevels is how loudly one channel speaks to each account, as
// internal/notifysettings computes it. Optional: without it a server channel
// notifies its mentions and a conversation notifies everyone, as before.
type NotificationLevels interface {
	Levels(ctx context.Context, channel channels.Channel) (map[uuid.UUID]notifysettings.Level, error)
}

// Conversations tells who is in a conversation. Optional: without it, a
// conversation message is stored and simply not announced live.
type Conversations interface {
	MemberIDs(ctx context.Context, channelID uuid.UUID) ([]uuid.UUID, error)
	// Kind tells a pair from a group: a block freezes the first and only marks
	// the author in the second.
	Kind(ctx context.Context, channelID uuid.UUID) (conversations.Kind, error)
	// ChannelIDs is every ordinary conversation of an account, for the search scope.
	ChannelIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

// WorkspaceList lists the servers an account is in, so a search without a
// target can cover all of them.
type WorkspaceList interface {
	Servers(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

// Files binds uploaded files to the message that names them and loads the
// files of a page of messages.
type Files interface {
	// MayAttach returns ErrAttachNotAllowed when the caller may not attach files
	// in this channel, which it may already write in.
	MayAttach(ctx context.Context, channel channels.Channel, callerID uuid.UUID) error
	AttachAll(ctx context.Context, messageID, uploaderID uuid.UUID, ids []uuid.UUID) ([]Attachment, error)
	ByMessages(ctx context.Context, messageIDs []uuid.UUID) (map[uuid.UUID][]Attachment, error)
}

// Members answers whether an account is on a server, so a mention of an
// outsider is dropped instead of notifying someone who cannot read the channel.
type Members interface {
	IsMember(ctx context.Context, workspaceID, userID uuid.UUID) (bool, error)
}

// Privacy decides whom a reader blocked. internal/privacy owns the rules; this
// package only asks, and a nil Privacy leaves every message unmarked.
type Privacy interface {
	// BlockedEitherWay reports a block in either direction: a direct dialogue
	// with such a pair accepts no new messages from either side.
	BlockedEitherWay(ctx context.Context, a, b uuid.UUID) (bool, error)
	// BlockedIDs is whom this reader blocked, as a set.
	BlockedIDs(ctx context.Context, userID uuid.UUID) (map[uuid.UUID]bool, error)
}

// Profiles is the public part of an account, owned by internal/profile. One
// call fills every author of a page, so history never asks per message.
type Profiles interface {
	Profiles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]profile.Public, error)
}

type Service struct {
	store    Store
	channels ChannelAccess
	members  Members
	// files is optional: without it a message simply carries no attachments.
	files Files
	// publisher is optional; see Publisher.
	publisher Publisher
	// conversations is optional; see Conversations.
	conversations Conversations
	// privacy is optional: without it blocked authors stay visible.
	privacy Privacy
	// limiter caps the first message of a new dialogue; nil means no cap.
	limiter *ratelimit.Limiter
	// profiles is optional at construction; main wires it in. Without it an
	// author carries only the id, which is still enough to check authorship.
	profiles Profiles
	// notifier is optional; see Notifier.
	notifier Notifier
	// levels is optional; see NotificationLevels.
	levels NotificationLevels
	// workspaces is optional: without it a search without a target covers only
	// conversations.
	workspaces WorkspaceList
	// searchLimiter caps searches per account; nil means no cap.
	searchLimiter *ratelimit.Limiter
}

func NewService(store Store, access ChannelAccess, members Members, files Files, publisher Publisher) *Service {
	return &Service{store: store, channels: access, members: members, files: files, publisher: publisher}
}

// SetFiles wires the files module in after construction: a message carries
// files, and a file asks which channel its message is in, so one of the two
// has to be told about the other once both exist.
func (s *Service) SetFiles(f Files) { s.files = f }

// SetConversations wires the conversations module in after construction: a
// message in a conversation is published to its people, not to a server.
func (s *Service) SetConversations(c Conversations) { s.conversations = c }

// SetPrivacy wires the blocking rules in after construction: history and live
// group events mark the authors the reader blocked.
func (s *Service) SetPrivacy(p Privacy) { s.privacy = p }

// SetLimiter wires the per-account bucket for the first message of a dialogue.
func (s *Service) SetLimiter(limiter *ratelimit.Limiter) { s.limiter = limiter }

// SetProfiles wires the profile module in after both exist.
func (s *Service) SetProfiles(profiles Profiles) { s.profiles = profiles }

// SetNotifier lets messages reach people away from the app: a direct or group
// message, or a mention in a server channel.
func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

// SetNotificationLevels wires the server-side notification settings in: a
// channel speaks as loudly as its people asked, instead of mentions only.
func (s *Service) SetNotificationLevels(levels NotificationLevels) { s.levels = levels }

// SetWorkspaces lets a search without a target cover every server of the account.
func (s *Service) SetWorkspaces(list WorkspaceList) { s.workspaces = list }

// SetSearchLimiter wires the per-account bucket for full-text search.
func (s *Service) SetSearchLimiter(limiter *ratelimit.Limiter) { s.searchLimiter = limiter }

// fillAuthors loads the public part of every author on a page in one call. The
// author of a deleted account stays nil: its messages keep their place.
func (s *Service) fillAuthors(ctx context.Context, list []Message) error {
	if s.profiles == nil || len(list) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(list))
	for i := range list {
		if list[i].Author != nil {
			ids = append(ids, list[i].Author.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	profiles, err := s.profiles.Profiles(ctx, ids)
	if err != nil {
		return fmt.Errorf("load message authors: %w", err)
	}
	for i := range list {
		if list[i].Author == nil {
			continue
		}
		if loaded, ok := profiles[list[i].Author.ID]; ok {
			author := loaded
			list[i].Author = &author
		}
	}
	return nil
}

// fillAuthor is fillAuthors for one message.
func (s *Service) fillAuthor(ctx context.Context, message Message) (Message, error) {
	list := []Message{message}
	if err := s.fillAuthors(ctx, list); err != nil {
		return Message{}, err
	}
	return list[0], nil
}

// withAttachments fills in the files of one message.
func (s *Service) withAttachments(ctx context.Context, m Message) (Message, error) {
	if s.files == nil {
		return m, nil
	}
	byMessage, err := s.files.ByMessages(ctx, []uuid.UUID{m.ID})
	if err != nil {
		return Message{}, err
	}
	m.Attachments = byMessage[m.ID]
	return m, nil
}

// withAttachmentsPage fills in the files of a whole page in one query.
func (s *Service) withAttachmentsPage(ctx context.Context, list []Message) ([]Message, error) {
	if s.files == nil || len(list) == 0 {
		return list, nil
	}
	ids := make([]uuid.UUID, 0, len(list))
	for _, m := range list {
		ids = append(ids, m.ID)
	}
	byMessage, err := s.files.ByMessages(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Attachments = byMessage[list[i].ID]
	}
	return list, nil
}

// RecordSystem writes a group-change line into a conversation feed and tells
// every current participant about it. It is called by internal/conversations
// after the change itself is stored; a failure here leaves the change in place.
func (s *Service) RecordSystem(ctx context.Context, channelID uuid.UUID, event conversations.SystemEvent) error {
	created, err := s.store.CreateSystem(ctx, NewSystemMessage{
		ChannelID: channelID,
		AuthorID:  event.ActorID,
		Content:   event.Content,
		Type:      event.Type,
		TargetID:  event.TargetID,
		Name:      event.Name,
	})
	if err != nil {
		return err
	}
	loaded, err := s.store.Load(ctx, created, event.ActorID)
	if err != nil {
		return err
	}
	loaded, err = s.fillAuthor(ctx, loaded)
	if err != nil {
		return err
	}
	s.publish(ctx, events.KindMessageCreated, uuid.Nil, loaded)
	return nil
}

// Since returns what a reconnecting client missed in one channel, oldest first.
func (s *Service) Since(ctx context.Context, channelID, callerID uuid.UUID, afterSeq int64, limit int) ([]Message, error) {
	view, err := s.channels.Get(ctx, channelID, callerID)
	if err != nil {
		return nil, mapAccessError(err)
	}
	if view.Channel.Kind == channels.KindSecret {
		return nil, ErrNotFound
	}
	switch {
	case limit <= 0:
		limit = defaultPageSize
	case limit > maxPageSize:
		limit = maxPageSize
	}
	list, err := s.store.Since(ctx, channelID, afterSeq, limit, callerID)
	if err != nil {
		return nil, err
	}
	if err := s.fillAuthors(ctx, list); err != nil {
		return nil, err
	}
	if view.Channel.WorkspaceID == uuid.Nil {
		if s.inGroup(ctx, channelID) {
			s.markBlocked(ctx, callerID, list)
		}
	} else {
		// A server channel keeps the message for everyone; only its client-side
		// view can collapse it, and the history endpoint is that view.
		s.markBlocked(ctx, callerID, list)
	}
	return list, nil
}

// ChannelOf is which channel a message belongs to. internal/files asks before
// it hands out a download link.
func (s *Service) ChannelOf(ctx context.Context, messageID uuid.UUID) (uuid.UUID, error) {
	message, err := s.store.Message(ctx, messageID)
	if err != nil {
		return uuid.Nil, err
	}
	return message.ChannelID, nil
}

// SinceEvents returns what a reconnecting client missed in one channel as
// events, oldest first, ready for the gateway to send.
func (s *Service) SinceEvents(ctx context.Context, channelID, callerID uuid.UUID, afterSeq int64, limit int) ([]events.Event, error) {
	list, err := s.Since(ctx, channelID, callerID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	out := make([]events.Event, 0, len(list))
	for _, m := range list {
		out = append(out, events.Event{
			Kind:      events.KindMessageCreated,
			ChannelID: m.ChannelID,
			Seq:       m.Seq,
			Payload:   APIMessage(m),
		})
	}
	return out, nil
}

// publish tells the watchers what happened: a server's connections for a
// channel, each participant's own connections for a conversation. A secret chat
// never reaches this code — it has no plaintext messages at all.
func (s *Service) publish(ctx context.Context, kind events.Kind, workspaceID uuid.UUID, m Message) {
	if s.publisher == nil {
		return
	}
	event := events.Event{
		Kind:        kind,
		WorkspaceID: workspaceID,
		ChannelID:   m.ChannelID,
		Seq:         m.Seq,
		Payload:     APIMessage(m),
	}
	if workspaceID != uuid.Nil {
		// A server channel has one stream for everyone, so a per-reader mark
		// cannot travel here; the client sees it when it loads the history.
		s.publisher.Publish(event)
		return
	}
	if s.conversations == nil {
		return
	}
	members, err := s.conversations.MemberIDs(ctx, m.ChannelID)
	if err != nil {
		// The message is stored; a failed fan-out must not fail the send.
		return
	}
	// A system line is not its author's speech: a block changes nothing about it.
	mark := m.Kind != MessageKindSystem && s.inGroup(ctx, m.ChannelID)
	for _, memberID := range members {
		personal := event
		// A group has its own stream per participant, so the block mark can be
		// personal. A direct dialogue does not need it: writing there is refused.
		if mark {
			personal.Payload = APIMessage(s.blockedFor(ctx, memberID, m))
		}
		s.publisher.PublishToUser(memberID, personal)
	}
}

// inGroup reports whether the channel is a group chat, where a blocked author
// stays in the history. A direct dialogue never has a blocked pair writing in
// it, and a server channel has a single stream that cannot be personal.
func (s *Service) inGroup(ctx context.Context, channelID uuid.UUID) bool {
	if s.privacy == nil || s.conversations == nil {
		return false
	}
	kind, err := s.conversations.Kind(ctx, channelID)
	if err != nil {
		return false
	}
	return kind == conversations.KindGroup
}

// blockedFor returns the message as one viewer sees it: the author is blocked
// by them, so the client collapses it. A failed lookup leaves the message as it
// is: a display mark must not break the history, and the block still binds
// where it matters.
func (s *Service) blockedFor(ctx context.Context, viewerID uuid.UUID, m Message) Message {
	if s.privacy == nil || m.Author == nil {
		return m
	}
	blocked, err := s.privacy.BlockedIDs(ctx, viewerID)
	if err != nil {
		return m
	}
	if blocked[m.Author.ID] {
		m.Blocked = true
	}
	return m
}

// markBlocked marks a page of history for its reader in one lookup.
func (s *Service) markBlocked(ctx context.Context, viewerID uuid.UUID, list []Message) {
	if s.privacy == nil || len(list) == 0 {
		return
	}
	blocked, err := s.privacy.BlockedIDs(ctx, viewerID)
	if err != nil || len(blocked) == 0 {
		return
	}
	for i := range list {
		if list[i].Author != nil && blocked[list[i].Author.ID] {
			list[i].Blocked = true
		}
	}
}

// directBlocked reports whether the caller may not write in a direct dialogue
// because either side blocked the other. Only a dialogue freezes this way; a
// group keeps its history, marked for whoever blocked whom.
func (s *Service) directBlocked(ctx context.Context, channel channels.Channel, callerID uuid.UUID) (bool, error) {
	if s.privacy == nil || s.conversations == nil || channel.WorkspaceID != uuid.Nil {
		return false, nil
	}
	kind, err := s.conversations.Kind(ctx, channel.ID)
	if err != nil {
		return false, fmt.Errorf("conversation kind: %w", err)
	}
	if kind != conversations.KindDirect {
		return false, nil
	}
	members, err := s.conversations.MemberIDs(ctx, channel.ID)
	if err != nil {
		return false, fmt.Errorf("list conversation members: %w", err)
	}
	for _, memberID := range members {
		if memberID == callerID {
			continue
		}
		blocked, err := s.privacy.BlockedEitherWay(ctx, callerID, memberID)
		if err != nil {
			return false, fmt.Errorf("check block: %w", err)
		}
		if blocked {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) Send(ctx context.Context, callerID uuid.UUID, m NewMessage) (Message, bool, error) {
	channel, err := s.channels.MayWrite(ctx, m.ChannelID, callerID)
	if err != nil {
		return Message{}, false, mapAccessError(err)
	}
	// A secret chat carries ciphertext envelopes, not messages: plaintext must
	// never be stored for it.
	if channel.Kind == channels.KindSecret {
		return Message{}, false, ErrNotFound
	}

	// A dialogue between two accounts where either blocked the other accepts no
	// new messages: the refusal is the same 404 as everywhere else.
	blocked, err := s.directBlocked(ctx, channel, callerID)
	if err != nil {
		return Message{}, false, err
	}
	if blocked {
		return Message{}, false, ErrNotFound
	}

	content, err := cleanContent(m.Content)
	// A message that carries files may have no text at all.
	emptyWithFiles := errors.Is(err, ErrInvalidContent) && content == "" && len(m.AttachmentIDs) > 0
	if err != nil && !emptyWithFiles {
		return Message{}, false, err
	}
	if m.ClientMessageID == "" {
		return Message{}, false, ErrInvalidContent
	}

	// Files need their own right: checked before anything is stored, so a
	// refused message is not left half sent without them.
	if len(m.AttachmentIDs) > 0 && s.files != nil {
		if err := s.files.MayAttach(ctx, channel, callerID); err != nil {
			return Message{}, false, mapAccessError(err)
		}
	}

	if m.ReplyToID != nil {
		replied, err := s.store.Message(ctx, *m.ReplyToID)
		if err != nil || replied.ChannelID != m.ChannelID {
			return Message{}, false, ErrInvalidReply
		}
	}

	mentions, err := s.knownMembers(ctx, channel, m.Mentions)
	if err != nil {
		return Message{}, false, err
	}

	// The first message of a new dialogue spends the same per-account token as
	// opening it: a new conversation and its opener are one action to guard.
	if channel.WorkspaceID == uuid.Nil && s.limiter != nil {
		has, err := s.store.HasMessages(ctx, channel.ID)
		if err != nil {
			return Message{}, false, err
		}
		if !has {
			allowed, retryAfter := s.limiter.Allow(callerID)
			if !allowed {
				return Message{}, false, &RateLimitedError{RetryAfter: retryAfter}
			}
		}
	}

	created, existed, err := s.store.Create(ctx, NewMessage{
		ChannelID:       m.ChannelID,
		AuthorID:        callerID,
		ClientMessageID: m.ClientMessageID,
		Content:         content,
		ReplyToID:       m.ReplyToID,
	})
	if err != nil {
		return Message{}, false, err
	}
	// A resend keeps the mentions of the first send: the text is the same message.
	if !existed && len(mentions) > 0 {
		if err := s.store.AddMentions(ctx, created.ID, mentions); err != nil {
			return Message{}, false, err
		}
	}

	if !existed && len(m.AttachmentIDs) > 0 && s.files != nil {
		if _, err := s.files.AttachAll(ctx, created.ID, callerID, m.AttachmentIDs); err != nil {
			return Message{}, false, err
		}
	}

	loaded, err := s.store.Load(ctx, created, callerID)
	if err != nil {
		return Message{}, false, err
	}
	loaded, err = s.withAttachments(ctx, loaded)
	if err != nil {
		return Message{}, false, err
	}
	loaded, err = s.fillAuthor(ctx, loaded)
	if err != nil {
		return Message{}, false, err
	}
	// A resend is not news: the watchers already saw this message.
	if !existed {
		s.publish(ctx, events.KindMessageCreated, channel.WorkspaceID, loaded)
		s.notifyAway(ctx, channel, callerID, loaded, mentions)
	}
	return loaded, existed, nil
}

// SearchRateLimitedError says how long the account should wait before another
// search. The HTTP layer turns it into 429 with Retry-After.
type SearchRateLimitedError struct {
	RetryAfter time.Duration
}

func (e *SearchRateLimitedError) Error() string {
	return fmt.Sprintf("messages: too many searches, retry in %s", e.RetryAfter)
}

// A search page stays small: a result is a pointer, not a reading surface.
const (
	searchPageSize       = 25
	maxSearchQueryLength = 200
)

// Search looks for text in what the caller may read. Visibility is decided
// here, before the query: the SQL receives only channels and conversations the
// caller can open, so a hidden channel cannot appear even in a count. Blocked
// authors are excluded, deleted messages and system lines never match, and
// secret chats are not conversations in this sense at all.
func (s *Service) Search(ctx context.Context, callerID uuid.UUID, params SearchParams) (SearchPage, error) {
	if s.searchLimiter != nil {
		if allowed, wait := s.searchLimiter.Allow(callerID); !allowed {
			return SearchPage{}, &SearchRateLimitedError{RetryAfter: wait}
		}
	}
	term := strings.TrimSpace(params.Query)
	if term == "" || len([]rune(term)) > maxSearchQueryLength {
		return SearchPage{}, ErrInvalidSearch
	}
	allowed, err := s.searchChannels(ctx, callerID, params)
	if err != nil {
		return SearchPage{}, err
	}
	if len(allowed) == 0 {
		return SearchPage{}, nil
	}

	limit := params.Limit
	if limit <= 0 || limit > searchPageSize {
		limit = searchPageSize
	}
	excluded := []uuid.UUID{}
	if s.privacy != nil {
		if blocked, err := s.privacy.BlockedIDs(ctx, callerID); err == nil {
			for id := range blocked {
				excluded = append(excluded, id)
			}
		}
	}
	hits, err := s.store.Search(ctx, SearchQuery{
		Term:            term,
		ChannelIDs:      allowed,
		AuthorID:        params.AuthorID,
		HasFiles:        params.HasFiles,
		Before:          params.Before,
		ExcludedAuthors: excluded,
		Limit:           limit + 1,
	})
	if err != nil {
		return SearchPage{}, fmt.Errorf("search messages: %w", err)
	}

	var next *int64
	if len(hits) > limit {
		hits = hits[:limit]
		cursor := hits[len(hits)-1].Message.Seq
		next = &cursor
	}

	list := make([]Message, 0, len(hits))
	channelIDs := make([]uuid.UUID, 0, len(hits))
	seen := make(map[uuid.UUID]bool, len(hits))
	for _, hit := range hits {
		list = append(list, hit.Message)
		if !seen[hit.Message.ChannelID] {
			seen[hit.Message.ChannelID] = true
			channelIDs = append(channelIDs, hit.Message.ChannelID)
		}
	}
	// Authors and files of the whole page in one query each, not one per hit.
	if err := s.fillAuthors(ctx, list); err != nil {
		return SearchPage{}, err
	}
	if s.files != nil {
		if list, err = s.withAttachmentsPage(ctx, list); err != nil {
			return SearchPage{}, err
		}
	}
	names, err := s.channels.LookupMany(ctx, channelIDs)
	if err != nil {
		return SearchPage{}, fmt.Errorf("name search channels: %w", err)
	}

	results := make([]SearchResult, 0, len(list))
	for i, message := range list {
		channel, ok := names[message.ChannelID]
		if !ok {
			continue
		}
		result := SearchResult{Message: message, Headline: hits[i].Headline, ChannelName: channel.Name}
		if channel.WorkspaceID != uuid.Nil {
			workspaceID := channel.WorkspaceID
			result.WorkspaceID = &workspaceID
		}
		results = append(results, result)
	}
	return SearchPage{Results: results, NextCursor: next}, nil
}

// searchChannels is the visibility rule of search: one channel, one
// conversation, one server, or everything the account may read.
func (s *Service) searchChannels(ctx context.Context, callerID uuid.UUID, params SearchParams) ([]uuid.UUID, error) {
	switch {
	case params.ConversationID != nil:
		if params.ChannelID != nil || params.WorkspaceID != nil {
			return nil, ErrInvalidSearch
		}
		view, err := s.channels.Get(ctx, *params.ConversationID, callerID)
		if err != nil {
			return nil, mapAccessError(err)
		}
		if view.Channel.WorkspaceID != uuid.Nil || view.Channel.Kind == channels.KindSecret {
			return nil, ErrNotFound
		}
		return []uuid.UUID{*params.ConversationID}, nil
	case params.ChannelID != nil:
		view, err := s.channels.Get(ctx, *params.ChannelID, callerID)
		if err != nil {
			return nil, mapAccessError(err)
		}
		// A conversation is not a channel here, and a secret chat is neither.
		if view.Channel.WorkspaceID == uuid.Nil || view.Channel.Kind == channels.KindSecret {
			return nil, ErrNotFound
		}
		return []uuid.UUID{*params.ChannelID}, nil
	case params.WorkspaceID != nil:
		visible, err := s.channels.Visible(ctx, *params.WorkspaceID, callerID)
		if err != nil {
			return nil, mapAccessError(err)
		}
		return idsOf(visible), nil
	}

	allowed := map[uuid.UUID]bool{}
	if s.workspaces != nil && !params.ConversationsOnly {
		workspaceIDs, err := s.workspaces.Servers(ctx, callerID)
		if err != nil {
			return nil, fmt.Errorf("list search servers: %w", err)
		}
		for _, workspaceID := range workspaceIDs {
			visible, err := s.channels.Visible(ctx, workspaceID, callerID)
			if err != nil {
				return nil, mapAccessError(err)
			}
			for id := range visible {
				allowed[id] = true
			}
		}
	}
	if s.conversations != nil {
		conversationIDs, err := s.conversations.ChannelIDs(ctx, callerID)
		if err != nil {
			return nil, fmt.Errorf("list search conversations: %w", err)
		}
		for _, id := range conversationIDs {
			allowed[id] = true
		}
	}
	return idsOf(allowed), nil
}

func idsOf(set map[uuid.UUID]bool) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

func (s *Service) History(ctx context.Context, channelID, callerID uuid.UUID, before *int64, limit int) (Page, error) {
	view, err := s.channels.Get(ctx, channelID, callerID)
	if err != nil {
		return Page{}, mapAccessError(err)
	}
	if view.Channel.Kind == channels.KindSecret {
		return Page{}, ErrNotFound
	}

	switch {
	case limit <= 0:
		limit = defaultPageSize
	case limit > maxPageSize:
		limit = maxPageSize
	}

	list, err := s.store.List(ctx, channelID, before, limit, callerID)
	if err != nil {
		return Page{}, err
	}
	if err := s.fillAuthors(ctx, list); err != nil {
		return Page{}, err
	}

	list, err = s.withAttachmentsPage(ctx, list)
	if err != nil {
		return Page{}, err
	}
	if view.Channel.WorkspaceID == uuid.Nil {
		if s.inGroup(ctx, channelID) {
			s.markBlocked(ctx, callerID, list)
		}
	} else {
		// A server channel keeps the message for everyone; only its client-side
		// view can collapse it, and the history endpoint is that view.
		s.markBlocked(ctx, callerID, list)
	}

	page := Page{Messages: list}
	// A full page means there may be more: the oldest seq here is the next cursor.
	if len(list) == limit && limit > 0 {
		oldest := list[len(list)-1].Seq
		page.NextBefore = &oldest
	}
	return page, nil
}

func (s *Service) Edit(ctx context.Context, messageID, callerID uuid.UUID, content string) (Message, error) {
	message, channel, err := s.readable(ctx, messageID, callerID)
	if err != nil {
		return Message{}, err
	}
	// Editing is not moderation: only the author rewrites their own text.
	if message.Author == nil || message.Author.ID != callerID {
		return Message{}, ErrForbidden
	}

	content, err = cleanContent(content)
	if err != nil {
		return Message{}, err
	}

	edited, err := s.store.Edit(ctx, messageID, callerID, content)
	if err != nil {
		return Message{}, err
	}
	loaded, err := s.store.Load(ctx, edited, callerID)
	if err != nil {
		return Message{}, err
	}
	loaded, err = s.withAttachments(ctx, loaded)
	if err != nil {
		return Message{}, err
	}
	loaded, err = s.fillAuthor(ctx, loaded)
	if err != nil {
		return Message{}, err
	}
	s.publish(ctx, events.KindMessageUpdated, channel.WorkspaceID, loaded)
	return loaded, nil
}

func (s *Service) Delete(ctx context.Context, messageID, callerID uuid.UUID) (Message, error) {
	message, err := s.store.Message(ctx, messageID)
	if err != nil {
		return Message{}, err
	}
	view, err := s.channels.Get(ctx, message.ChannelID, callerID)
	if err != nil {
		return Message{}, mapAccessError(err)
	}
	channel := view.Channel
	if channel.Kind == channels.KindSecret || message.Author == nil {
		// A secret chat has no plaintext messages; a system line has no author
		// and stays.
		return Message{}, ErrForbidden
	}
	// The author deletes their own; manageMessages deletes anyone's in a server
	// channel. A conversation has no moderators.
	if message.Author.ID != callerID &&
		(channel.WorkspaceID == uuid.Nil || !view.Permissions.Has(perms.ManageMessages)) {
		return Message{}, ErrForbidden
	}

	deleted, err := s.store.Delete(ctx, messageID, message.Author.ID)
	if err != nil {
		return Message{}, err
	}
	loaded, err := s.store.Load(ctx, deleted, callerID)
	if err != nil {
		return Message{}, err
	}
	loaded, err = s.withAttachments(ctx, loaded)
	if err != nil {
		return Message{}, err
	}
	loaded, err = s.fillAuthor(ctx, loaded)
	if err != nil {
		return Message{}, err
	}
	s.publish(ctx, events.KindMessageDeleted, channel.WorkspaceID, loaded)
	return loaded, nil
}

// React adds the caller's reaction. Reacting is writing, so a read-only channel
// takes none from a plain member.
func (s *Service) React(ctx context.Context, messageID, callerID uuid.UUID, emoji string) (Message, error) {
	message, err := s.store.Message(ctx, messageID)
	if err != nil {
		return Message{}, err
	}
	channel, err := s.channels.MayWrite(ctx, message.ChannelID, callerID)
	if err != nil {
		return Message{}, mapAccessError(err)
	}
	if channel.Kind == channels.KindSecret {
		return Message{}, ErrNotFound
	}

	emoji, err = cleanEmoji(emoji)
	if err != nil {
		return Message{}, err
	}
	if message.DeletedAt != nil {
		return Message{}, ErrForbidden
	}

	if err := s.store.AddReaction(ctx, messageID, callerID, emoji); err != nil {
		return Message{}, err
	}
	loaded, err := s.store.Load(ctx, message, callerID)
	if err != nil {
		return Message{}, err
	}
	loaded, err = s.withAttachments(ctx, loaded)
	if err != nil {
		return Message{}, err
	}
	loaded, err = s.fillAuthor(ctx, loaded)
	if err != nil {
		return Message{}, err
	}
	s.publish(ctx, events.KindReactionChanged, channel.WorkspaceID, loaded)
	return loaded, nil
}

// Unreact takes the caller's own reaction back; reading the channel is enough,
// so someone who lost the right to write can still clean up after themselves.
func (s *Service) Unreact(ctx context.Context, messageID, callerID uuid.UUID, emoji string) (Message, error) {
	message, channel, err := s.readable(ctx, messageID, callerID)
	if err != nil {
		return Message{}, err
	}
	emoji, err = cleanEmoji(emoji)
	if err != nil {
		return Message{}, err
	}

	if err := s.store.RemoveReaction(ctx, messageID, callerID, emoji); err != nil {
		return Message{}, err
	}
	loaded, err := s.store.Load(ctx, message, callerID)
	if err != nil {
		return Message{}, err
	}
	loaded, err = s.withAttachments(ctx, loaded)
	if err != nil {
		return Message{}, err
	}
	loaded, err = s.fillAuthor(ctx, loaded)
	if err != nil {
		return Message{}, err
	}
	s.publish(ctx, events.KindReactionChanged, channel.WorkspaceID, loaded)
	return loaded, nil
}

// readable loads a message the caller is allowed to see, with its channel.
func (s *Service) readable(ctx context.Context, messageID, callerID uuid.UUID) (Message, channels.Channel, error) {
	message, err := s.store.Message(ctx, messageID)
	if err != nil {
		return Message{}, channels.Channel{}, err
	}
	view, err := s.channels.Get(ctx, message.ChannelID, callerID)
	if err != nil {
		return Message{}, channels.Channel{}, mapAccessError(err)
	}
	if view.Channel.Kind == channels.KindSecret {
		return Message{}, channels.Channel{}, ErrNotFound
	}
	return message, view.Channel, nil
}

// knownMembers drops mentions of accounts outside the server: a notification to
// someone who cannot open the channel is noise at best. The client sends ids,
// never text, so a display name cannot redirect a mention: @nickname stays the
// handle even when the profile shows another name. A conversation has no server,
// so its people are the audience instead.
func (s *Service) knownMembers(ctx context.Context, channel channels.Channel, ids []uuid.UUID) ([]uuid.UUID, error) {
	allowed := make(map[uuid.UUID]bool, len(ids))
	if channel.WorkspaceID == uuid.Nil {
		// Without the conversations module nobody counts as a member, so no
		// mention is stored; the message itself is not refused.
		if s.conversations != nil {
			members, err := s.conversations.MemberIDs(ctx, channel.ID)
			if err != nil {
				return nil, err
			}
			for _, memberID := range members {
				allowed[memberID] = true
			}
		}
	} else {
		for _, id := range uniqueIDs(ids) {
			member, err := s.members.IsMember(ctx, channel.WorkspaceID, id)
			if err != nil {
				return nil, err
			}
			if member {
				allowed[id] = true
			}
		}
	}

	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range uniqueIDs(ids) {
		if allowed[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// uniqueIDs keeps the first occurrence of every id, in order.
func uniqueIDs(ids []uuid.UUID) []uuid.UUID {
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

// mapAccessError keeps this package's errors: a channel the caller may not read
// is "not found", a channel they may read but not write is "not allowed".
func mapAccessError(err error) error {
	switch {
	case errors.Is(err, channels.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, channels.ErrForbidden):
		return ErrForbidden
	default:
		return err
	}
}

func cleanContent(content string) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" || len([]rune(content)) > maxContentLength {
		return "", ErrInvalidContent
	}
	return content, nil
}

func cleanEmoji(emoji string) (string, error) {
	emoji = strings.TrimSpace(emoji)
	if emoji == "" || len([]rune(emoji)) > maxEmojiLength {
		return "", ErrInvalidEmoji
	}
	return emoji, nil
}

// notifyAway tells the people this message is for, who may not have Orbit open:
// every other participant of a conversation, and in a server channel both the
// accounts it mentions and the ones who asked to hear everything there. The
// levels come from internal/notifysettings in one query for the whole audience,
// never one per member. The notification says who and where; the text is not
// sent, so it never leaves the app.
func (s *Service) notifyAway(ctx context.Context, channel channels.Channel, authorID uuid.UUID, m Message, mentions []uuid.UUID) {
	if s.notifier == nil || m.Kind == MessageKindSystem {
		return
	}
	author := authorName(m)
	levels := s.channelLevels(ctx, channel)
	if channel.WorkspaceID == uuid.Nil {
		s.notifyConversation(ctx, channel, authorID, author, levels)
		return
	}

	mentioned := make([]uuid.UUID, 0, len(mentions))
	for _, id := range mentions {
		if id == authorID || levels[id] == notifysettings.LevelNone {
			continue
		}
		// A mention in a channel the person cannot open would name the channel to them.
		if _, err := s.channels.Get(ctx, channel.ID, id); err != nil {
			continue
		}
		mentioned = append(mentioned, id)
	}
	// People who asked for every message here hear this one too, unless they
	// were named already: one notification, with the more precise wording.
	loud := make([]uuid.UUID, 0, len(levels))
	for id, level := range levels {
		if id == authorID || level != notifysettings.LevelAll || slices.Contains(mentioned, id) {
			continue
		}
		if _, err := s.channels.Get(ctx, channel.ID, id); err != nil {
			continue
		}
		loud = append(loud, id)
	}
	url := "/s/" + channel.WorkspaceID.String() + "/" + channel.ID.String()
	if len(loud) > 0 {
		s.notifier.Notify(ctx, loud, push.Notification{
			Title: "Новое сообщение в #" + channel.Name,
			Body:  "От " + author,
			URL:   url,
			Tag:   channel.ID.String(),
		})
	}
	if len(mentioned) > 0 {
		s.notifier.Notify(ctx, mentioned, push.Notification{
			Title: "Упоминание в #" + channel.Name,
			Body:  "От " + author,
			URL:   url,
			Tag:   channel.ID.String(),
		})
	}
}

// channelLevels is the whole audience's choice for one channel. Without the
// module every account keeps the default: mentions in a channel, everything in
// a conversation.
func (s *Service) channelLevels(ctx context.Context, channel channels.Channel) map[uuid.UUID]notifysettings.Level {
	if s.levels == nil {
		return nil
	}
	levels, err := s.levels.Levels(ctx, channel)
	if err != nil {
		return nil
	}
	return levels
}

func (s *Service) notifyConversation(ctx context.Context, channel channels.Channel, authorID uuid.UUID, author string, levels map[uuid.UUID]notifysettings.Level) {
	if s.conversations == nil {
		return
	}
	members, err := s.conversations.MemberIDs(ctx, channel.ID)
	if err != nil {
		return
	}
	recipients := make([]uuid.UUID, 0, len(members))
	for _, id := range members {
		if id == authorID || levels[id] == notifysettings.LevelNone {
			continue
		}
		// Someone who blocked the author keeps the group, not the news of them.
		if s.privacy != nil {
			if blocked, err := s.privacy.BlockedIDs(ctx, id); err == nil && blocked[authorID] {
				continue
			}
		}
		recipients = append(recipients, id)
	}
	if len(recipients) == 0 {
		return
	}
	body := "Новое сообщение"
	if kind, err := s.conversations.Kind(ctx, channel.ID); err == nil && kind == conversations.KindGroup {
		body = "Новое сообщение в групповом чате"
	}
	s.notifier.Notify(ctx, recipients, push.Notification{
		Title: author,
		Body:  body,
		URL:   "/me/" + channel.ID.String(),
		Tag:   channel.ID.String(),
	})
}

// authorName is how a notification names the author: the display name, then the nickname.
func authorName(m Message) string {
	if m.Author == nil {
		return "Удалённый пользователь"
	}
	if m.Author.DisplayName != "" {
		return m.Author.DisplayName
	}
	return m.Author.Nickname
}
