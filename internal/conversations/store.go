package conversations

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/profile"
)

// Pool is the part of pgxpool.Pool this store needs: queries plus a
// transaction, because a conversation and its members are written together.
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

// Create writes the conversation and everyone in it at once: a conversation
// without its people would be unreachable.
func (s *PostgresStore) Create(ctx context.Context, kind Kind, directKey string, memberIDs []uuid.UUID) (Conversation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Conversation{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)
	row, err := q.CreateConversation(ctx, db.CreateConversationParams{
		Kind:      db.ChannelKind(kind),
		DirectKey: text(directKey),
	})
	if err != nil {
		return Conversation{}, fmt.Errorf("create conversation: %w", err)
	}
	for _, id := range memberIDs {
		if err := q.AddChannelMember(ctx, db.AddChannelMemberParams{
			ChannelID: row.ID,
			UserID:    pgUUID(id),
		}); err != nil {
			return Conversation{}, fmt.Errorf("add conversation member: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Conversation{}, fmt.Errorf("commit: %w", err)
	}

	return s.Conversation(ctx, uuid.UUID(row.ID.Bytes))
}

func (s *PostgresStore) Direct(ctx context.Context, directKey string) (Conversation, error) {
	row, err := s.q.GetDirectConversation(ctx, text(directKey))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Conversation{}, ErrNotFound
		}
		return Conversation{}, fmt.Errorf("get direct conversation: %w", err)
	}
	return s.Conversation(ctx, uuid.UUID(row.ID.Bytes))
}

// Secret finds the secret chat of a pair of devices. It is a separate lookup
// because the direct key belongs to ordinary dialogues only.
func (s *PostgresStore) Secret(ctx context.Context, key string) (Conversation, error) {
	row, err := s.q.GetSecretConversation(ctx, text(key))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Conversation{}, ErrNotFound
		}
		return Conversation{}, fmt.Errorf("get secret conversation: %w", err)
	}
	return s.Conversation(ctx, uuid.UUID(row.ID.Bytes))
}

// MemberIDs is who belongs to a conversation.
func (s *PostgresStore) MemberIDs(ctx context.Context, channelID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.ListConversationMemberIDs(ctx, pgUUID(channelID))
	if err != nil {
		return nil, fmt.Errorf("list conversation members: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		out = append(out, uuid.UUID(row.Bytes))
	}
	return out, nil
}

func (s *PostgresStore) Conversation(ctx context.Context, id uuid.UUID) (Conversation, error) {
	row, err := s.q.GetChannel(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Conversation{}, ErrNotFound
		}
		return Conversation{}, fmt.Errorf("get conversation: %w", err)
	}
	if row.WorkspaceID.Valid {
		// A channel of a server is not a conversation.
		return Conversation{}, ErrNotFound
	}

	members, err := s.members(ctx, id)
	if err != nil {
		return Conversation{}, err
	}
	return Conversation{
		ID:        id,
		Kind:      Kind(row.Kind),
		Members:   members,
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

func (s *PostgresStore) List(ctx context.Context, userID uuid.UUID) ([]Conversation, error) {
	rows, err := s.q.ListUserConversations(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	out := make([]Conversation, 0, len(rows))
	for _, row := range rows {
		id := uuid.UUID(row.Channel.ID.Bytes)
		members, err := s.members(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, Conversation{
			ID:        id,
			Kind:      Kind(row.Channel.Kind),
			Members:   members,
			CreatedAt: row.Channel.CreatedAt.Time,
		})
	}
	return out, nil
}

func (s *PostgresStore) IsParticipant(ctx context.Context, channelID, userID uuid.UUID) (bool, error) {
	inside, err := s.q.IsChannelMember(ctx, db.IsChannelMemberParams{
		ChannelID: pgUUID(channelID),
		UserID:    pgUUID(userID),
	})
	if err != nil {
		return false, fmt.Errorf("is channel member: %w", err)
	}
	return inside, nil
}

func (s *PostgresStore) KnownUsers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	pgIDs := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		pgIDs = append(pgIDs, pgUUID(id))
	}
	rows, err := s.q.ListKnownUsers(ctx, pgIDs)
	if err != nil {
		return nil, fmt.Errorf("list known users: %w", err)
	}
	known := make(map[uuid.UUID]bool, len(rows))
	for _, row := range rows {
		known[uuid.UUID(row.Bytes)] = true
	}
	return known, nil
}

func (s *PostgresStore) members(ctx context.Context, channelID uuid.UUID) ([]Member, error) {
	rows, err := s.q.ListConversationMembers(ctx, pgUUID(channelID))
	if err != nil {
		return nil, fmt.Errorf("list conversation members: %w", err)
	}
	out := make([]Member, 0, len(rows))
	for _, row := range rows {
		out = append(out, Member{
			// Only the id is known here: Service fills the public part in one batch.
			Public:   profile.Public{ID: uuid.UUID(row.UserID.Bytes)},
			JoinedAt: row.JoinedAt.Time,
		})
	}
	return out, nil
}

func (s *PostgresStore) SharesConversation(ctx context.Context, a, b uuid.UUID) (bool, error) {
	shared, err := s.q.HasSharedConversation(ctx, db.HasSharedConversationParams{
		A: pgUUID(a),
		B: pgUUID(b),
	})
	if err != nil {
		return false, fmt.Errorf("has shared conversation: %w", err)
	}
	return shared, nil
}

func (s *PostgresStore) Peers(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.ListConversationPeers(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list conversation peers: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		out = append(out, uuid.UUID(row.Bytes))
	}
	return out, nil
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
