// Package messages owns what people write in a channel: sending, history,
// edits, replies, mentions and reactions (FR-04). Who may read or write a
// channel is decided by internal/channels; this package asks it every time.
package messages

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/events"
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

// Author is who wrote a message, as the API shows them.
type Author struct {
	ID        uuid.UUID
	Nickname  string
	Email     string
	IsGuest   bool
	CreatedAt time.Time
}

type Message struct {
	ID        uuid.UUID
	ChannelID uuid.UUID
	Seq       int64
	// Author is nil when the account has been deleted; the message stays.
	Author    *Author
	Content   string
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

// Page is history, newest first.
type Page struct {
	Messages []Message
	// NextBefore is the cursor for the next page; nil at the start of the history.
	NextBefore *int64
}

type Store interface {
	// Create stores the message, or returns the one already stored for this
	// client id — the same send twice must not make two messages.
	Create(ctx context.Context, m NewMessage) (Message, bool, error)
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
}

// ChannelAccess is the part of internal/channels this package needs.
type ChannelAccess interface {
	// MayWrite returns the channel when the caller may post in it.
	MayWrite(ctx context.Context, channelID, callerID uuid.UUID) (channels.Channel, error)
	// Get returns the channel when the caller may read it.
	Get(ctx context.Context, channelID, callerID uuid.UUID) (channels.View, error)
}

// Publisher announces what happened to the clients watching the server. A nil
// publisher is allowed: the HTTP API works the same, nobody is just told live.
type Publisher interface {
	Publish(event events.Event)
	// PublishToUser reaches one account's connections, wherever they are: a
	// conversation has no server to announce to.
	PublishToUser(userID uuid.UUID, event events.Event)
}

// Conversations tells who is in a conversation. Optional: without it, a
// conversation message is stored and simply not announced live.
type Conversations interface {
	MemberIDs(ctx context.Context, channelID uuid.UUID) ([]uuid.UUID, error)
	// Kind tells a pair from a group: a block freezes the first and only marks
	// the author in the second.
	Kind(ctx context.Context, channelID uuid.UUID) (conversations.Kind, error)
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
	mark := s.inGroup(ctx, m.ChannelID)
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

	mentions, err := s.knownMembers(ctx, channel.WorkspaceID, m.Mentions)
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
	// A resend is not news: the watchers already saw this message.
	if !existed {
		s.publish(ctx, events.KindMessageCreated, channel.WorkspaceID, loaded)
	}
	return loaded, existed, nil
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
	s.publish(ctx, events.KindMessageUpdated, channel.WorkspaceID, loaded)
	return loaded, nil
}

func (s *Service) Delete(ctx context.Context, messageID, callerID uuid.UUID) (Message, error) {
	message, channel, err := s.readable(ctx, messageID, callerID)
	if err != nil {
		return Message{}, err
	}
	if message.Author == nil || message.Author.ID != callerID {
		return Message{}, ErrForbidden
	}

	deleted, err := s.store.Delete(ctx, messageID, callerID)
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
// someone who cannot open the channel is noise at best.
func (s *Service) knownMembers(ctx context.Context, workspaceID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		member, err := s.members.IsMember(ctx, workspaceID, id)
		if err != nil {
			return nil, err
		}
		if member {
			out = append(out, id)
		}
	}
	return out, nil
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
