package channels

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/perms"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
)

// The old «only admins write» flag on a real PostgreSQL: written as an
// @everyone override, read back as IsReadOnly, and taken away cleanly.
func TestPostgresReadOnlyIsAnEveryoneOverride(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := t.Context()
	store := NewPostgresStore(pool)

	var owner, workspace, everyone uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (nickname, is_guest) VALUES ('owner', true) RETURNING id`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, owner_id) VALUES ('Сервер', $1) RETURNING id`, owner).Scan(&workspace); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO server_roles (workspace_id, name, position, permissions, is_default)
		VALUES ($1, '@everyone', 0, $2, true) RETURNING id`, workspace, int64(perms.ViewChannel|perms.SendMessages)).Scan(&everyone); err != nil {
		t.Fatal(err)
	}

	channel, err := store.Create(ctx, NewChannel{WorkspaceID: workspace, Kind: KindText, Name: "новости"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetReadOnly(ctx, channel, true); err != nil {
		t.Fatalf("SetReadOnly: %v", err)
	}
	read, err := store.Channel(ctx, channel.ID)
	if err != nil {
		t.Fatalf("Channel: %v", err)
	}
	if !read.IsReadOnly || len(read.Overrides) != 1 || read.Overrides[0].RoleID != everyone ||
		!read.Overrides[0].Deny.Has(perms.SendMessages|perms.Speak) {
		t.Errorf("read-only channel = %+v", read)
	}
	list, err := store.List(ctx, workspace)
	if err != nil || len(list) != 1 || !list[0].IsReadOnly {
		t.Errorf("List = %+v, %v; want the one read-only channel", list, err)
	}

	if err := store.SetReadOnly(ctx, read, false); err != nil {
		t.Fatalf("SetReadOnly off: %v", err)
	}
	read, err = store.Channel(ctx, channel.ID)
	if err != nil || read.IsReadOnly || len(read.Overrides) != 0 {
		t.Errorf("after turning it off = %+v, %v; want no override left", read, err)
	}
}
