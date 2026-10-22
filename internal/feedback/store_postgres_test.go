package feedback

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
)

// A report on a real PostgreSQL: the row comes back as it was written, the
// screenshot columns go together, the table refuses what the service refuses,
// and the report outlives its author.
func TestPostgresBugReports(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := t.Context()
	store := NewPostgresStore(db.New(pool))

	var anna uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (nickname, is_guest) VALUES ('anna', true) RETURNING id`).Scan(&anna); err != nil {
		t.Fatal(err)
	}

	svc := NewService(store, newFakeStorage(), 1<<20)
	plain, err := svc.Submit(ctx, anna, validDraft(), nil)
	if err != nil {
		t.Fatalf("Submit without a screenshot: %v", err)
	}
	if plain.UserID != anna || plain.CreatedAt.IsZero() || plain.Screenshot != nil ||
		plain.Description != "Не отправляется сообщение" || plain.Route != "/s/0d3c/7a1b" {
		t.Errorf("stored report = %+v", plain)
	}

	shot, err := svc.Submit(ctx, anna, validDraft(), encodePNG(t))
	if err != nil {
		t.Fatalf("Submit with a screenshot: %v", err)
	}
	var key, mimeType string
	var size int64
	if err := pool.QueryRow(ctx, `SELECT screenshot_key, screenshot_type, screenshot_bytes FROM bug_reports WHERE id = $1`,
		shot.ID).Scan(&key, &mimeType, &size); err != nil {
		t.Fatal(err)
	}
	if key != shot.Screenshot.Key || mimeType != "image/png" || size != shot.Screenshot.SizeBytes {
		t.Errorf("screenshot row = %q %q %d, want %+v", key, mimeType, size, shot.Screenshot)
	}

	// The table holds the line even for a caller that skips the service.
	refused := []struct {
		name   string
		report Report
	}{
		{"blank description", Report{ID: uuid.New(), UserID: anna, Description: "   ", AppVersion: "1", Route: "/"}},
		{"long steps", Report{ID: uuid.New(), UserID: anna, Description: "x", Steps: strings.Repeat("ш", 4001), AppVersion: "1", Route: "/"}},
		{"no app version", Report{ID: uuid.New(), UserID: anna, Description: "x", Route: "/"}},
		{"half a screenshot", Report{
			ID: uuid.New(), UserID: anna, Description: "x", AppVersion: "1", Route: "/",
			Screenshot: &Screenshot{Key: "bug-reports/x.png"},
		}},
	}
	for _, tt := range refused {
		if _, err := store.Create(ctx, tt.report); err == nil {
			t.Errorf("%s: stored, want a check violation", tt.name)
		}
	}

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, anna); err != nil {
		t.Fatalf("delete the author: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM bug_reports WHERE user_id IS NULL`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 2 {
		t.Errorf("reports without an author = %d, want both kept", left)
	}
}
