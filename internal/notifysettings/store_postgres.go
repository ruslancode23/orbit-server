package notifysettings

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

// PostgresStore keeps the rows in notification_settings.
type PostgresStore struct {
	q db.Querier
}

// NewPostgresStore returns a store over the generated queries.
func NewPostgresStore(q db.Querier) *PostgresStore { return &PostgresStore{q: q} }

var _ Store = (*PostgresStore)(nil)

func (s *PostgresStore) List(ctx context.Context, userID uuid.UUID) ([]Setting, error) {
	rows, err := s.q.ListNotificationSettings(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list notification settings: %w", err)
	}
	return toSettings(rows), nil
}

func (s *PostgresStore) Get(ctx context.Context, userID uuid.UUID, scope Scope, targetID uuid.UUID) (Setting, bool, error) {
	row, err := s.q.GetNotificationSetting(ctx, db.GetNotificationSettingParams{
		UserID:   pgUUID(userID),
		Scope:    string(scope),
		TargetID: pgUUID(targetID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Setting{}, false, nil
	}
	if err != nil {
		return Setting{}, false, fmt.Errorf("get notification setting: %w", err)
	}
	return toSetting(row), true, nil
}

func (s *PostgresStore) Upsert(ctx context.Context, setting Setting) (Setting, error) {
	row, err := s.q.UpsertNotificationSetting(ctx, db.UpsertNotificationSettingParams{
		UserID:     pgUUID(setting.UserID),
		Scope:      string(setting.Scope),
		TargetID:   pgUUID(setting.TargetID),
		Level:      toText(setting.Level),
		MutedUntil: toTimestamp(setting.MutedUntil),
	})
	if err != nil {
		return Setting{}, fmt.Errorf("upsert notification setting: %w", err)
	}
	return toSetting(row), nil
}

func (s *PostgresStore) Delete(ctx context.Context, userID uuid.UUID, scope Scope, targetID uuid.UUID) (bool, error) {
	n, err := s.q.DeleteNotificationSetting(ctx, db.DeleteNotificationSettingParams{
		UserID:   pgUUID(userID),
		Scope:    string(scope),
		TargetID: pgUUID(targetID),
	})
	if err != nil {
		return false, fmt.Errorf("delete notification setting: %w", err)
	}
	return n > 0, nil
}

func (s *PostgresStore) Count(ctx context.Context, userID uuid.UUID) (int, error) {
	n, err := s.q.CountNotificationSettings(ctx, pgUUID(userID))
	if err != nil {
		return 0, fmt.Errorf("count notification settings: %w", err)
	}
	return int(n), nil
}

func (s *PostgresStore) ForTargets(ctx context.Context, targetIDs []uuid.UUID) ([]Setting, error) {
	rows, err := s.q.ListNotificationSettingsForTargets(ctx, pgUUIDs(targetIDs))
	if err != nil {
		return nil, fmt.Errorf("list notification settings for targets: %w", err)
	}
	return toSettings(rows), nil
}

func (s *PostgresStore) ForUserTargets(ctx context.Context, userID uuid.UUID, targetIDs []uuid.UUID) ([]Setting, error) {
	rows, err := s.q.ListNotificationSettingsForUserTargets(ctx, db.ListNotificationSettingsForUserTargetsParams{
		UserID:    pgUUID(userID),
		TargetIds: pgUUIDs(targetIDs),
	})
	if err != nil {
		return nil, fmt.Errorf("list notification settings for one account: %w", err)
	}
	return toSettings(rows), nil
}

func toSettings(rows []db.NotificationSetting) []Setting {
	out := make([]Setting, 0, len(rows))
	for _, row := range rows {
		out = append(out, toSetting(row))
	}
	return out
}

func toSetting(row db.NotificationSetting) Setting {
	return Setting{
		UserID:     uuid.UUID(row.UserID.Bytes),
		Scope:      Scope(row.Scope),
		TargetID:   uuid.UUID(row.TargetID.Bytes),
		Level:      fromText(row.Level),
		MutedUntil: fromTimestamp(row.MutedUntil),
		UpdatedAt:  row.UpdatedAt.Time,
	}
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func pgUUIDs(ids []uuid.UUID) []pgtype.UUID {
	out := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		out = append(out, pgUUID(id))
	}
	return out
}

func toText(level *Level) pgtype.Text {
	if level == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: string(*level), Valid: true}
}

func fromText(text pgtype.Text) *Level {
	if !text.Valid {
		return nil
	}
	level := Level(text.String)
	return &level
}

func toTimestamp(at *time.Time) pgtype.Timestamptz {
	if at == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *at, Valid: true}
}

func fromTimestamp(at pgtype.Timestamptz) *time.Time {
	if !at.Valid {
		return nil
	}
	out := at.Time
	return &out
}
