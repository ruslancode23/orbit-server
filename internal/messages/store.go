package messages

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
)

type PostgresStore struct {
	q db.Querier
}

func NewPostgresStore(q db.Querier) *PostgresStore { return &PostgresStore{q: q} }

var _ Store = (*PostgresStore)(nil)

func (s *PostgresStore) Create(ctx context.Context, m NewMessage) (Message, bool, error) {
	row, err := s.q.CreateMessage(ctx, db.CreateMessageParams{
		ChannelID:       pgUUID(m.ChannelID),
		AuthorID:        pgUUID(m.AuthorID),
		ClientMessageID: m.ClientMessageID,
		Content:         m.Content,
		ReplyToID:       pgUUIDPtr(m.ReplyToID),
	})
	if err != nil {
		return Message{}, false, fmt.Errorf("create message: %w", err)
	}

	message := Message{
		ID:        uuid.UUID(row.ID.Bytes),
		ChannelID: uuid.UUID(row.ChannelID.Bytes),
		Seq:       row.Seq,
		Content:   row.Content,
		ReplyToID: uuidPtr(row.ReplyToID),
		CreatedAt: row.CreatedAt.Time,
		EditedAt:  timePtr(row.EditedAt),
		DeletedAt: timePtr(row.DeletedAt),
	}
	// The author is the caller; the row carries only the id.
	message.Author = &Author{ID: m.AuthorID}
	return message, !row.Inserted, nil
}

func (s *PostgresStore) Message(ctx context.Context, id uuid.UUID) (Message, error) {
	row, err := s.q.GetMessage(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Message{}, ErrNotFound
		}
		return Message{}, fmt.Errorf("get message: %w", err)
	}
	return toMessage(row.Message, row.User), nil
}

func (s *PostgresStore) List(ctx context.Context, channelID uuid.UUID, before *int64, limit int, viewerID uuid.UUID) ([]Message, error) {
	rows, err := s.q.ListChannelMessages(ctx, db.ListChannelMessagesParams{
		ChannelID: pgUUID(channelID),
		BeforeSeq: pgInt8(before),
		Limit:     int32(limit), //nolint:gosec // capped at maxPageSize by the service
	})
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}

	list := make([]Message, 0, len(rows))
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		message := toMessage(row.Message, row.User)
		list = append(list, message)
		ids = append(ids, message.ID)
	}
	// Mentions and reactions of the whole page in one query each, not per message.
	return s.decorate(ctx, list, ids, viewerID)
}

func (s *PostgresStore) Edit(ctx context.Context, id, authorID uuid.UUID, content string) (Message, error) {
	row, err := s.q.EditMessage(ctx, db.EditMessageParams{
		ID:       pgUUID(id),
		AuthorID: pgUUID(authorID),
		Content:  content,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Someone else's message, or one already deleted.
			return Message{}, ErrForbidden
		}
		return Message{}, fmt.Errorf("edit message: %w", err)
	}
	return s.Message(ctx, uuid.UUID(row.ID.Bytes))
}

func (s *PostgresStore) Delete(ctx context.Context, id, authorID uuid.UUID) (Message, error) {
	row, err := s.q.DeleteMessage(ctx, db.DeleteMessageParams{
		ID:       pgUUID(id),
		AuthorID: pgUUID(authorID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Message{}, ErrForbidden
		}
		return Message{}, fmt.Errorf("delete message: %w", err)
	}
	return s.Message(ctx, uuid.UUID(row.ID.Bytes))
}

func (s *PostgresStore) AddMentions(ctx context.Context, messageID uuid.UUID, userIDs []uuid.UUID) error {
	for _, id := range userIDs {
		if err := s.q.AddMessageMention(ctx, db.AddMessageMentionParams{
			MessageID: pgUUID(messageID),
			UserID:    pgUUID(id),
		}); err != nil {
			return fmt.Errorf("add mention: %w", err)
		}
	}
	return nil
}

func (s *PostgresStore) AddReaction(ctx context.Context, messageID, userID uuid.UUID, emoji string) error {
	if err := s.q.AddReaction(ctx, db.AddReactionParams{
		MessageID: pgUUID(messageID),
		UserID:    pgUUID(userID),
		Emoji:     emoji,
	}); err != nil {
		return fmt.Errorf("add reaction: %w", err)
	}
	return nil
}

func (s *PostgresStore) RemoveReaction(ctx context.Context, messageID, userID uuid.UUID, emoji string) error {
	if _, err := s.q.RemoveReaction(ctx, db.RemoveReactionParams{
		MessageID: pgUUID(messageID),
		UserID:    pgUUID(userID),
		Emoji:     emoji,
	}); err != nil {
		return fmt.Errorf("remove reaction: %w", err)
	}
	return nil
}

func (s *PostgresStore) Load(ctx context.Context, m Message, viewerID uuid.UUID) (Message, error) {
	// Reload so the author and the timestamps come from the database, not the caller.
	fresh, err := s.Message(ctx, m.ID)
	if err != nil {
		return Message{}, err
	}
	list, err := s.decorate(ctx, []Message{fresh}, []uuid.UUID{fresh.ID}, viewerID)
	if err != nil {
		return Message{}, err
	}
	return list[0], nil
}

// decorate fills mentions and reactions for a page of messages.
func (s *PostgresStore) decorate(ctx context.Context, list []Message, ids []uuid.UUID, viewerID uuid.UUID) ([]Message, error) {
	if len(list) == 0 {
		return list, nil
	}

	mentions, err := s.q.ListMessageMentions(ctx, toPgUUIDs(ids))
	if err != nil {
		return nil, fmt.Errorf("list mentions: %w", err)
	}
	byMessage := make(map[uuid.UUID][]uuid.UUID, len(list))
	for _, row := range mentions {
		id := uuid.UUID(row.MessageID.Bytes)
		byMessage[id] = append(byMessage[id], uuid.UUID(row.UserID.Bytes))
	}

	reactions, err := s.q.ListMessageReactions(ctx, db.ListMessageReactionsParams{
		ViewerID:   pgUUID(viewerID),
		MessageIds: toPgUUIDs(ids),
	})
	if err != nil {
		return nil, fmt.Errorf("list reactions: %w", err)
	}
	reactionsByMessage := make(map[uuid.UUID][]Reaction, len(list))
	for _, row := range reactions {
		id := uuid.UUID(row.MessageID.Bytes)
		reactionsByMessage[id] = append(reactionsByMessage[id], Reaction{
			Emoji:   row.Emoji,
			Count:   int(row.Total),
			Reacted: row.Reacted,
		})
	}

	for i := range list {
		list[i].Mentions = byMessage[list[i].ID]
		list[i].Reactions = reactionsByMessage[list[i].ID]
	}
	return list, nil
}

func toMessage(row db.Message, user db.User) Message {
	message := Message{
		ID:        uuid.UUID(row.ID.Bytes),
		ChannelID: uuid.UUID(row.ChannelID.Bytes),
		Seq:       row.Seq,
		Content:   row.Content,
		ReplyToID: uuidPtr(row.ReplyToID),
		CreatedAt: row.CreatedAt.Time,
		EditedAt:  timePtr(row.EditedAt),
		DeletedAt: timePtr(row.DeletedAt),
	}
	// The LEFT JOIN leaves an empty user when the account has been deleted.
	if row.AuthorID.Valid && user.ID.Valid {
		message.Author = &Author{
			ID:        uuid.UUID(user.ID.Bytes),
			Nickname:  user.Nickname,
			Email:     user.Email.String,
			IsGuest:   user.IsGuest,
			CreatedAt: user.CreatedAt.Time,
		}
	}
	return message
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func pgUUIDPtr(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgUUID(*id)
}

func toPgUUIDs(ids []uuid.UUID) []pgtype.UUID {
	out := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		out = append(out, pgUUID(id))
	}
	return out
}

func pgInt8(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

func uuidPtr(v pgtype.UUID) *uuid.UUID {
	if !v.Valid {
		return nil
	}
	id := uuid.UUID(v.Bytes)
	return &id
}

func timePtr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}
