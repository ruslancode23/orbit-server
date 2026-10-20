package friends

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

// PostgresStore is the SQL behind friendships. The friends module owns the
// table; nobody else reads or writes it directly.
type PostgresStore struct {
	q db.Querier
}

func NewPostgresStore(q db.Querier) *PostgresStore { return &PostgresStore{q: q} }

var _ Store = (*PostgresStore)(nil)

func (s *PostgresStore) UserByNickname(ctx context.Context, nickname string) (User, error) {
	row, err := s.q.UserByNickname(ctx, nickname)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("find user by nickname: %w", err)
	}
	return User{ID: uuid.UUID(row.ID.Bytes), Nickname: row.Nickname, IsGuest: row.IsGuest}, nil
}

func (s *PostgresStore) UserByID(ctx context.Context, id uuid.UUID) (User, error) {
	row, err := s.q.UserByID(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("find user by id: %w", err)
	}
	return User{ID: uuid.UUID(row.ID.Bytes), Nickname: row.Nickname, IsGuest: row.IsGuest}, nil
}

func (s *PostgresStore) Pair(ctx context.Context, low, high uuid.UUID) (Friendship, error) {
	row, err := s.q.FriendshipPair(ctx, db.FriendshipPairParams{
		UserLow:  pgUUID(low),
		UserHigh: pgUUID(high),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Friendship{}, ErrNotFound
		}
		return Friendship{}, fmt.Errorf("read friendship pair: %w", err)
	}
	return Friendship{
		Low:          uuid.UUID(row.UserLow.Bytes),
		High:         uuid.UUID(row.UserHigh.Bytes),
		Status:       Status(row.Status),
		RequestedBy:  uuid.UUID(row.RequestedBy.Bytes),
		CreatedAt:    row.CreatedAt.Time,
		AcceptedAt:   row.AcceptedAt.Time,
		LowNickname:  row.LowNickname,
		HighNickname: row.HighNickname,
		LowIsGuest:   row.LowIsGuest,
		HighIsGuest:  row.HighIsGuest,
	}, nil
}

func (s *PostgresStore) CreateRequest(ctx context.Context, low, high, requestedBy uuid.UUID) (time.Time, bool, error) {
	createdAt, err := s.q.CreateFriendshipRequest(ctx, db.CreateFriendshipRequestParams{
		UserLow:     pgUUID(low),
		UserHigh:    pgUUID(high),
		RequestedBy: pgUUID(requestedBy),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("create friendship request: %w", err)
	}
	return createdAt.Time, true, nil
}

func (s *PostgresStore) Accept(ctx context.Context, low, high uuid.UUID) (time.Time, error) {
	row, err := s.q.AcceptFriendship(ctx, db.AcceptFriendshipParams{
		UserLow:  pgUUID(low),
		UserHigh: pgUUID(high),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, ErrNotFound
		}
		return time.Time{}, fmt.Errorf("accept friendship: %w", err)
	}
	return row.AcceptedAt.Time, nil
}

func (s *PostgresStore) DeletePending(ctx context.Context, low, high uuid.UUID) (bool, error) {
	rows, err := s.q.DeletePendingFriendship(ctx, db.DeletePendingFriendshipParams{
		UserLow:  pgUUID(low),
		UserHigh: pgUUID(high),
	})
	if err != nil {
		return false, fmt.Errorf("delete pending friendship: %w", err)
	}
	return rows > 0, nil
}

func (s *PostgresStore) DeleteAccepted(ctx context.Context, low, high uuid.UUID) (bool, error) {
	rows, err := s.q.DeleteAcceptedFriendship(ctx, db.DeleteAcceptedFriendshipParams{
		UserLow:  pgUUID(low),
		UserHigh: pgUUID(high),
	})
	if err != nil {
		return false, fmt.Errorf("delete accepted friendship: %w", err)
	}
	return rows > 0, nil
}

func (s *PostgresStore) Delete(ctx context.Context, low, high uuid.UUID) (bool, error) {
	rows, err := s.q.DeleteFriendship(ctx, db.DeleteFriendshipParams{
		UserLow:  pgUUID(low),
		UserHigh: pgUUID(high),
	})
	if err != nil {
		return false, fmt.Errorf("delete friendship: %w", err)
	}
	return rows > 0, nil
}

func (s *PostgresStore) Incoming(ctx context.Context, userID uuid.UUID) ([]Request, error) {
	rows, err := s.q.ListIncomingFriendRequests(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list incoming requests: %w", err)
	}
	out := make([]Request, 0, len(rows))
	for _, row := range rows {
		out = append(out, Request{
			User:      User{ID: uuid.UUID(row.UserID.Bytes), Nickname: row.Nickname},
			Direction: DirectionIncoming,
			CreatedAt: row.CreatedAt.Time,
		})
	}
	return out, nil
}

func (s *PostgresStore) Outgoing(ctx context.Context, userID uuid.UUID) ([]Request, error) {
	rows, err := s.q.ListOutgoingFriendRequests(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list outgoing requests: %w", err)
	}
	out := make([]Request, 0, len(rows))
	for _, row := range rows {
		out = append(out, Request{
			User:      User{ID: uuid.UUID(row.UserID.Bytes), Nickname: row.Nickname},
			Direction: DirectionOutgoing,
			CreatedAt: row.CreatedAt.Time,
		})
	}
	return out, nil
}

func (s *PostgresStore) Friends(ctx context.Context, userID uuid.UUID) ([]Friend, error) {
	rows, err := s.q.ListFriends(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list friends: %w", err)
	}
	out := make([]Friend, 0, len(rows))
	for _, row := range rows {
		out = append(out, Friend{
			ID:       uuid.UUID(row.ID.Bytes),
			Nickname: row.Nickname,
			Since:    row.AcceptedAt.Time,
		})
	}
	return out, nil
}

func (s *PostgresStore) CountFriends(ctx context.Context, userID uuid.UUID) (int, error) {
	count, err := s.q.CountFriends(ctx, pgUUID(userID))
	if err != nil {
		return 0, fmt.Errorf("count friends: %w", err)
	}
	return int(count), nil
}

func (s *PostgresStore) CountPending(ctx context.Context, userID uuid.UUID) (int, error) {
	count, err := s.q.CountOutgoingPendingRequests(ctx, pgUUID(userID))
	if err != nil {
		return 0, fmt.Errorf("count outgoing requests: %w", err)
	}
	return int(count), nil
}

func (s *PostgresStore) FriendsOf(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.FriendsOf(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list friend ids: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		out = append(out, uuid.UUID(row.Bytes))
	}
	return out, nil
}

func (s *PostgresStore) AreFriends(ctx context.Context, a, b uuid.UUID) (bool, error) {
	friends, err := s.q.AreFriends(ctx, db.AreFriendsParams{A: pgUUID(a), B: pgUUID(b)})
	if err != nil {
		return false, fmt.Errorf("check friendship: %w", err)
	}
	return friends, nil
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
