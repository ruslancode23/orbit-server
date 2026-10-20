package privacy

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
)

// PostgresStore is the SQL behind the rules: users.dm_policy and user_blocks.
type PostgresStore struct {
	q db.Querier
}

func NewPostgresStore(q db.Querier) *PostgresStore { return &PostgresStore{q: q} }

var _ Store = (*PostgresStore)(nil)

func (s *PostgresStore) UserPrivacy(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Account, error) {
	rows, err := s.q.GetUserPrivacy(ctx, toPgUUIDs(ids))
	if err != nil {
		return nil, fmt.Errorf("get user privacy: %w", err)
	}
	out := make(map[uuid.UUID]Account, len(rows))
	for _, row := range rows {
		out[uuid.UUID(row.ID.Bytes)] = Account{
			Policy:              Policy(row.DmPolicy),
			FriendRequestPolicy: FriendRequestPolicy(row.FriendRequestPolicy),
			IsGuest:             row.IsGuest,
		}
	}
	return out, nil
}

func (s *PostgresStore) HasSharedWorkspace(ctx context.Context, a, b uuid.UUID) (bool, error) {
	shared, err := s.q.HasSharedWorkspace(ctx, db.HasSharedWorkspaceParams{
		A: pgUUID(a),
		B: pgUUID(b),
	})
	if err != nil {
		return false, fmt.Errorf("has shared workspace: %w", err)
	}
	return shared, nil
}

func (s *PostgresStore) BlockedEitherWay(ctx context.Context, a, b uuid.UUID) (bool, error) {
	blocked, err := s.q.IsBlockedEitherWay(ctx, db.IsBlockedEitherWayParams{
		A: pgUUID(a),
		B: pgUUID(b),
	})
	if err != nil {
		return false, fmt.Errorf("is blocked either way: %w", err)
	}
	return blocked, nil
}

func (s *PostgresStore) BlockedIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.ListBlockedIDs(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list blocked ids: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		out = append(out, uuid.UUID(row.Bytes))
	}
	return out, nil
}

func (s *PostgresStore) ListBlocks(ctx context.Context, userID uuid.UUID) ([]BlockedUser, error) {
	rows, err := s.q.ListBlockedUsers(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list blocked users: %w", err)
	}
	out := make([]BlockedUser, 0, len(rows))
	for _, row := range rows {
		out = append(out, BlockedUser{
			ID:        uuid.UUID(row.ID.Bytes),
			Nickname:  row.Nickname,
			BlockedAt: row.CreatedAt.Time,
		})
	}
	return out, nil
}

func (s *PostgresStore) Block(ctx context.Context, blockerID, blockedID uuid.UUID) error {
	if err := s.q.BlockUser(ctx, db.BlockUserParams{
		BlockerID: pgUUID(blockerID),
		BlockedID: pgUUID(blockedID),
	}); err != nil {
		return fmt.Errorf("block user: %w", err)
	}
	return nil
}

func (s *PostgresStore) Unblock(ctx context.Context, blockerID, blockedID uuid.UUID) error {
	if err := s.q.UnblockUser(ctx, db.UnblockUserParams{
		BlockerID: pgUUID(blockerID),
		BlockedID: pgUUID(blockedID),
	}); err != nil {
		return fmt.Errorf("unblock user: %w", err)
	}
	return nil
}

func (s *PostgresStore) UserExists(ctx context.Context, id uuid.UUID) (bool, error) {
	exists, err := s.q.UserExists(ctx, pgUUID(id))
	if err != nil {
		return false, fmt.Errorf("user exists: %w", err)
	}
	return exists, nil
}

func (s *PostgresStore) Policy(ctx context.Context, userID uuid.UUID) (Policy, error) {
	policy, err := s.q.GetDMPolicy(ctx, pgUUID(userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("get dm policy: %w", err)
	}
	return Policy(policy), nil
}

func (s *PostgresStore) SetFriendRequestPolicy(ctx context.Context, userID uuid.UUID, policy FriendRequestPolicy) error {
	if err := s.q.SetFriendRequestPolicy(ctx, db.SetFriendRequestPolicyParams{
		ID:                  pgUUID(userID),
		FriendRequestPolicy: string(policy),
	}); err != nil {
		return fmt.Errorf("set friend request policy: %w", err)
	}
	return nil
}

func (s *PostgresStore) SetPolicy(ctx context.Context, userID uuid.UUID, policy Policy) error {
	if err := s.q.SetDMPolicy(ctx, db.SetDMPolicyParams{
		ID:       pgUUID(userID),
		DmPolicy: string(policy),
	}); err != nil {
		return fmt.Errorf("set dm policy: %w", err)
	}
	return nil
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func toPgUUIDs(ids []uuid.UUID) []pgtype.UUID {
	out := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		out = append(out, pgUUID(id))
	}
	return out
}
