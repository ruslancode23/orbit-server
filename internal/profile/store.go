package profile

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

// PostgresStore implements Store over the sqlc queries of db/queries/profile.sql.
type PostgresStore struct {
	q db.Querier
}

func NewPostgresStore(q db.Querier) *PostgresStore { return &PostgresStore{q: q} }

var _ Store = (*PostgresStore)(nil)

func (s *PostgresStore) Profile(ctx context.Context, userID uuid.UUID) (Stored, error) {
	row, err := s.q.GetUserProfile(ctx, pgUUID(userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Stored{}, ErrNotFound
		}
		return Stored{}, fmt.Errorf("get profile: %w", err)
	}
	return stored(columns{
		id: row.ID, nickname: row.Nickname, displayName: row.DisplayName,
		bio: row.Bio, accentColor: row.AccentColor, avatarKey: row.AvatarKey,
		statusEmoji: row.CustomStatusEmoji, statusText: row.CustomStatusText,
		statusExpiresAt: row.CustomStatusExpiresAt, isGuest: row.IsGuest, createdAt: row.CreatedAt,
	}), nil
}

func (s *PostgresStore) Profiles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Stored, error) {
	rows, err := s.q.ListUserProfiles(ctx, toPgUUIDs(ids))
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	out := make(map[uuid.UUID]Stored, len(rows))
	for _, row := range rows {
		profile := stored(columns{
			id: row.ID, nickname: row.Nickname, displayName: row.DisplayName,
			bio: row.Bio, accentColor: row.AccentColor, avatarKey: row.AvatarKey,
			statusEmoji: row.CustomStatusEmoji, statusText: row.CustomStatusText,
			statusExpiresAt: row.CustomStatusExpiresAt, isGuest: row.IsGuest, createdAt: row.CreatedAt,
		})
		out[profile.ID] = profile
	}
	return out, nil
}

func (s *PostgresStore) UpdateProfile(ctx context.Context, userID uuid.UUID, u Update) (Stored, error) {
	params := db.UpdateUserProfileParams{ID: pgUUID(userID)}
	if u.DisplayName != nil {
		params.SetDisplayName = true
		// An empty name becomes NULL: the display name falls back to the nickname.
		params.DisplayName = text(*u.DisplayName)
	}
	if u.Bio != nil {
		params.SetBio = true
		// bio is NOT NULL: clearing it stores an empty string, not NULL.
		params.Bio = pgtype.Text{String: *u.Bio, Valid: true}
	}
	if u.AccentColor != nil {
		params.AccentColor = pgtype.Text{String: string(*u.AccentColor), Valid: true}
	}

	row, err := s.q.UpdateUserProfile(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Stored{}, ErrNotFound
		}
		return Stored{}, fmt.Errorf("update profile: %w", err)
	}
	return stored(columns{
		id: row.ID, nickname: row.Nickname, displayName: row.DisplayName,
		bio: row.Bio, accentColor: row.AccentColor, avatarKey: row.AvatarKey,
		statusEmoji: row.CustomStatusEmoji, statusText: row.CustomStatusText,
		statusExpiresAt: row.CustomStatusExpiresAt, isGuest: row.IsGuest, createdAt: row.CreatedAt,
	}), nil
}

func (s *PostgresStore) SetAvatar(ctx context.Context, userID uuid.UUID, key string) (Stored, string, error) {
	row, err := s.q.SetUserAvatar(ctx, db.SetUserAvatarParams{
		AvatarKey: text(key),
		ID:        pgUUID(userID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Stored{}, "", ErrNotFound
		}
		return Stored{}, "", fmt.Errorf("set avatar: %w", err)
	}
	return stored(columns{
		id: row.ID, nickname: row.Nickname, displayName: row.DisplayName,
		bio: row.Bio, accentColor: row.AccentColor, avatarKey: row.AvatarKey,
		statusEmoji: row.CustomStatusEmoji, statusText: row.CustomStatusText,
		statusExpiresAt: row.CustomStatusExpiresAt, isGuest: row.IsGuest, createdAt: row.CreatedAt,
	}), row.PreviousAvatarKey.String, nil
}

func (s *PostgresStore) SetStatus(ctx context.Context, userID uuid.UUID, emoji, textValue string, expiresAt *time.Time) (Stored, error) {
	row, err := s.q.SetUserStatus(ctx, db.SetUserStatusParams{
		Emoji:      text(emoji),
		StatusText: text(textValue),
		ExpiresAt:  pgTimePtr(expiresAt),
		ID:         pgUUID(userID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Stored{}, ErrNotFound
		}
		return Stored{}, fmt.Errorf("set status: %w", err)
	}
	return stored(columns{
		id: row.ID, nickname: row.Nickname, displayName: row.DisplayName,
		bio: row.Bio, accentColor: row.AccentColor, avatarKey: row.AvatarKey,
		statusEmoji: row.CustomStatusEmoji, statusText: row.CustomStatusText,
		statusExpiresAt: row.CustomStatusExpiresAt, isGuest: row.IsGuest, createdAt: row.CreatedAt,
	}), nil
}

func (s *PostgresStore) ClearStatus(ctx context.Context, userID uuid.UUID) (Stored, error) {
	row, err := s.q.ClearUserStatus(ctx, pgUUID(userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Stored{}, ErrNotFound
		}
		return Stored{}, fmt.Errorf("clear status: %w", err)
	}
	return stored(columns{
		id: row.ID, nickname: row.Nickname, displayName: row.DisplayName,
		bio: row.Bio, accentColor: row.AccentColor, avatarKey: row.AvatarKey,
		statusEmoji: row.CustomStatusEmoji, statusText: row.CustomStatusText,
		statusExpiresAt: row.CustomStatusExpiresAt, isGuest: row.IsGuest, createdAt: row.CreatedAt,
	}), nil
}

// columns is the list every profile query selects. Keeping the mapping in one
// place means a new column changes one function, not five.
type columns struct {
	id              pgtype.UUID
	nickname        string
	displayName     pgtype.Text
	bio             string
	accentColor     string
	avatarKey       pgtype.Text
	statusEmoji     pgtype.Text
	statusText      pgtype.Text
	statusExpiresAt pgtype.Timestamptz
	isGuest         bool
	createdAt       pgtype.Timestamptz
}

func stored(c columns) Stored {
	out := Stored{
		Public: Public{
			ID:          uuid.UUID(c.id.Bytes),
			Nickname:    c.nickname,
			DisplayName: c.nickname,
			Bio:         c.bio,
			AccentColor: AccentColor(c.accentColor),
			IsGuest:     c.isGuest,
			CreatedAt:   c.createdAt.Time,
		},
		AvatarKey: c.avatarKey.String,
	}
	if c.displayName.Valid {
		out.DisplayName = c.displayName.String
	}
	if c.statusEmoji.Valid || c.statusText.Valid || c.statusExpiresAt.Valid {
		status := &CustomStatus{Emoji: c.statusEmoji.String, Text: c.statusText.String}
		if c.statusExpiresAt.Valid {
			expiresAt := c.statusExpiresAt.Time
			status.ExpiresAt = &expiresAt
		}
		out.CustomStatus = status
	}
	return out
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

// text maps "" to SQL NULL, which is how an unset display name, avatar key or
// status part is stored.
func text(value string) pgtype.Text {
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}

func pgTimePtr(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *value, Valid: true}
}
