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
	CreatedAt time.Time
	EditedAt  *time.Time
	DeletedAt *time.Time
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

// Members answers whether an account is on a server, so a mention of an
// outsider is dropped instead of notifying someone who cannot read the channel.
type Members interface {
	IsMember(ctx context.Context, workspaceID, userID uuid.UUID) (bool, error)
}

type Service struct {
	store    Store
	channels ChannelAccess
	members  Members
}

func NewService(store Store, access ChannelAccess, members Members) *Service {
	return &Service{store: store, channels: access, members: members}
}

func (s *Service) Send(ctx context.Context, callerID uuid.UUID, m NewMessage) (Message, bool, error) {
	channel, err := s.channels.MayWrite(ctx, m.ChannelID, callerID)
	if err != nil {
		return Message{}, false, mapAccessError(err)
	}

	content, err := cleanContent(m.Content)
	if err != nil {
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

	loaded, err := s.store.Load(ctx, created, callerID)
	if err != nil {
		return Message{}, false, err
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

	page := Page{Messages: list}
	// A full page means there may be more: the oldest seq here is the next cursor.
	if len(list) == limit && limit > 0 {
		oldest := list[len(list)-1].Seq
		page.NextBefore = &oldest
	}
	return page, nil
}

func (s *Service) Edit(ctx context.Context, messageID, callerID uuid.UUID, content string) (Message, error) {
	message, err := s.readable(ctx, messageID, callerID)
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
	return s.store.Load(ctx, edited, callerID)
}

func (s *Service) Delete(ctx context.Context, messageID, callerID uuid.UUID) (Message, error) {
	message, err := s.readable(ctx, messageID, callerID)
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
	return s.store.Load(ctx, deleted, callerID)
}

// React adds the caller's reaction. Reacting is writing, so a read-only channel
// takes none from a plain member.
func (s *Service) React(ctx context.Context, messageID, callerID uuid.UUID, emoji string) (Message, error) {
	message, err := s.store.Message(ctx, messageID)
	if err != nil {
		return Message{}, err
	}
	if _, err := s.channels.MayWrite(ctx, message.ChannelID, callerID); err != nil {
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
	return s.store.Load(ctx, message, callerID)
}

// Unreact takes the caller's own reaction back; reading the channel is enough,
// so someone who lost the right to write can still clean up after themselves.
func (s *Service) Unreact(ctx context.Context, messageID, callerID uuid.UUID, emoji string) (Message, error) {
	message, err := s.readable(ctx, messageID, callerID)
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
	return s.store.Load(ctx, message, callerID)
}

// readable loads a message the caller is allowed to see.
func (s *Service) readable(ctx context.Context, messageID, callerID uuid.UUID) (Message, error) {
	message, err := s.store.Message(ctx, messageID)
	if err != nil {
		return Message{}, err
	}
	if _, err := s.channels.Get(ctx, message.ChannelID, callerID); err != nil {
		return Message{}, mapAccessError(err)
	}
	return message, nil
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
