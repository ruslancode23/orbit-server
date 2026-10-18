// Package messages owns what people write in a channel: sending, history,
// edits, replies, mentions and reactions (FR-04). Who may read or write a
// channel is decided by internal/channels; this package asks it every time.
package messages

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/events"
)

var (
	// ErrNotFound covers a missing message or channel and one the caller may not
	// read: the same answer, so the API reveals nothing about other servers.
	ErrNotFound = errors.New("messages: not found")
	// ErrForbidden means the caller may read the channel but not do this.
	ErrForbidden = errors.New("messages: not allowed")
	// ErrInvalidContent means the text is empty or too long.
	ErrInvalidContent = errors.New("messages: text must be 1 to 4000 characters")
	// ErrInvalidReply means the message replied to is in another channel or gone.
	ErrInvalidReply = errors.New("messages: the message replied to is not in this channel")
	// ErrInvalidEmoji means the reaction is empty or too long.
	ErrInvalidEmoji = errors.New("messages: a reaction must be 1 to 32 characters")
)

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
}

// Files binds uploaded files to the message that names them and loads the
// files of a page of messages.
type Files interface {
	AttachAll(ctx context.Context, messageID, uploaderID uuid.UUID, ids []uuid.UUID) ([]Attachment, error)
	ByMessages(ctx context.Context, messageIDs []uuid.UUID) (map[uuid.UUID][]Attachment, error)
}

// Members answers whether an account is on a server, so a mention of an
// outsider is dropped instead of notifying someone who cannot read the channel.
type Members interface {
	IsMember(ctx context.Context, workspaceID, userID uuid.UUID) (bool, error)
}

type Service struct {
	store    Store
	channels ChannelAccess
	members  Members
	// files is optional: without it a message simply carries no attachments.
	files Files
	// publisher is optional; see Publisher.
	publisher Publisher
}

func NewService(store Store, access ChannelAccess, members Members, files Files, publisher Publisher) *Service {
	return &Service{store: store, channels: access, members: members, files: files, publisher: publisher}
}

// SetFiles wires the files module in after construction: a message carries
// files, and a file asks which channel its message is in, so one of the two
// has to be told about the other once both exist.
func (s *Service) SetFiles(f Files) { s.files = f }

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
	if _, err := s.channels.Get(ctx, channelID, callerID); err != nil {
		return nil, mapAccessError(err)
	}
	switch {
	case limit <= 0:
		limit = defaultPageSize
	case limit > maxPageSize:
		limit = maxPageSize
	}
	return s.store.Since(ctx, channelID, afterSeq, limit, callerID)
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

// publish tells the watchers of this channel's server what happened.
func (s *Service) publish(kind events.Kind, workspaceID uuid.UUID, m Message) {
	if s.publisher == nil {
		return
	}
	s.publisher.Publish(events.Event{
		Kind:        kind,
		WorkspaceID: workspaceID,
		ChannelID:   m.ChannelID,
		Seq:         m.Seq,
		Payload:     APIMessage(m),
	})
}

func (s *Service) Send(ctx context.Context, callerID uuid.UUID, m NewMessage) (Message, bool, error) {
	channel, err := s.channels.MayWrite(ctx, m.ChannelID, callerID)
	if err != nil {
		return Message{}, false, mapAccessError(err)
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
		s.publish(events.KindMessageCreated, channel.WorkspaceID, loaded)
	}
	return loaded, existed, nil
}

func (s *Service) History(ctx context.Context, channelID, callerID uuid.UUID, before *int64, limit int) (Page, error) {
	if _, err := s.channels.Get(ctx, channelID, callerID); err != nil {
		return Page{}, mapAccessError(err)
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
	s.publish(events.KindMessageUpdated, channel.WorkspaceID, loaded)
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
	s.publish(events.KindMessageDeleted, channel.WorkspaceID, loaded)
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
	s.publish(events.KindReactionChanged, channel.WorkspaceID, loaded)
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
	s.publish(events.KindReactionChanged, channel.WorkspaceID, loaded)
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
