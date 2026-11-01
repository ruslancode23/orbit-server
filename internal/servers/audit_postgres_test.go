package servers

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/audit"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
	"github.com/ruslancode23/orbit-server/internal/profile"
)

// staticProfiles answers the actors the journal asks about, so the endpoint's
// profile enrichment is tested without the whole profile module.
type staticProfiles map[uuid.UUID]profile.Public

func (p staticProfiles) Profiles(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]profile.Public, error) {
	out := make(map[uuid.UUID]profile.Public, len(ids))
	for _, id := range ids {
		if person, ok := p[id]; ok {
			out[id] = person
		}
	}
	return out, nil
}

// The audit endpoint on a real PostgreSQL: the journal rows the module writes,
// the 404 for an outsider and the 403 for a plain member, the cursor pages,
// the filters, and the profile of a moderator who no longer exists.
func TestPostgresAuditEndpoint(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := t.Context()
	store := NewPostgresStore(pool)
	svc := NewService(store, nil, Icons{})
	journal := audit.NewService(audit.NewPostgresStore(db.New(pool)), slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.SetAuditor(journal)
	svc.SetAuditLog(journal)

	user := func(nickname string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO users (nickname, is_guest) VALUES ($1, true) RETURNING id`, nickname).Scan(&id); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		return id
	}
	owner, admin, member, banned, outsider := user("owner"), user("admin"), user("member"), user("banned"), user("outsider")

	workspace, err := svc.Create(ctx, NewWorkspace{Name: "Аудит"}, owner)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	for _, id := range []uuid.UUID{admin, member, banned} {
		if _, err := svc.Join(ctx, workspace.Workspace.ID, id); err != nil {
			t.Fatalf("join %s: %v", id, err)
		}
	}
	if err := store.GrantAdmin(ctx, workspace.Workspace.ID, admin); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	svc.SetProfiles(staticProfiles{
		owner: {ID: owner, Nickname: "owner", DisplayName: "Owner", CreatedAt: time.Now()},
		admin: {ID: admin, Nickname: "admin", DisplayName: "Admin", CreatedAt: time.Now()},
	})
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := api.ListWorkspaceAuditRequestObject{WorkspaceId: workspace.Workspace.ID}

	// An outsider must not be able to tell that the server exists.
	resp, err := h.ListWorkspaceAudit(sessionFor(ctx, outsider), request)
	if err != nil {
		t.Fatalf("outsider: %v", err)
	}
	if _, ok := resp.(api.ListWorkspaceAudit404JSONResponse); !ok {
		t.Fatalf("outsider response = %T, want 404", resp)
	}
	// A plain member knows the server but may not read the journal.
	resp, err = h.ListWorkspaceAudit(sessionFor(ctx, member), request)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	if _, ok := resp.(api.ListWorkspaceAudit403JSONResponse); !ok {
		t.Fatalf("member response = %T, want 403", resp)
	}

	// The owner bans one member, the admin bans another: both actions must be
	// in the journal, and only through the modules' own paths.
	if _, err := svc.Ban(ctx, workspace.Workspace.ID, owner, banned, "spam"); err != nil {
		t.Fatalf("owner ban: %v", err)
	}
	if _, err := svc.Ban(ctx, workspace.Workspace.ID, admin, member, "flood"); err != nil {
		t.Fatalf("admin ban: %v", err)
	}

	// A few more rows exercise the cursor; they are written through the journal
	// itself because the endpoint only reads what other modules wrote.
	for i := range 3 {
		if err := journal.Record(ctx, audit.Entry{
			WorkspaceID: workspace.Workspace.ID,
			ActorID:     &owner,
			Action:      audit.ActionChannelDelete,
			TargetType:  audit.TargetChannel,
			TargetID:    audit.Target(uuid.New()),
			Metadata:    map[string]any{"name": "канал"},
		}); err != nil {
			t.Fatalf("record channel delete %d: %v", i, err)
		}
	}

	// The admin account is deleted: its rows stay, actor_id becomes NULL, and
	// the endpoint says «deleted» instead of inventing a profile.
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, admin); err != nil {
		t.Fatalf("delete admin: %v", err)
	}

	// The owner pages through the whole journal; five rows, two per page.
	var all []api.AuditEntry
	cursor := (*string)(nil)
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination does not terminate")
		}
		req := request
		limit := 2
		req.Params = api.ListWorkspaceAuditParams{Limit: &limit, Before: cursor}
		out, err := h.ListWorkspaceAudit(sessionFor(ctx, owner), req)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		page, ok := out.(api.ListWorkspaceAudit200JSONResponse)
		if !ok {
			t.Fatalf("response = %T, want 200", out)
		}
		all = append(all, page.Items...)
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	if len(all) != 5 {
		t.Fatalf("paged journal = %d entries, want 5", len(all))
	}
	seen := map[uuid.UUID]bool{}
	for _, entry := range all {
		if seen[entry.Id] {
			t.Fatalf("entry %s repeated across pages", entry.Id)
		}
		seen[entry.Id] = true
		if entry.Metadata == nil {
			t.Errorf("entry %s has a null metadata, want an object", entry.Id)
		}
	}

	deleted, live := (*api.AuditEntry)(nil), (*api.AuditEntry)(nil)
	for i := range all {
		entry := &all[i]
		if entry.Action != string(audit.ActionMemberBan) {
			continue
		}
		if entry.ActorDeleted {
			deleted = entry
		} else {
			live = entry
		}
	}
	if live == nil || live.Actor == nil || live.Actor.Nickname != "owner" {
		t.Fatalf("live ban = %+v, want the owner's profile", live)
	}
	if deleted == nil || deleted.Actor != nil {
		t.Fatalf("deleted-actor ban = %+v, want no profile and actorDeleted", deleted)
	}

	// The action filter and the actor filter work through the endpoint.
	action := string(audit.ActionMemberBan)
	actor := owner
	filtered, err := h.ListWorkspaceAudit(sessionFor(ctx, owner), api.ListWorkspaceAuditRequestObject{
		WorkspaceId: workspace.Workspace.ID,
		Params:      api.ListWorkspaceAuditParams{Action: &action, ActorId: &actor},
	})
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	filteredPage, ok := filtered.(api.ListWorkspaceAudit200JSONResponse)
	if !ok {
		t.Fatalf("filtered response = %T, want 200", filtered)
	}
	if len(filteredPage.Items) != 1 || filteredPage.Items[0].Actor == nil || filteredPage.Items[0].Actor.Nickname != "owner" {
		t.Fatalf("filtered items = %+v, want the owner's one ban", filteredPage.Items)
	}

	// A cursor this server did not issue is a client error, not an empty page.
	bad := "not-a-cursor"
	badResp, err := h.ListWorkspaceAudit(sessionFor(ctx, owner), api.ListWorkspaceAuditRequestObject{
		WorkspaceId: workspace.Workspace.ID,
		Params:      api.ListWorkspaceAuditParams{Before: &bad},
	})
	if err != nil {
		t.Fatalf("bad cursor: %v", err)
	}
	if _, ok := badResp.(api.ListWorkspaceAudit400JSONResponse); !ok {
		t.Fatalf("bad cursor response = %T, want 400", badResp)
	}
}
