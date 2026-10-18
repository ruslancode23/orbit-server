package invites

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

func (s *PostgresStore) Create(ctx context.Context, workspaceID, createdBy uuid.UUID, code string, expiresAt *time.Time, maxUses *int) (Invite, error) {
	row, err := s.q.CreateInvite(ctx, db.CreateInviteParams{
		WorkspaceID: pgUUID(workspaceID),
		Code:        code,
		CreatedBy:   pgUUID(createdBy),
		ExpiresAt:   pgTime(expiresAt),
		MaxUses:     pgInt4(maxUses),
	})
	if err != nil {
		return Invite{}, fmt.Errorf("create invite: %w", err)
	}
	return toInvite(row), nil
}

func (s *PostgresStore) Live(ctx context.Context, code string) (Invite, error) {
	row, err := s.q.GetLiveInvite(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown, expired, revoked or used up: all the same from outside.
			return Invite{}, ErrNotFound
		}
		return Invite{}, fmt.Errorf("get live invite: %w", err)
	}
	return toInvite(row), nil
}

// Use counts one pass through the link. The query checks the limits again, so
// two people racing for the last use cannot both get in.
func (s *PostgresStore) Use(ctx context.Context, id uuid.UUID) (Invite, error) {
	row, err := s.q.UseInvite(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Invite{}, ErrNotFound
		}
		return Invite{}, fmt.Errorf("use invite: %w", err)
	}
	return toInvite(row), nil
}

func (s *PostgresStore) Revoke(ctx context.Context, id uuid.UUID) error {
	affected, err := s.q.RevokeInvite(ctx, pgUUID(id))
	if err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Invite reads one link whatever its state: revoking a dead link is allowed.
func (s *PostgresStore) Invite(ctx context.Context, id uuid.UUID) (Invite, error) {
	row, err := s.q.GetInvite(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Invite{}, ErrNotFound
		}
		return Invite{}, fmt.Errorf("get invite: %w", err)
	}
	return toInvite(row), nil
}

func (s *PostgresStore) List(ctx context.Context, workspaceID uuid.UUID) ([]Invite, error) {
	rows, err := s.q.ListWorkspaceInvites(ctx, pgUUID(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	out := make([]Invite, 0, len(rows))
	for _, row := range rows {
		out = append(out, toInvite(row))
	}
	return out, nil
}

func toInvite(row db.Invite) Invite {
	invite := Invite{
		ID:          uuid.UUID(row.ID.Bytes),
		WorkspaceID: uuid.UUID(row.WorkspaceID.Bytes),
		Code:        row.Code,
		Uses:        int(row.Uses),
		CreatedAt:   row.CreatedAt.Time,
	}
	if row.ExpiresAt.Valid {
		expiresAt := row.ExpiresAt.Time
		invite.ExpiresAt = &expiresAt
	}
	if row.MaxUses.Valid {
		maxUses := int(row.MaxUses.Int32)
		invite.MaxUses = &maxUses
	}
	return invite
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func pgTime(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func pgInt4(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true} //nolint:gosec // capped at maxUses by the service
}
