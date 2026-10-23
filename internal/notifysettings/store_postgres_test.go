package notifysettings

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
)

// Rows on a real PostgreSQL: one per account and target, the level or the
// deadline, the whole audience in one query, and the rows go with the account.
func TestPostgresNotificationSettings(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := t.Context()
	store := NewPostgresStore(db.New(pool))

	user := func(nickname string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO users (nickname, is_guest) VALUES ($1, true) RETURNING id`, nickname).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	anna, boris := user("anna"), user("boris")
	workspaceID, channelID, otherChannelID := uuid.New(), uuid.New(), uuid.New()
	deadline := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	all := LevelAll

	stored, err := store.Upsert(ctx, Setting{
		UserID: anna, Scope: ScopeChannel, TargetID: channelID, Level: &all,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if stored.Level == nil || *stored.Level != LevelAll || stored.UpdatedAt.IsZero() {
		t.Fatalf("stored = %+v, want the level and a timestamp", stored)
	}

	// The same row again replaces the level and adds the deadline; it does not grow.
	muted := LevelNone
	updated, err := store.Upsert(ctx, Setting{
		UserID: anna, Scope: ScopeChannel, TargetID: channelID, Level: &muted, MutedUntil: &deadline,
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if updated.Level == nil || *updated.Level != LevelNone || updated.MutedUntil == nil || !updated.MutedUntil.Equal(deadline) {
		t.Fatalf("updated = %+v, want none until the deadline", updated)
	}
	if n, err := store.Count(ctx, anna); err != nil || n != 1 {
		t.Fatalf("Count = %d (err %v), want 1", n, err)
	}

	// A row with no level inherits; nil is not the same as "none".
	if _, err := store.Upsert(ctx, Setting{UserID: anna, Scope: ScopeWorkspace, TargetID: workspaceID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(ctx, Setting{UserID: boris, Scope: ScopeWorkspace, TargetID: workspaceID, Level: &all}); err != nil {
		t.Fatal(err)
	}

	rows, err := store.ForTargets(ctx, []uuid.UUID{channelID, workspaceID})
	if err != nil || len(rows) != 3 {
		t.Fatalf("ForTargets = %v (err %v), want three rows", rows, err)
	}
	own, err := store.ForUserTargets(ctx, anna, []uuid.UUID{channelID, workspaceID})
	if err != nil || len(own) != 2 {
		t.Fatalf("ForUserTargets = %v (err %v), want Anna's two rows", own, err)
	}
	list, err := store.List(ctx, anna)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %v (err %v), want two rows", list, err)
	}
	if _, ok, err := store.Get(ctx, anna, ScopeChannel, otherChannelID); err != nil || ok {
		t.Fatalf("Get of a missing row = ok %v (err %v), want false", ok, err)
	}

	if deleted, err := store.Delete(ctx, boris, ScopeWorkspace, workspaceID); err != nil || !deleted {
		t.Fatalf("Boris could not delete his row: %v (err %v)", deleted, err)
	}
	if deleted, err := store.Delete(ctx, anna, ScopeWorkspace, workspaceID); err != nil || !deleted {
		t.Fatalf("Anna could not delete her row: %v (err %v)", deleted, err)
	}
	if deleted, _ := store.Delete(ctx, anna, ScopeWorkspace, workspaceID); deleted {
		t.Fatal("deleting a missing row reported a delete")
	}

	// Deleting the account takes its rows with it.
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, anna); err != nil {
		t.Fatal(err)
	}
	if n, err := store.Count(ctx, anna); err != nil || n != 0 {
		t.Fatalf("after the account is gone Count = %d (err %v), want 0", n, err)
	}
}
