package audit

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
)

// The journal on a real PostgreSQL: servers are isolated, the keyset cursor
// walks the whole page set without repeats, the filters reach SQL, metadata
// round-trips, and deleting an account keeps its rows with actor_id NULL.
func TestPostgresAuditLog(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := t.Context()
	store := NewPostgresStore(db.New(pool))
	svc := NewService(store, nil)

	user := func(nickname string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO users (nickname, is_guest) VALUES ($1, true) RETURNING id`, nickname).Scan(&id); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		return id
	}
	workspace := func(owner uuid.UUID, name string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, owner_id) VALUES ($1, $2) RETURNING id`, name, owner).Scan(&id); err != nil {
			t.Fatalf("insert workspace: %v", err)
		}
		return id
	}

	anna, boris, carol := user("anna"), user("boris"), user("carol")
	first := workspace(anna, "Первый")
	second := workspace(carol, "Второй")

	record := func(workspaceID, actorID uuid.UUID, action Action, targetType TargetType, targetID *uuid.UUID, metadata map[string]any) {
		t.Helper()
		entry := Entry{
			WorkspaceID: workspaceID,
			ActorID:     &actorID,
			Action:      action,
			TargetType:  targetType,
			TargetID:    targetID,
			Metadata:    metadata,
		}
		if err := svc.Record(ctx, entry); err != nil {
			t.Fatalf("Record %s: %v", action, err)
		}
	}

	banTarget := uuid.New()
	record(first, anna, ActionMemberBan, TargetUser, Target(banTarget), map[string]any{"note": "keep"})
	record(first, anna, ActionMemberKick, TargetUser, Target(uuid.New()), nil)
	record(first, boris, ActionMemberRoleGrant, TargetUser, Target(uuid.New()), map[string]any{"roleName": "Модератор"})
	record(first, boris, ActionChannelDelete, TargetChannel, Target(uuid.New()), nil)
	record(first, anna, ActionInviteCreate, TargetInvite, Target(uuid.New()), nil)
	// Another server: the same table, never the same page.
	record(second, carol, ActionMemberBan, TargetUser, Target(uuid.New()), nil)

	page, err := svc.List(ctx, first, ListParams{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Entries) != 5 {
		t.Fatalf("first server page = %d entries, want 5", len(page.Entries))
	}
	for _, entry := range page.Entries {
		if entry.WorkspaceID != first {
			t.Errorf("entry %s belongs to another server", entry.ID)
		}
	}
	if page.Next != nil {
		t.Errorf("five entries fit one page, next = %+v", page.Next)
	}

	// Newest first, and every action is present exactly once.
	wanted := map[Action]bool{
		ActionMemberBan: true, ActionMemberKick: true, ActionMemberRoleGrant: true,
		ActionChannelDelete: true, ActionInviteCreate: true,
	}
	for i, entry := range page.Entries {
		delete(wanted, entry.Action)
		if i > 0 && entry.CreatedAt.After(page.Entries[i-1].CreatedAt) {
			t.Errorf("entry %d is newer than the one before it", i)
		}
		if entry.Action == ActionMemberBan && (entry.TargetID == nil || *entry.TargetID != banTarget) {
			t.Errorf("ban target = %v, want %s", entry.TargetID, banTarget)
		}
	}
	if len(wanted) != 0 {
		t.Errorf("actions missing from the page: %v", wanted)
	}

	// Keyset pagination: three pages of two, then one, no repeats.
	var seen []uuid.UUID
	cursor := (*Cursor)(nil)
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination does not terminate")
		}
		got, err := svc.List(ctx, first, ListParams{Filter: Filter{Before: cursor}, Limit: 2})
		if err != nil {
			t.Fatalf("List page: %v", err)
		}
		for _, entry := range got.Entries {
			seen = append(seen, entry.ID)
		}
		if got.Next == nil {
			break
		}
		cursor = got.Next
	}
	if len(seen) != 5 {
		t.Fatalf("pages covered %d entries, want 5", len(seen))
	}
	unique := map[uuid.UUID]bool{}
	for _, id := range seen {
		if unique[id] {
			t.Errorf("entry %s repeated across pages", id)
		}
		unique[id] = true
	}

	// The filters reach SQL: one action, one moderator.
	action := ActionMemberBan
	bans, err := svc.List(ctx, first, ListParams{Filter: Filter{Action: &action}})
	if err != nil || len(bans.Entries) != 1 || bans.Entries[0].Action != ActionMemberBan {
		t.Fatalf("action filter = %+v (err %v), want the one ban", bans.Entries, err)
	}
	byAnna, err := svc.List(ctx, first, ListParams{Filter: Filter{ActorID: &anna}})
	if err != nil || len(byAnna.Entries) != 3 {
		t.Fatalf("actor filter = %d entries (err %v), want Anna's three", len(byAnna.Entries), err)
	}
	for _, entry := range byAnna.Entries {
		if entry.ActorID == nil || *entry.ActorID != anna {
			t.Errorf("actor filter returned an entry of %v", entry.ActorID)
		}
	}

	// Metadata survives jsonb.
	grants, err := svc.List(ctx, first, ListParams{Filter: Filter{ActorID: &boris}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, entry := range grants.Entries {
		if entry.Action != ActionMemberRoleGrant {
			continue
		}
		found = true
		if entry.Metadata["roleName"] != "Модератор" {
			t.Errorf("metadata = %v, want the role name", entry.Metadata)
		}
	}
	if !found {
		t.Fatal("the role grant is missing")
	}

	// Deleting the moderator keeps the row and nulls actor_id (ON DELETE SET NULL).
	// Boris owns nothing, so the account can be deleted without the workspace
	// owner foreign key standing in the way.
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, boris); err != nil {
		t.Fatalf("delete boris: %v", err)
	}
	after, err := svc.List(ctx, first, ListParams{})
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	nulled := 0
	for _, entry := range after.Entries {
		if entry.ActorID == nil {
			nulled++
		}
	}
	if nulled != 2 {
		t.Fatalf("entries with a deleted actor = %d, want 2 (Boris's grant and channel delete)", nulled)
	}

	// Deleting the server takes its journal with it (ON DELETE CASCADE).
	if _, err := pool.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, first); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	if got, err := svc.List(ctx, first, ListParams{}); err != nil || len(got.Entries) != 0 {
		t.Fatalf("after workspace delete = %d entries (err %v), want none", len(got.Entries), err)
	}
}

// The cursor is stable when two rows share a timestamp: the id breaks the tie,
// so a page boundary inside one instant cannot drop or repeat a row.
func TestPostgresAuditCursorTies(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := t.Context()
	svc := NewService(NewPostgresStore(db.New(pool)), nil)

	var owner uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (nickname, is_guest) VALUES ('owner', true) RETURNING id`).Scan(&owner); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	var workspaceID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, owner_id) VALUES ('Сервер', $1) RETURNING id`, owner).Scan(&workspaceID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	// All rows get one created_at, so only the id can order them.
	at := time.Now().UTC().Truncate(time.Microsecond)
	var ids []uuid.UUID
	for range 4 {
		id := uuid.New()
		ids = append(ids, id)
		if _, err := pool.Exec(ctx,
			`INSERT INTO audit_log (id, workspace_id, actor_id, action, target_type, created_at)
			 VALUES ($1, $2, $3, 'member.ban', 'user', $4)`,
			id, workspaceID, owner, at); err != nil {
			t.Fatalf("insert entry: %v", err)
		}
	}

	first, err := svc.List(ctx, workspaceID, ListParams{Limit: 2})
	if err != nil || len(first.Entries) != 2 || first.Next == nil {
		t.Fatalf("first page = %d entries, next %v, err %v", len(first.Entries), first.Next, err)
	}
	second, err := svc.List(ctx, workspaceID, ListParams{Filter: Filter{Before: first.Next}, Limit: 2})
	if err != nil || len(second.Entries) != 2 || second.Next != nil {
		t.Fatalf("second page = %d entries, next %v, err %v", len(second.Entries), second.Next, err)
	}
	seen := map[uuid.UUID]bool{}
	for _, entry := range append(first.Entries, second.Entries...) {
		if seen[entry.ID] {
			t.Fatalf("entry %s repeated across the tie", entry.ID)
		}
		seen[entry.ID] = true
	}
	if len(seen) != len(ids) {
		t.Fatalf("pages covered %d rows, want %d", len(seen), len(ids))
	}
}
