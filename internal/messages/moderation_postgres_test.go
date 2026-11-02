package messages

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/audit"
	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

// The moderation settings on a real PostgreSQL: the slowmode clock is a query
// over stored messages, the caps and the timeout are columns, and every
// moderator action lands in the journal.
func TestPostgresModeration(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := t.Context()
	queries := db.New(pool)

	user := func(nickname string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (nickname, is_guest) VALUES ($1, true) RETURNING id`, nickname).Scan(&id); err != nil {
			t.Fatalf("insert user %s: %v", nickname, err)
		}
		return id
	}
	owner, member, other, newcomer := user("owner"), user("member"), user("other"), user("newcomer")

	workspaces := servers.NewService(servers.NewPostgresStore(pool), nil, servers.Icons{})
	workspaces.SetAuditor(audit.NewService(audit.NewPostgresStore(queries), nil))
	channelService := channels.NewService(channels.NewPostgresStore(pool), workspaces)
	channelService.SetAuditor(audit.NewService(audit.NewPostgresStore(queries), nil))
	svc := NewService(NewPostgresStore(queries), channelService, workspaces, nil, nil)

	server, err := workspaces.Create(ctx, servers.NewWorkspace{Name: "Модерация"}, owner)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	workspaceID := server.Workspace.ID
	for _, id := range []uuid.UUID{member, other} {
		if _, err := workspaces.Join(ctx, workspaceID, id); err != nil {
			t.Fatalf("join %s: %v", id, err)
		}
	}
	channel, err := channelService.Create(ctx, workspaceID, owner, channels.NewChannel{
		WorkspaceID: workspaceID, Kind: channels.KindText, Name: "общий",
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	channelID := channel.Channel.ID

	send := func(author uuid.UUID, clientID, content string, mentions ...uuid.UUID) (Message, error) {
		t.Helper()
		message, _, err := svc.Send(ctx, author, NewMessage{
			ChannelID:       channelID,
			ClientMessageID: clientID,
			Content:         content,
			Mentions:        mentions,
		})
		return message, err
	}
	count := func() int {
		t.Helper()
		var total int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE channel_id = $1`, channelID).Scan(&total); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		return total
	}

	// Slowmode: the second message of one author waits, and nothing is stored.
	slowmode := 3600
	if _, err := channelService.Update(ctx, channelID, owner, channels.Update{SlowmodeSeconds: &slowmode}); err != nil {
		t.Fatalf("set slowmode: %v", err)
	}
	if _, err := send(member, "m-1", "первое"); err != nil {
		t.Fatalf("first message: %v", err)
	}
	if _, err := send(member, "m-2", "второе"); !errors.Is(err, ErrSlowmode) {
		t.Fatalf("second message = %v, want ErrSlowmode", err)
	}
	if got := count(); got != 1 {
		t.Errorf("stored %d messages, want only the first", got)
	}

	// The owner manages the channel and is not held back.
	if _, err := send(owner, "o-1", "первое"); err != nil {
		t.Fatalf("owner's first message: %v", err)
	}
	if _, err := send(owner, "o-2", "второе"); err != nil {
		t.Fatalf("owner's second message: %v", err)
	}

	// The caps: a message over one is refused and not stored.
	mentions := 2
	links := 1
	if _, err := workspaces.Update(ctx, workspaceID, owner, servers.WorkspaceUpdate{
		MaxMentionsPerMessage: &mentions,
		MaxLinksPerMessage:    &links,
	}); err != nil {
		t.Fatalf("set caps: %v", err)
	}
	slowmode = 0
	if _, err := channelService.Update(ctx, channelID, owner, channels.Update{SlowmodeSeconds: &slowmode}); err != nil {
		t.Fatalf("clear slowmode: %v", err)
	}
	before := count()
	if _, err := send(member, "m-3", "эй", owner, other, member); !errors.Is(err, ErrTooManyMentions) {
		t.Fatalf("three mentions = %v, want ErrTooManyMentions", err)
	}
	if _, err := send(member, "m-4", "https://a.example https://b.example"); !errors.Is(err, ErrTooManyLinks) {
		t.Fatalf("two links = %v, want ErrTooManyLinks", err)
	}
	if got := count(); got != before {
		t.Errorf("a refused message was stored: %d messages, want %d", got, before)
	}
	if _, err := send(member, "m-5", "эй", owner, owner); err != nil {
		t.Fatalf("two mentions: %v", err)
	}

	// Automute: a newcomer is silenced by joining.
	automute := 600
	if _, err := workspaces.Update(ctx, workspaceID, owner, servers.WorkspaceUpdate{
		AutomuteNewMembersSeconds: &automute,
	}); err != nil {
		t.Fatalf("set automute: %v", err)
	}
	joined, err := workspaces.Join(ctx, workspaceID, newcomer)
	if err != nil {
		t.Fatalf("join newcomer: %v", err)
	}
	if joined.MutedUntil == nil {
		t.Fatal("the newcomer has no automute")
	}
	if _, err := send(newcomer, "n-1", "привет"); !errors.Is(err, ErrMuted) {
		t.Fatalf("newcomer's message = %v, want ErrMuted", err)
	}

	// A moderator mutes a member with a reason; the reason is stored with the
	// mute and written to the journal, and the message is refused until it ends.
	muted, err := workspaces.Mute(ctx, workspaceID, owner, member, time.Hour, "спам")
	if err != nil {
		t.Fatalf("mute: %v", err)
	}
	if muted.MutedUntil == nil {
		t.Fatal("the muted member carries no mutedUntil")
	}
	var storedUntil time.Time
	var reason string
	if err := pool.QueryRow(ctx,
		`SELECT muted_until, mute_reason FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, member).Scan(&storedUntil, &reason); err != nil {
		t.Fatalf("read the mute: %v", err)
	}
	if reason != "спам" {
		t.Errorf("stored reason = %q, want the moderator's reason", reason)
	}
	if _, err := send(member, "m-6", "ещё"); !errors.Is(err, ErrMuted) {
		t.Fatalf("muted member's message = %v, want ErrMuted", err)
	}

	var action string
	var metadata []byte
	if err := pool.QueryRow(ctx,
		`SELECT action, metadata FROM audit_log WHERE workspace_id = $1 AND action = $2 ORDER BY created_at DESC LIMIT 1`,
		workspaceID, audit.ActionMemberMute).Scan(&action, &metadata); err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	if !strings.Contains(string(metadata), "спам") {
		t.Errorf("journal metadata = %s, want the reason", metadata)
	}

	// The owner is out of reach, and so is a moderator themselves.
	if _, err := workspaces.Mute(ctx, workspaceID, member, owner, time.Hour, ""); !errors.Is(err, servers.ErrForbidden) {
		t.Errorf("muting the owner = %v, want ErrForbidden", err)
	}
	if _, err := workspaces.Mute(ctx, workspaceID, owner, owner, time.Hour, ""); !errors.Is(err, servers.ErrForbidden) {
		t.Errorf("muting themselves = %v, want ErrForbidden", err)
	}

	// Duration zero lifts it, the columns are cleared and the journal says so;
	// the member writes again.
	if _, err := workspaces.Mute(ctx, workspaceID, owner, member, 0, ""); err != nil {
		t.Fatalf("unmute: %v", err)
	}
	var cleared *time.Time
	var clearedReason *string
	if err := pool.QueryRow(ctx,
		`SELECT muted_until, mute_reason FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, member).Scan(&cleared, &clearedReason); err != nil {
		t.Fatalf("read the lifted mute: %v", err)
	}
	if cleared != nil || clearedReason != nil {
		t.Errorf("mute = %v/%v after unmute, want both NULL", cleared, clearedReason)
	}
	var unmutes int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE workspace_id = $1 AND action = $2`,
		workspaceID, audit.ActionMemberUnmute).Scan(&unmutes); err != nil {
		t.Fatalf("count unmutes: %v", err)
	}
	if unmutes != 1 {
		t.Errorf("journal has %d unmutes, want 1", unmutes)
	}
	if _, err := send(member, "m-7", "снова можно"); err != nil {
		t.Fatalf("message after the mute: %v", err)
	}
}
