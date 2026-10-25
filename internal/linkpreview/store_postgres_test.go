package linkpreview

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
)

// The cache and the message links on a real PostgreSQL: one row per normalized
// URL, the card attached to a message and dropped with it.
func TestPostgresLinkPreviews(t *testing.T) {
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
	anna := user("anna")
	var workspaceID, channelID, messageID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, owner_id) VALUES ('w', $1) RETURNING id`, anna).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO channels (workspace_id, kind, name) VALUES ($1, 'text', 'общий') RETURNING id`, workspaceID).Scan(&channelID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO messages (channel_id, author_id, client_message_id, content) VALUES ($1, $2, 'm-1', 'ссылка') RETURNING id`, channelID, anna).Scan(&messageID); err != nil {
		t.Fatal(err)
	}

	preview, err := store.Save(ctx, cachedPreview{
		Preview: Preview{
			ID: uuid.New(), URL: "https://example.com/a", Title: "Заголовок",
			Description: "Описание", SiteName: "Example", ImageURL: "https://example.com/i.png",
		},
		Status: "ok", FetchedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if preview.ID == uuid.Nil || preview.Title != "Заголовок" {
		t.Fatalf("saved = %+v", preview)
	}
	if got, err := store.Preview(ctx, "https://example.com/a"); err != nil || got.Title != "Заголовок" {
		t.Fatalf("Preview = %+v (err %v)", got, err)
	}
	// The same URL updates instead of duplicating.
	updated, err := store.Save(ctx, cachedPreview{
		Preview: Preview{ID: uuid.New(), URL: "https://example.com/a", Title: "Новый"},
		Status:  "ok", FetchedAt: time.Now(),
	})
	if err != nil || updated.ID != preview.ID || updated.Title != "Новый" {
		t.Fatalf("second Save = %+v (err %v), want the same row updated", updated, err)
	}

	if err := store.Attach(ctx, messageID, preview.ID); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	previews, err := store.ForMessages(ctx, []uuid.UUID{messageID})
	if err != nil || previews[messageID].Title != "Новый" {
		t.Fatalf("ForMessages = %v (err %v)", previews, err)
	}
	if err := store.Detach(ctx, messageID); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if previews, _ := store.ForMessages(ctx, []uuid.UUID{messageID}); len(previews) != 0 {
		t.Fatalf("previews after Detach = %v", previews)
	}

	// Deleting the message takes the link with it, not the cached card.
	if err := store.Attach(ctx, messageID, preview.ID); err != nil {
		t.Fatalf("re-Attach: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM messages WHERE id = $1`, messageID); err != nil {
		t.Fatal(err)
	}
	var links int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_previews WHERE message_id = $1`, messageID).Scan(&links); err != nil || links != 0 {
		t.Fatalf("links after deleting the message = %d (err %v)", links, err)
	}
	if _, err := store.ByID(ctx, preview.ID); err != nil {
		t.Errorf("the cached card went away with the message: %v", err)
	}
}
