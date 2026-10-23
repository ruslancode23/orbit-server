package push

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
)

// Subscriptions on a real PostgreSQL: a browser has one row whoever it belongs
// to, an account only removes its own, and the rows go with the account.
func TestPostgresPushSubscriptions(t *testing.T) {
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

	first, err := store.Upsert(ctx, Subscription{ID: uuid.New(), UserID: anna, Endpoint: fcm + "laptop", P256dh: "k1", Auth: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	// The same browser signs in as Boris: the row moves, keeps its id and takes the new keys.
	moved, err := store.Upsert(ctx, Subscription{ID: uuid.New(), UserID: boris, Endpoint: fcm + "laptop", P256dh: "k2", Auth: "a2"})
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID != first.ID || moved.UserID != boris || moved.P256dh != "k2" || moved.Auth != "a2" {
		t.Fatalf("moved = %+v, want the first row (%s) under Boris with new keys", moved, first.ID)
	}
	if subs, err := store.ForUsers(ctx, []uuid.UUID{anna}); err != nil || len(subs) != 0 {
		t.Fatalf("Anna still has %v (err %v)", subs, err)
	}

	phone, err := store.Upsert(ctx, Subscription{ID: uuid.New(), UserID: anna, Endpoint: fcm + "phone", P256dh: "k3", Auth: "a3"})
	if err != nil {
		t.Fatal(err)
	}
	subs, err := store.ForUsers(ctx, []uuid.UUID{anna, boris})
	if err != nil || len(subs) != 2 {
		t.Fatalf("ForUsers = %v (err %v), want both browsers", subs, err)
	}

	if err := store.Touch(ctx, fcm+"phone"); err != nil {
		t.Fatal(err)
	}
	var touched bool
	if err := pool.QueryRow(ctx, `SELECT last_success_at IS NOT NULL FROM push_subscriptions WHERE id = $1`, phone.ID).Scan(&touched); err != nil || !touched {
		t.Fatalf("Touch did not record the delivery (err %v)", err)
	}

	if deleted, err := store.Delete(ctx, phone.ID, boris); err != nil || deleted {
		t.Fatalf("Boris deleted Anna's phone: deleted=%v err=%v", deleted, err)
	}
	if deleted, err := store.Delete(ctx, phone.ID, anna); err != nil || !deleted {
		t.Fatalf("Anna could not delete her phone: deleted=%v err=%v", deleted, err)
	}

	if err := store.DeleteEndpoint(ctx, fcm+"laptop"); err != nil {
		t.Fatal(err)
	}
	if subs, err := store.ForUsers(ctx, []uuid.UUID{boris}); err != nil || len(subs) != 0 {
		t.Fatalf("the laptop is still there: %v (err %v)", subs, err)
	}

	// Deleting an account takes its browsers with it.
	if _, err := store.Upsert(ctx, Subscription{ID: uuid.New(), UserID: anna, Endpoint: fcm + "tablet", P256dh: "k", Auth: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, anna); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM push_subscriptions`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d subscriptions outlived their account (err %v)", left, err)
	}
}
