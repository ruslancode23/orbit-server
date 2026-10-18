package channels

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
)

// uniqueViolation is PostgreSQL's SQLSTATE for a broken unique constraint.
const uniqueViolation = "23505"

// Pool is the part of pgxpool.Pool this store needs: queries plus transactions,
// because reordering writes every channel of a server at once.
type Pool interface {
	db.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

type PostgresStore struct {
	pool Pool
	q    *db.Queries
}

func NewPostgresStore(pool Pool) *PostgresStore {
	return &PostgresStore{pool: pool, q: db.New(pool)}
}

var _ Store = (*PostgresStore)(nil)

func (s *PostgresStore) Create(ctx context.Context, c NewChannel) (Channel, error) {
	row, err := s.q.CreateChannel(ctx, db.CreateChannelParams{
		WorkspaceID: pgUUID(c.WorkspaceID),
		Kind:        db.ChannelKindText,
		Name:        text(c.Name),
		Topic:       text(c.Topic),
		IsReadOnly:  c.IsReadOnly,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return Channel{}, ErrNameTaken
		}
		return Channel{}, fmt.Errorf("create channel: %w", err)
	}
	return toChannel(row), nil
}

func (s *PostgresStore) Channel(ctx context.Context, id uuid.UUID) (Channel, error) {
	row, err := s.q.GetChannel(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Channel{}, ErrNotFound
		}
		return Channel{}, fmt.Errorf("get channel: %w", err)
	}
	return toChannel(row), nil
}

func (s *PostgresStore) List(ctx context.Context, workspaceID uuid.UUID) ([]Channel, error) {
	rows, err := s.q.ListWorkspaceChannels(ctx, pgUUID(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	out := make([]Channel, 0, len(rows))
	for _, row := range rows {
		out = append(out, toChannel(row))
	}
	return out, nil
}

func (s *PostgresStore) Update(ctx context.Context, id uuid.UUID, u Update) (Channel, error) {
	params := db.UpdateChannelParams{ID: pgUUID(id)}
	if u.Name != nil {
		params.Name = text(*u.Name)
	}
	if u.Topic != nil {
		// An empty topic clears it; the query needs the flag because a NULL
		// parameter means "leave this field alone".
		params.ClearTopic = *u.Topic == ""
		params.Topic = text(*u.Topic)
	}
	if u.IsReadOnly != nil {
		params.IsReadOnly = pgtype.Bool{Bool: *u.IsReadOnly, Valid: true}
	}

	row, err := s.q.UpdateChannel(ctx, params)
	if err != nil {
		var pgErr *pgconn.PgError
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return Channel{}, ErrNotFound
		case errors.As(err, &pgErr) && pgErr.Code == uniqueViolation:
			return Channel{}, ErrNameTaken
		}
		return Channel{}, fmt.Errorf("update channel: %w", err)
	}
	return toChannel(row), nil
}

func (s *PostgresStore) Delete(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteChannel(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	return nil
}

// Reorder writes every position in one transaction: a half-applied order would
// show the channels in a sequence nobody asked for.
func (s *PostgresStore) Reorder(ctx context.Context, workspaceID uuid.UUID, ids []uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)
	for position, id := range ids {
		affected, err := q.SetChannelPosition(ctx, db.SetChannelPositionParams{
			Position:    int32(position), //nolint:gosec // a list index, far below int32
			ID:          pgUUID(id),
			WorkspaceID: pgUUID(workspaceID),
		})
		if err != nil {
			return fmt.Errorf("set channel position: %w", err)
		}
		if affected == 0 {
			// The channel vanished or belongs to another server: nothing is written.
			return ErrIncompleteOrder
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func toChannel(row db.Channel) Channel {
	return Channel{
		ID:          uuid.UUID(row.ID.Bytes),
		WorkspaceID: uuid.UUID(row.WorkspaceID.Bytes),
		Name:        row.Name.String,
		Topic:       row.Topic.String,
		Position:    int(row.Position),
		IsReadOnly:  row.IsReadOnly,
		CreatedAt:   row.CreatedAt.Time,
	}
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func text(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
