package files

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

func (s *PostgresStore) Create(ctx context.Context, a Attachment) (Attachment, error) {
	row, err := s.q.CreateAttachment(ctx, db.CreateAttachmentParams{
		UploaderID: pgUUID(a.UploaderID),
		ChannelID:  pgUUIDPtr(a.ChannelID),
		StorageKey: a.StorageKey,
		FileName:   a.FileName,
		MimeType:   a.MimeType,
		SizeBytes:  a.SizeBytes,
		Width:      pgInt4(a.Width),
		Height:     pgInt4(a.Height),
		ExpiresAt:  pgTimestamptz(a.ExpiresAt),
	})
	if err != nil {
		return Attachment{}, fmt.Errorf("create attachment: %w", err)
	}
	return toAttachment(row), nil
}

func (s *PostgresStore) Attachment(ctx context.Context, id uuid.UUID) (Attachment, error) {
	row, err := s.q.GetAttachment(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attachment{}, ErrNotFound
		}
		return Attachment{}, fmt.Errorf("get attachment: %w", err)
	}
	return toAttachment(row), nil
}

// Attach binds a free file of this uploader to a message. A file that is
// someone else's, or already used, simply is not found.
func (s *PostgresStore) Attach(ctx context.Context, id, messageID, uploaderID uuid.UUID) (Attachment, error) {
	row, err := s.q.AttachToMessage(ctx, db.AttachToMessageParams{
		ID:         pgUUID(id),
		MessageID:  pgUUID(messageID),
		UploaderID: pgUUID(uploaderID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attachment{}, ErrNotFound
		}
		return Attachment{}, fmt.Errorf("attach to message: %w", err)
	}
	return toAttachment(row), nil
}

func (s *PostgresStore) ByMessages(ctx context.Context, messageIDs []uuid.UUID) ([]Attachment, error) {
	ids := make([]pgtype.UUID, 0, len(messageIDs))
	for _, id := range messageIDs {
		ids = append(ids, pgUUID(id))
	}
	rows, err := s.q.ListMessageAttachments(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list message attachments: %w", err)
	}
	out := make([]Attachment, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAttachment(row))
	}
	return out, nil
}

func (s *PostgresStore) Orphans(ctx context.Context, olderThan time.Time, limit int) ([]Attachment, error) {
	rows, err := s.q.ListOrphanAttachments(ctx, db.ListOrphanAttachmentsParams{
		CreatedAt: pgtype.Timestamptz{Time: olderThan, Valid: true},
		Limit:     int32(limit), //nolint:gosec // a batch size the caller picks
	})
	if err != nil {
		return nil, fmt.Errorf("list orphan attachments: %w", err)
	}
	out := make([]Attachment, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAttachment(row))
	}
	return out, nil
}

// Delete removes a file that is still attached to nothing. The condition makes
// the sweep safe against a message that arrived a moment ago.
func (s *PostgresStore) Delete(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DetachOrphanedAttachment(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("delete attachment: %w", err)
	}
	return nil
}

func (s *PostgresStore) Expired(ctx context.Context, limit int) ([]Attachment, error) {
	rows, err := s.q.ListExpiredAttachments(ctx, int32(limit)) //nolint:gosec // a batch size the caller picks
	if err != nil {
		return nil, fmt.Errorf("list expired attachments: %w", err)
	}
	out := make([]Attachment, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAttachment(row))
	}
	return out, nil
}

func (s *PostgresStore) DeleteExpired(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteExpiredAttachment(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("delete expired attachment: %w", err)
	}
	return nil
}

func toAttachment(row db.Attachment) Attachment {
	attachment := Attachment{
		ID:         uuid.UUID(row.ID.Bytes),
		UploaderID: uuid.UUID(row.UploaderID.Bytes),
		StorageKey: row.StorageKey,
		FileName:   row.FileName,
		MimeType:   row.MimeType,
		SizeBytes:  row.SizeBytes,
		CreatedAt:  row.CreatedAt.Time,
	}
	if row.MessageID.Valid {
		messageID := uuid.UUID(row.MessageID.Bytes)
		attachment.MessageID = &messageID
	}
	if row.ChannelID.Valid {
		channelID := uuid.UUID(row.ChannelID.Bytes)
		attachment.ChannelID = &channelID
	}
	if row.ExpiresAt.Valid {
		expiresAt := row.ExpiresAt.Time
		attachment.ExpiresAt = &expiresAt
	}
	if row.Width.Valid {
		width := int(row.Width.Int32)
		attachment.Width = &width
	}
	if row.Height.Valid {
		height := int(row.Height.Int32)
		attachment.Height = &height
	}
	return attachment
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func pgUUIDPtr(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgUUID(*id)
}

func pgTimestamptz(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *value, Valid: true}
}

func pgInt4(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true} //nolint:gosec // image sizes in pixels
}
