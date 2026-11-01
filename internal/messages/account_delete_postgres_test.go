package messages

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

const deleteAccountPassword = "correct horse battery"

// Deleting an account on real PostgreSQL: the personal rows cascade away, while
// the message in a shared channel and the dialogue the other person keeps stay
// in place with a NULL author. History and search must survive that NULL, and a
// server owner is refused rather than losing the server silently.
func TestPostgresAccountDeletionKeepsHistory(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := t.Context()
	queries := db.New(pool)

	accounts := auth.NewService(auth.NewPostgresStore(queries), time.Hour)
	anna, annaCreds, err := accounts.Register(ctx, "Anna", "anna@example.com", deleteAccountPassword)
	if err != nil {
		t.Fatalf("Register Anna: %v", err)
	}
	carol, carolCreds, err := accounts.Register(ctx, "Carol", "carol@example.com", deleteAccountPassword)
	if err != nil {
		t.Fatalf("Register Carol: %v", err)
	}

	workspaces := servers.NewService(servers.NewPostgresStore(pool), nil, servers.Icons{})
	channelService := channels.NewService(channels.NewPostgresStore(pool), workspaces)
	conversationService := conversations.NewService(conversations.NewPostgresStore(pool), 10, allowAll{})
	channelService.SetParticipants(conversationService)
	svc := NewService(NewPostgresStore(queries), channelService, workspaces, nil, nil)
	svc.SetConversations(conversationService)
	svc.SetWorkspaces(workspaces)

	// Carol owns the server; Anna is a member and the one who leaves.
	server, err := workspaces.Create(ctx, servers.NewWorkspace{Name: "Общий"}, carol.ID)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if _, err := workspaces.Join(ctx, server.Workspace.ID, anna.ID); err != nil {
		t.Fatalf("join anna: %v", err)
	}
	channel, err := channelService.Create(ctx, server.Workspace.ID, carol.ID, channels.NewChannel{
		WorkspaceID: server.Workspace.ID, Kind: channels.KindText, Name: "общий",
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	channelMessage, _, err := svc.Send(ctx, anna.ID, NewMessage{
		ChannelID: channel.Channel.ID, ClientMessageID: uuid.NewString(), Content: "это сообщение останется",
	})
	if err != nil {
		t.Fatalf("send channel message: %v", err)
	}

	// A direct conversation keeps its history for the other participant.
	direct, _, err := conversationService.Start(ctx, anna.ID, []uuid.UUID{carol.ID})
	if err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	directMessage, _, err := svc.Send(ctx, anna.ID, NewMessage{
		ChannelID: direct.ID, ClientMessageID: uuid.NewString(), Content: "личная переписка",
	})
	if err != nil {
		t.Fatalf("send direct message: %v", err)
	}

	// Personal rows that a delete has to cascade: a device, a push subscription,
	// notification settings, a friendship and the memberships made above.
	if _, err := pool.Exec(ctx,
		`INSERT INTO devices (user_id, identity_key, signing_key) VALUES ($1, 'identity', 'signing')`,
		anna.ID); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO push_subscriptions (id, user_id, endpoint, p256dh, auth)
		 VALUES ($1, $2, 'https://push.example/anna', 'key', 'auth')`,
		uuid.New(), anna.ID); err != nil {
		t.Fatalf("insert push subscription: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO notification_settings (user_id, scope, target_id, level) VALUES ($1, 'workspace', $2, 'all')`,
		anna.ID, server.Workspace.ID); err != nil {
		t.Fatalf("insert notification setting: %v", err)
	}
	low, high := anna.ID, carol.ID
	var ordered bool
	if err := pool.QueryRow(ctx, `SELECT $1::uuid < $2::uuid`, low, high).Scan(&ordered); err != nil {
		t.Fatalf("order pair: %v", err)
	}
	if !ordered {
		low, high = high, low
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO friendships (user_low, user_high, status, requested_by, accepted_at) VALUES ($1, $2, 'accepted', $1, now())`,
		low, high); err != nil {
		t.Fatalf("insert friendship: %v", err)
	}

	if err := accounts.DeleteAccount(ctx, anna.ID, deleteAccountPassword); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	// Anna is gone, and every row that belonged to her alone went with her.
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, anna.ID).Scan(&left); err != nil {
		t.Fatalf("count user: %v", err)
	}
	if left != 0 {
		t.Error("the account row is still there")
	}
	for table, query := range map[string]string{
		"sessions":              `SELECT count(*) FROM sessions WHERE user_id = $1`,
		"workspace_members":     `SELECT count(*) FROM workspace_members WHERE user_id = $1`,
		"channel_members":       `SELECT count(*) FROM channel_members WHERE user_id = $1`,
		"devices":               `SELECT count(*) FROM devices WHERE user_id = $1`,
		"push_subscriptions":    `SELECT count(*) FROM push_subscriptions WHERE user_id = $1`,
		"notification_settings": `SELECT count(*) FROM notification_settings WHERE user_id = $1`,
		"friendships":           `SELECT count(*) FROM friendships WHERE user_low = $1 OR user_high = $1`,
		"password_reset_tokens": `SELECT count(*) FROM password_reset_tokens WHERE user_id = $1`,
	} {
		var rows int
		if err := pool.QueryRow(ctx, query, anna.ID).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if rows != 0 {
			t.Errorf("%s kept %d rows of the deleted account", table, rows)
		}
	}

	// The shared channel and its message stay, with no author.
	page, err := svc.History(ctx, channel.Channel.ID, carol.ID, nil, 50)
	if err != nil {
		t.Fatalf("history after deletion: %v", err)
	}
	found := false
	for _, message := range page.Messages {
		if message.ID != channelMessage.ID {
			continue
		}
		found = true
		if message.Content != "это сообщение останется" {
			t.Errorf("content = %q, want it untouched", message.Content)
		}
		if message.Author != nil {
			t.Errorf("author = %+v, want a deleted-account placeholder", message.Author)
		}
	}
	if !found {
		t.Error("the channel message disappeared with its author")
	}

	// Search works over the same NULL author.
	hits, err := svc.Search(ctx, carol.ID, SearchParams{Query: "останется"})
	if err != nil {
		t.Fatalf("search after deletion: %v", err)
	}
	found = false
	for _, hit := range hits.Results {
		if hit.Message.ID != channelMessage.ID {
			continue
		}
		found = true
		if hit.Message.Author != nil {
			t.Errorf("search author = %+v, want nil", hit.Message.Author)
		}
	}
	if !found {
		t.Error("search no longer finds the message of a deleted account")
	}

	// The dialogue stays for Carol, who keeps her copy of it.
	page, err = svc.History(ctx, direct.ID, carol.ID, nil, 50)
	if err != nil {
		t.Fatalf("direct history after deletion: %v", err)
	}
	found = false
	for _, message := range page.Messages {
		if message.ID == directMessage.ID {
			found = true
			if message.Author != nil {
				t.Errorf("direct author = %+v, want nil", message.Author)
			}
		}
	}
	if !found {
		t.Error("the direct message disappeared with its author")
	}
	list, err := conversationService.List(ctx, carol.ID, "", 20)
	if err != nil {
		t.Fatalf("list conversations: %v", err)
	}
	found = false
	for _, conversation := range list.Conversations {
		if conversation.ID == direct.ID {
			found = true
		}
	}
	if !found {
		t.Error("the dialogue disappeared for the remaining participant")
	}

	// Carol still owns the server; deleting her account is refused instead of
	// silently taking the community with it.
	if err := accounts.DeleteAccount(ctx, carol.ID, deleteAccountPassword); !errors.Is(err, auth.ErrOwnsServers) {
		t.Errorf("owner delete error = %v, want ErrOwnsServers", err)
	}
	if _, err := accounts.Authenticate(ctx, carolCreds.Token); err != nil {
		t.Errorf("the refused delete disturbed the owner: %v", err)
	}
	if _, err := accounts.Authenticate(ctx, annaCreds.Token); err == nil {
		t.Error("the deleted account still has a live session")
	}
}
