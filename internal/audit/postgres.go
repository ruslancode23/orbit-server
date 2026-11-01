package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
)

// PostgresStore keeps the rows in audit_log.
type PostgresStore struct {
	q db.Querier
}

// NewPostgresStore returns a store over the generated queries.
func NewPostgresStore(q db.Querier) *PostgresStore { return &PostgresStore{q: q} }

var _ Store = (*PostgresStore)(nil)

// Insert writes one entry. The row's id and created_at come from the database.
func (s *PostgresStore) Insert(ctx context.Context, e Entry) error {
	metadata, err := marshalMetadata(e.Metadata)
	if err != nil {
		return err
	}
	if _, err := s.q.RecordAuditEntry(ctx, db.RecordAuditEntryParams{
		WorkspaceID: pgUUID(e.WorkspaceID),
		ActorID:     optionalUUID(e.ActorID),
		Action:      string(e.Action),
		TargetType:  string(e.TargetType),
		TargetID:    optionalUUID(e.TargetID),
		Metadata:    metadata,
	}); err != nil {
		return fmt.Errorf("insert audit entry: %w", err)
	}
	return nil
}

// List reads one page of a server's journal, newest first.
func (s *PostgresStore) List(ctx context.Context, workspaceID uuid.UUID, f Filter, limit int) ([]Entry, error) {
	params := db.ListAuditEntriesParams{
		WorkspaceID: pgUUID(workspaceID),
		PageLimit:   int32(limit), // #nosec G115 -- the service clamps it to 101.
	}
	if f.Action != nil {
		params.Action = pgtype.Text{String: string(*f.Action), Valid: true}
	}
	if f.ActorID != nil {
		params.ActorID = pgUUID(*f.ActorID)
	}
	if f.Before != nil {
		params.BeforeCreatedAt = pgtype.Timestamptz{Time: f.Before.CreatedAt, Valid: true}
		params.BeforeID = pgUUID(f.Before.ID)
	}
	rows, err := s.q.ListAuditEntries(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}
	entries := make([]Entry, 0, len(rows))
	for _, row := range rows {
		entry, err := toEntry(row)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func toEntry(row db.AuditLog) (Entry, error) {
	entry := Entry{
		ID:          uuid.UUID(row.ID.Bytes),
		WorkspaceID: uuid.UUID(row.WorkspaceID.Bytes),
		Action:      Action(row.Action),
		TargetType:  TargetType(row.TargetType),
		CreatedAt:   row.CreatedAt.Time,
	}
	if row.ActorID.Valid {
		id := uuid.UUID(row.ActorID.Bytes)
		entry.ActorID = &id
	}
	if row.TargetID.Valid {
		id := uuid.UUID(row.TargetID.Bytes)
		entry.TargetID = &id
	}
	if len(row.Metadata) > 0 {
		metadata := map[string]any{}
		if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
			return Entry{}, fmt.Errorf("decode audit metadata %s: %w", entry.ID, err)
		}
		entry.Metadata = metadata
	}
	return entry, nil
}

// marshalMetadata turns the entry's map back into jsonb bytes. An empty map is
// the same as no metadata: the column is nullable and the API shows `{}`.
func marshalMetadata(metadata map[string]any) ([]byte, error) {
	if len(metadata) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("encode audit metadata: %w", err)
	}
	return raw, nil
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func optionalUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgUUID(*id)
}
