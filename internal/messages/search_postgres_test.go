package messages

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/perms"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

// allowAll is the conversations policy these tests need: the search never
// tests who may start a dialogue.
type allowAll struct{}

func (allowAll) CanStartDirect(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return true, nil
}

func (allowAll) CanStartGroup(context.Context, uuid.UUID, []uuid.UUID) (bool, error) {
	return true, nil
}

// Search on a real PostgreSQL: the dictionaries, the visibility rule and the
// exclusions the plan names — another server, a hidden channel, a conversation
// without the caller, a secret chat, a deleted message and a system line.
func TestPostgresSearchRespectsVisibility(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := t.Context()
	queries := db.New(pool)

	user := func(nickname string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO users (nickname, is_guest) VALUES ($1, true) RETURNING id`, nickname).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	anna, bob, carol := user("anna"), user("bob"), user("carol")

	workspaces := servers.NewService(servers.NewPostgresStore(pool), nil, servers.Icons{})
	channelService := channels.NewService(channels.NewPostgresStore(pool), workspaces)
	conversationService := conversations.NewService(conversations.NewPostgresStore(pool), 10, allowAll{})
	channelService.SetParticipants(conversationService)
	svc := NewService(NewPostgresStore(queries), channelService, workspaces, nil, nil)
	svc.SetConversations(conversationService)
	svc.SetWorkspaces(workspaces)

	// Bob owns the server and invites Anna. Carol has a server of her own.
	server, err := workspaces.Create(ctx, servers.NewWorkspace{Name: "Поиск"}, bob)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if _, err := workspaces.Join(ctx, server.Workspace.ID, anna); err != nil {
		t.Fatalf("join anna: %v", err)
	}
	other, err := workspaces.Create(ctx, servers.NewWorkspace{Name: "Чужой"}, carol)
	if err != nil {
		t.Fatalf("create other server: %v", err)
	}
	foreignChannel, err := channelService.Create(ctx, other.Workspace.ID, carol, channels.NewChannel{
		WorkspaceID: other.Workspace.ID, Kind: channels.KindText, Name: "чужой",
	})
	if err != nil {
		t.Fatalf("create foreign channel: %v", err)
	}

	main, err := channelService.Create(ctx, server.Workspace.ID, bob, channels.NewChannel{
		WorkspaceID: server.Workspace.ID, Kind: channels.KindText, Name: "общий",
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	hidden, err := channelService.Create(ctx, server.Workspace.ID, bob, channels.NewChannel{
		WorkspaceID: server.Workspace.ID, Kind: channels.KindText, Name: "скрытый",
	})
	if err != nil {
		t.Fatalf("create hidden channel: %v", err)
	}
	// @everyone may not see the hidden channel; its owner still may.
	var everyone uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM server_roles WHERE workspace_id = $1 AND is_default`, server.Workspace.ID).Scan(&everyone); err != nil {
		t.Fatal(err)
	}
	if _, err := channelService.SetOverride(ctx, hidden.Channel.ID, bob, perms.Override{
		RoleID: everyone, Deny: perms.ViewChannel,
	}); err != nil {
		t.Fatalf("hide channel: %v", err)
	}

	send := func(author, channelID uuid.UUID, text string) Message {
		t.Helper()
		message, _, err := svc.Send(ctx, author, NewMessage{
			ChannelID: channelID, ClientMessageID: uuid.NewString(), Content: text,
		})
		if err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
		return message
	}

	// Russian morphology, a deleted message and a server line in the open channel.
	send(anna, main.Channel.ID, "Это сообщение про поиск")
	send(anna, main.Channel.ID, "deployment pipeline inside")
	deleted := send(anna, main.Channel.ID, "удалённая запись")
	if _, err := svc.Delete(ctx, deleted.ID, anna); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.store.CreateSystem(ctx, NewSystemMessage{
		ChannelID: main.Channel.ID, AuthorID: bob, Content: "системная строка",
		Type: conversations.SystemMemberAdded,
	}); err != nil {
		t.Fatalf("system line: %v", err)
	}
	send(bob, hidden.Channel.ID, "секретное место")
	send(carol, foreignChannel.Channel.ID, "чужой сервер")

	// A conversation with Anna, and one with Carol that Anna is not in.
	direct, _, err := conversationService.Start(ctx, bob, []uuid.UUID{anna})
	if err != nil {
		t.Fatalf("start dialogue: %v", err)
	}
	send(bob, direct.ID, "личное сообщение")
	withCarol, _, err := conversationService.Start(ctx, bob, []uuid.UUID{carol})
	if err != nil {
		t.Fatalf("start dialogue with carol: %v", err)
	}
	send(bob, withCarol.ID, "личное с карол")

	// A secret chat is a channel too, but no text may be findable in it.
	secret, _, err := conversationService.EnsureSecret(ctx, "test-secret-key", []uuid.UUID{anna, bob})
	if err != nil {
		t.Fatalf("ensure secret: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO messages (channel_id, author_id, client_message_id, content)
		VALUES ($1, $2, 'secret-1', 'секретный текст')`, secret.ID, anna); err != nil {
		t.Fatalf("secret message: %v", err)
	}

	found := func(searcher uuid.UUID, query string, params SearchParams) []uuid.UUID {
		t.Helper()
		params.Query = query
		page, err := svc.Search(ctx, searcher, params)
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		out := make([]uuid.UUID, 0, len(page.Results))
		for _, result := range page.Results {
			out = append(out, result.Message.ChannelID)
		}
		return out
	}
	contains := func(ids []uuid.UUID, id uuid.UUID) bool {
		for _, value := range ids {
			if value == id {
				return true
			}
		}
		return false
	}

	// Russian and English dictionaries both work, and the open channel is found.
	if ids := found(anna, "сообщение", SearchParams{}); !contains(ids, main.Channel.ID) {
		t.Errorf("«сообщение» did not find «сообщение»: %v", ids)
	}
	if ids := found(anna, "deploy", SearchParams{}); !contains(ids, main.Channel.ID) {
		t.Errorf("«deploy» did not find «deployment»: %v", ids)
	}
	// Deleted and system lines are not searchable.
	if ids := found(anna, "удалённая", SearchParams{}); len(ids) != 0 {
		t.Errorf("a deleted message was found: %v", ids)
	}
	if ids := found(anna, "системная", SearchParams{}); len(ids) != 0 {
		t.Errorf("a system line was found: %v", ids)
	}
	// The hidden channel is the owner's, not Anna's.
	if ids := found(anna, "секретное", SearchParams{}); len(ids) != 0 {
		t.Errorf("Anna found a hidden channel: %v", ids)
	}
	if ids := found(bob, "секретное", SearchParams{}); !contains(ids, hidden.Channel.ID) {
		t.Errorf("the owner did not find the hidden channel: %v", ids)
	}
	// Another server, a secret chat and somebody else's conversation stay dark.
	if ids := found(anna, "чужой", SearchParams{}); len(ids) != 0 {
		t.Errorf("a foreign server's message was found: %v", ids)
	}
	if ids := found(anna, "секретный", SearchParams{}); len(ids) != 0 {
		t.Errorf("a secret chat was searched: %v", ids)
	}
	if ids := found(anna, "карол", SearchParams{}); len(ids) != 0 {
		t.Errorf("a conversation without Anna was found: %v", ids)
	}
	// The author filter narrows to one person's messages.
	if ids := found(anna, "личное", SearchParams{AuthorID: &bob}); !contains(ids, direct.ID) {
		t.Errorf("the author filter lost Bob's message: %v", ids)
	}
	if ids := found(anna, "личное", SearchParams{AuthorID: &anna}); len(ids) != 0 {
		t.Errorf("the author filter kept somebody else's message: %v", ids)
	}
	// The conversation scope searches only that dialogue.
	if ids := found(anna, "личное", SearchParams{ConversationID: &direct.ID}); len(ids) != 1 || ids[0] != direct.ID {
		t.Errorf("conversation scope = %v, want only the dialogue", ids)
	}
	// The server scope stops at its own channels.
	if ids := found(anna, "личное", SearchParams{WorkspaceID: &server.Workspace.ID}); len(ids) != 0 {
		t.Errorf("server scope leaked a conversation: %v", ids)
	}

	// The headline marks the match with control characters, never with HTML.
	page, err := svc.Search(ctx, anna, SearchParams{Query: "deploy"})
	if err != nil || len(page.Results) == 0 {
		t.Fatalf("headline search: %v (results %d)", err, len(page.Results))
	}
	if page.Results[0].Headline == "" {
		t.Error("the headline is empty")
	}
}
