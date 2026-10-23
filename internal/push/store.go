package push

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

func (s *PostgresStore) Upsert(ctx context.Context, sub Subscription) (Subscription, error) {
	row, err := s.q.UpsertPushSubscription(ctx, db.UpsertPushSubscriptionParams{
		ID:       pgUUID(sub.ID),
		UserID:   pgUUID(sub.UserID),
		Endpoint: sub.Endpoint,
		P256dh:   sub.P256dh,
		Auth:     sub.Auth,
	})
	if err != nil {
		return Subscription{}, fmt.Errorf("upsert push subscription: %w", err)
	}
	return toSubscription(row), nil
}

func (s *PostgresStore) Delete(ctx context.Context, id, userID uuid.UUID) (bool, error) {
	n, err := s.q.DeletePushSubscription(ctx, db.DeletePushSubscriptionParams{ID: pgUUID(id), UserID: pgUUID(userID)})
	if err != nil {
		return false, fmt.Errorf("delete push subscription: %w", err)
	}
	return n > 0, nil
}

func (s *PostgresStore) DeleteEndpoint(ctx context.Context, endpoint string) error {
	if err := s.q.DeletePushSubscriptionByEndpoint(ctx, endpoint); err != nil {
		return fmt.Errorf("delete push subscription by endpoint: %w", err)
	}
	return nil
}

func (s *PostgresStore) ForUsers(ctx context.Context, userIDs []uuid.UUID) ([]Subscription, error) {
	ids := make([]pgtype.UUID, 0, len(userIDs))
	for _, id := range userIDs {
		ids = append(ids, pgUUID(id))
	}
	rows, err := s.q.ListPushSubscriptionsForUsers(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list push subscriptions: %w", err)
	}
	out := make([]Subscription, 0, len(rows))
	for _, row := range rows {
		out = append(out, toSubscription(row))
	}
	return out, nil
}

func (s *PostgresStore) Touch(ctx context.Context, endpoint string) error {
	if err := s.q.TouchPushSubscription(ctx, endpoint); err != nil {
		return fmt.Errorf("touch push subscription: %w", err)
	}
	return nil
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func toSubscription(row db.PushSubscription) Subscription {
	return Subscription{
		ID:       row.ID.Bytes,
		UserID:   row.UserID.Bytes,
		Endpoint: row.Endpoint,
		P256dh:   row.P256dh,
		Auth:     row.Auth,
	}
}
