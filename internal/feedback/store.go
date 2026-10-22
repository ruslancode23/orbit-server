package feedback

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

func (s *PostgresStore) Create(ctx context.Context, r Report) (Report, error) {
	params := db.CreateBugReportParams{
		ID:          pgtype.UUID{Bytes: r.ID, Valid: true},
		UserID:      pgtype.UUID{Bytes: r.UserID, Valid: r.UserID != uuid.Nil},
		Description: r.Description,
		Steps:       r.Steps,
		AppVersion:  r.AppVersion,
		UserAgent:   r.UserAgent,
		Route:       r.Route,
	}
	if r.Screenshot != nil {
		params.ScreenshotKey = pgtype.Text{String: r.Screenshot.Key, Valid: true}
		params.ScreenshotType = pgtype.Text{String: r.Screenshot.MimeType, Valid: true}
		params.ScreenshotBytes = pgtype.Int8{Int64: r.Screenshot.SizeBytes, Valid: true}
	}
	row, err := s.q.CreateBugReport(ctx, params)
	if err != nil {
		return Report{}, fmt.Errorf("create bug report: %w", err)
	}
	return toReport(row), nil
}

func toReport(row db.BugReport) Report {
	report := Report{
		ID:          row.ID.Bytes,
		Description: row.Description,
		Steps:       row.Steps,
		AppVersion:  row.AppVersion,
		UserAgent:   row.UserAgent,
		Route:       row.Route,
		CreatedAt:   row.CreatedAt.Time,
	}
	if row.UserID.Valid {
		report.UserID = row.UserID.Bytes
	}
	if row.ScreenshotKey.Valid {
		report.Screenshot = &Screenshot{
			Key:       row.ScreenshotKey.String,
			MimeType:  row.ScreenshotType.String,
			SizeBytes: row.ScreenshotBytes.Int64,
		}
	}
	return report
}
