package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
)

// uniqueViolation is PostgreSQL's SQLSTATE for a broken unique constraint.
const uniqueViolation = "23505"

// PostgresStore implements Store over the sqlc queries.
type PostgresStore struct {
	q db.Querier
}

func NewPostgresStore(q db.Querier) *PostgresStore { return &PostgresStore{q: q} }

var _ Store = (*PostgresStore)(nil)

func (s *PostgresStore) CreateUser(ctx context.Context, u NewUser) (User, error) {
	row, err := s.q.CreateUser(ctx, db.CreateUserParams{
		Nickname:     u.Nickname,
		Email:        text(u.Email),
		PasswordHash: text(u.PasswordHash),
		IsGuest:      u.IsGuest,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return User{}, ErrTaken
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return toUser(row), nil
}

func (s *PostgresStore) UserByLogin(ctx context.Context, login string) (StoredUser, error) {
	row, err := s.q.GetUserByLogin(ctx, login)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StoredUser{}, ErrNoUser
		}
		return StoredUser{}, fmt.Errorf("get user by login: %w", err)
	}
	return StoredUser{User: toUser(row), PasswordHash: row.PasswordHash.String}, nil
}

func (s *PostgresStore) UserByID(ctx context.Context, id uuid.UUID) (StoredUser, error) {
	row, err := s.q.GetUser(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StoredUser{}, ErrNoUser
		}
		return StoredUser{}, fmt.Errorf("get user: %w", err)
	}
	return StoredUser{User: toUser(row), PasswordHash: row.PasswordHash.String}, nil
}

func (s *PostgresStore) CreateSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, expiresAt time.Time) (uuid.UUID, error) {
	row, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		UserID:    pgUUID(userID),
		TokenHash: tokenHash,
		ExpiresAt: pgTime(expiresAt),
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("create session: %w", err)
	}
	return uuid.UUID(row.ID.Bytes), nil
}

func (s *PostgresStore) LiveSession(ctx context.Context, tokenHash []byte) (Session, error) {
	row, err := s.q.GetLiveSessionUser(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrNoSession
		}
		return Session{}, fmt.Errorf("get live session: %w", err)
	}
	return Session{
		ID:        uuid.UUID(row.SessionID.Bytes),
		User:      toUser(row.User),
		ExpiresAt: row.ExpiresAt.Time,
	}, nil
}

func (s *PostgresStore) TouchSession(ctx context.Context, id uuid.UUID) error {
	if err := s.q.TouchSession(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

func (s *PostgresStore) RevokeSession(ctx context.Context, id uuid.UUID) error {
	if err := s.q.RevokeSession(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

func (s *PostgresStore) RevokeUserSessions(ctx context.Context, userID uuid.UUID) error {
	if err := s.q.RevokeUserSessions(ctx, pgUUID(userID)); err != nil {
		return fmt.Errorf("revoke user sessions: %w", err)
	}
	return nil
}

func (s *PostgresStore) SetPassword(ctx context.Context, userID uuid.UUID, passwordHash string) error {
	err := s.q.SetUserPassword(ctx, db.SetUserPasswordParams{
		ID:           pgUUID(userID),
		PasswordHash: text(passwordHash),
	})
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	return nil
}

func toUser(row db.User) User {
	return User{
		ID:        uuid.UUID(row.ID.Bytes),
		Nickname:  row.Nickname,
		Email:     row.Email.String,
		IsGuest:   row.IsGuest,
		CreatedAt: row.CreatedAt.Time,
	}
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func pgTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// text maps "" to SQL NULL: an account without an email stores NULL, not an empty string,
// so the unique index on email does not collide on it.
func text(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
