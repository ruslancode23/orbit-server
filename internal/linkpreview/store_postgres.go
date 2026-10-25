package linkpreview

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
)

// PostgresStore keeps the preview cache and the message ↔ preview links.
type PostgresStore struct {
	q db.Querier
}

// NewPostgresStore returns a store over the generated queries.
func NewPostgresStore(q db.Querier) *PostgresStore { return &PostgresStore{q: q} }

var _ Store = (*PostgresStore)(nil)

func (s *PostgresStore) Preview(ctx context.Context, normalizedURL string) (cachedPreview, error) {
	row, err := s.q.GetLinkPreview(ctx, normalizedURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return cachedPreview{}, ErrNotFound
	}
	if err != nil {
		return cachedPreview{}, fmt.Errorf("get link preview: %w", err)
	}
	return toCached(row), nil
}

func (s *PostgresStore) Save(ctx context.Context, preview cachedPreview) (cachedPreview, error) {
	row, err := s.q.UpsertLinkPreview(ctx, db.UpsertLinkPreviewParams{
		ID:          pgUUID(preview.ID),
		Url:         preview.URL,
		Title:       text(preview.Title),
		Description: text(preview.Description),
		SiteName:    text(preview.SiteName),
		ImageUrl:    text(preview.ImageURL),
		Status:      preview.Status,
	})
	if err != nil {
		return cachedPreview{}, fmt.Errorf("upsert link preview: %w", err)
	}
	return toCached(row), nil
}

func (s *PostgresStore) Attach(ctx context.Context, messageID, previewID uuid.UUID) error {
	if err := s.q.AttachMessagePreview(ctx, db.AttachMessagePreviewParams{
		MessageID: pgUUID(messageID),
		PreviewID: pgUUID(previewID),
	}); err != nil {
		return fmt.Errorf("attach message preview: %w", err)
	}
	return nil
}

func (s *PostgresStore) Detach(ctx context.Context, messageID uuid.UUID) error {
	if err := s.q.DeleteMessagePreview(ctx, pgUUID(messageID)); err != nil {
		return fmt.Errorf("delete message preview: %w", err)
	}
	return nil
}

func (s *PostgresStore) ForMessages(ctx context.Context, messageIDs []uuid.UUID) (map[uuid.UUID]Preview, error) {
	rows, err := s.q.ListPreviewsForMessages(ctx, pgUUIDs(messageIDs))
	if err != nil {
		return nil, fmt.Errorf("list message previews: %w", err)
	}
	out := make(map[uuid.UUID]Preview, len(rows))
	for _, row := range rows {
		out[uuid.UUID(row.MessageID.Bytes)] = toPreview(row)
	}
	return out, nil
}

func (s *PostgresStore) ByID(ctx context.Context, id uuid.UUID) (cachedPreview, error) {
	row, err := s.q.GetLinkPreviewByID(ctx, pgUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return cachedPreview{}, ErrNotFound
	}
	if err != nil {
		return cachedPreview{}, fmt.Errorf("get link preview by id: %w", err)
	}
	return toCached(row), nil
}

func toCached(row db.LinkPreview) cachedPreview {
	return cachedPreview{
		Preview: Preview{
			ID:          uuid.UUID(row.ID.Bytes),
			URL:         row.Url,
			Title:       textValue(row.Title),
			Description: textValue(row.Description),
			SiteName:    textValue(row.SiteName),
			ImageURL:    textValue(row.ImageUrl),
		},
		Status:    row.Status,
		FetchedAt: row.FetchedAt.Time,
	}
}

func toPreview(row db.ListPreviewsForMessagesRow) Preview {
	return Preview{
		ID:          uuid.UUID(row.ID.Bytes),
		URL:         row.Url,
		Title:       textValue(row.Title),
		Description: textValue(row.Description),
		SiteName:    textValue(row.SiteName),
		ImageURL:    textValue(row.ImageUrl),
	}
}

func text(value string) pgtype.Text {
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}

func textValue(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func pgUUIDs(ids []uuid.UUID) []pgtype.UUID {
	out := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		out = append(out, pgUUID(id))
	}
	return out
}
