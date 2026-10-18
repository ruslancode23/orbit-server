package reads

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
)

type PostgresStore struct {
	q db.Querier
}

func NewPostgresStore(q db.Querier) *PostgresStore { return &PostgresStore{q: q} }

var _ Store = (*PostgresStore)(nil)

func (s *PostgresStore) Mark(ctx context.Context, channelID, userID uuid.UUID, seq int64) (Unread, error) {
	// The query keeps the greater of the two values, so an old device cannot
	// pull the cursor back.
	if _, err := s.q.MarkChannelRead(ctx, db.MarkChannelReadParams{
		ChannelID:   pgUUID(channelID),
		UserID:      pgUUID(userID),
		LastReadSeq: seq,
	}); err != nil {
		return Unread{}, fmt.Errorf("mark channel read: %w", err)
	}
	return s.Channel(ctx, channelID, userID)
}

func (s *PostgresStore) Channel(ctx context.Context, channelID, userID uuid.UUID) (Unread, error) {
	row, err := s.q.CountChannelUnread(ctx, db.CountChannelUnreadParams{
		UserID:    pgUUID(userID),
		ChannelID: pgUUID(channelID),
	})
	if err != nil {
		return Unread{}, fmt.Errorf("count channel unread: %w", err)
	}
	return Unread{
		ChannelID:   channelID,
		LastReadSeq: row.LastReadSeq,
		Unread:      int(row.Unread),
		Mentions:    int(row.Mentions),
	}, nil
}

func (s *PostgresStore) Workspace(ctx context.Context, workspaceID, userID uuid.UUID) ([]Unread, error) {
	rows, err := s.q.CountWorkspaceUnread(ctx, db.CountWorkspaceUnreadParams{
		UserID:      pgUUID(userID),
		WorkspaceID: pgUUID(workspaceID),
	})
	if err != nil {
		return nil, fmt.Errorf("count workspace unread: %w", err)
	}
	out := make([]Unread, 0, len(rows))
	for _, row := range rows {
		out = append(out, Unread{
			ChannelID:   uuid.UUID(row.ChannelID.Bytes),
			LastReadSeq: row.LastReadSeq,
			Unread:      int(row.Unread),
			Mentions:    int(row.Mentions),
		})
	}
	return out, nil
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
