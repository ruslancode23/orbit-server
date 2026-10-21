package conversations

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/events"
)

func TestListCarriesLastMessageUnreadAndGroupName(t *testing.T) {
	w := newWorld(t)
	direct, _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob})
	if err != nil {
		t.Fatalf("Start direct: %v", err)
	}
	group, _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob, w.vika})
	if err != nil {
		t.Fatalf("Start group: %v", err)
	}
	w.store.conversations[group.ID].Name = "Design"
	seedLastMessage(w.store, direct.ID, w.bob, 7, "привет")
	seedLastMessage(w.store, group.ID, w.vika, 9, "план")
	w.store.conversations[direct.ID].LastReadSeq = 4
	w.store.conversations[direct.ID].Unread = 2
	w.store.conversations[direct.ID].Mentions = 1
	w.store.conversations[group.ID].Unread = 3

	profiles := &countingProfiles{}
	w.svc.SetProfiles(profiles)
	page, err := w.svc.List(t.Context(), w.anna, "", 20)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if w.store.listCalls != 1 {
		t.Errorf("store list calls = %d, want 1 for the whole page", w.store.listCalls)
	}
	if profiles.calls != 1 {
		t.Errorf("profile calls = %d, want 1 for the whole page", profiles.calls)
	}

	byID := map[uuid.UUID]Conversation{}
	for _, conversation := range page.Conversations {
		byID[conversation.ID] = conversation
	}
	gotDirect, ok := byID[direct.ID]
	if !ok {
		t.Fatalf("the direct conversation is missing from %+v", page.Conversations)
	}
	if gotDirect.LastMessage == nil {
		t.Fatal("the direct conversation has no last message")
	}
	if gotDirect.LastMessage.Content != "привет" || gotDirect.LastMessage.Seq != 7 {
		t.Errorf("last message = %+v, want the seeded one", gotDirect.LastMessage)
	}
	if gotDirect.LastMessage.Author == nil || gotDirect.LastMessage.Author.ID != w.bob {
		t.Errorf("last author = %+v, want Bob", gotDirect.LastMessage.Author)
	}
	if gotDirect.LastMessage.Author.DisplayName != "name-"+w.bob.String()[:8] {
		t.Errorf("last author was not filled from the profile batch: %+v", gotDirect.LastMessage.Author)
	}
	if gotDirect.Unread != 2 || gotDirect.Mentions != 1 || gotDirect.LastReadSeq != 4 {
		t.Errorf("direct counters = %+v, want unread 2, mentions 1, cursor 4", gotDirect)
	}

	gotGroup, ok := byID[group.ID]
	if !ok {
		t.Fatalf("the group is missing from %+v", page.Conversations)
	}
	if gotGroup.Name != "Design" {
		t.Errorf("group name = %q, want Design", gotGroup.Name)
	}
	if gotGroup.Unread != 3 {
		t.Errorf("group unread = %d, want 3", gotGroup.Unread)
	}
	// The liveliest conversation comes first: the group's seq is 9, the direct 7.
	if page.Conversations[0].ID != group.ID {
		t.Errorf("first conversation = %v, want the group %v", page.Conversations[0].ID, group.ID)
	}
}

// A page cursor is stable when conversations have equal activity: the id is
// the last tie-breaker, so no row appears twice and none is skipped.
func TestConversationListPaginatesWithAStableCursor(t *testing.T) {
	w := newWorld(t)
	createdAt := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	ids := make([]uuid.UUID, 0, 3)
	for range 3 {
		peer := uuid.New()
		w.store.known[peer] = true
		conversation, err := w.store.Create(t.Context(), KindDirect, "", []uuid.UUID{w.anna, peer})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		conversation.CreatedAt = createdAt
		w.store.conversations[conversation.ID].CreatedAt = createdAt
		seedLastMessage(w.store, conversation.ID, peer, 5, "одно и то же")
		ids = append(ids, conversation.ID)
	}

	expected := append([]uuid.UUID(nil), ids...)
	for i := 0; i < len(expected); i++ {
		for j := i + 1; j < len(expected); j++ {
			if expected[j].String() > expected[i].String() {
				expected[i], expected[j] = expected[j], expected[i]
			}
		}
	}

	first, err := w.svc.List(t.Context(), w.anna, "", 2)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Conversations) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %+v, want two rows and a cursor", first)
	}
	if first.Conversations[0].ID != expected[0] || first.Conversations[1].ID != expected[1] {
		t.Errorf("first page ids = %v, %v, want %v, %v",
			first.Conversations[0].ID, first.Conversations[1].ID, expected[0], expected[1])
	}

	second, err := w.svc.List(t.Context(), w.anna, first.NextCursor, 2)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Conversations) != 1 || second.NextCursor != "" {
		t.Fatalf("second page = %+v, want the last row and no cursor", second)
	}
	if second.Conversations[0].ID != expected[2] {
		t.Errorf("second page id = %v, want %v", second.Conversations[0].ID, expected[2])
	}

	// A cursor the server never issued is the client's mistake, not a 500.
	if _, err := w.svc.List(t.Context(), w.anna, "not-a-cursor", 2); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("bad cursor: err = %v, want ErrInvalidCursor", err)
	}
}

func TestUnreadSummarySkipsHiddenConversationsAndSecrets(t *testing.T) {
	w := newWorld(t)
	direct, _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob})
	if err != nil {
		t.Fatalf("Start direct: %v", err)
	}
	group, _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob, w.vika})
	if err != nil {
		t.Fatalf("Start group: %v", err)
	}
	secret, _, err := w.svc.EnsureSecret(t.Context(), "secret:"+uuid.NewString(), []uuid.UUID{w.anna, w.bob})
	if err != nil {
		t.Fatalf("EnsureSecret: %v", err)
	}
	w.store.conversations[direct.ID].Unread = 2
	w.store.conversations[direct.ID].Mentions = 1
	w.store.conversations[group.ID].Unread = 3
	w.store.conversations[secret.ID].Unread = 100

	summary, err := w.svc.Unread(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("Unread: %v", err)
	}
	if summary.Unread != 5 || summary.Mentions != 1 {
		t.Errorf("summary = %+v, want unread 5, mentions 1", summary)
	}

	seedLastMessage(w.store, direct.ID, w.bob, 7, "новое")
	if err := w.svc.SetHidden(t.Context(), direct.ID, w.anna, true); err != nil {
		t.Fatalf("SetHidden: %v", err)
	}
	summary, err = w.svc.Unread(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("Unread after hiding: %v", err)
	}
	if summary.Unread != 3 || summary.Mentions != 0 {
		t.Errorf("summary after hiding = %+v, want the direct conversation left out", summary)
	}
}

// Hiding is private and lasts until the next message: the same message rule
// applies to the badge, so a hidden conversation stops counting.
func TestHiddenConversationComesBackWithANewMessage(t *testing.T) {
	w := newWorld(t)
	conversation, _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	seedLastMessage(w.store, conversation.ID, w.bob, 4, "старое")
	w.store.conversations[conversation.ID].Unread = 1
	recorder := &eventRecorder{}
	w.svc.SetPublisher(recorder)

	if err := w.svc.SetHidden(t.Context(), conversation.ID, w.anna, true); err != nil {
		t.Fatalf("SetHidden: %v", err)
	}
	page, err := w.svc.List(t.Context(), w.anna, "", 20)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Conversations) != 0 {
		t.Errorf("hidden conversation is still listed: %+v", page.Conversations)
	}
	if len(recorder.events) != 1 || recorder.events[0].Kind != events.KindConversationHidden {
		t.Fatalf("events = %+v, want one conversation.hidden", recorder.events)
	}
	if recorder.users[0] != w.anna {
		t.Errorf("hidden event went to %v, want only the caller", recorder.users)
	}
	payload, ok := recorder.events[0].Payload.(map[string]any)
	if !ok || payload["hidden"] != true {
		t.Errorf("hidden payload = %#v, want hidden=true", recorder.events[0].Payload)
	}

	// The other participant is not affected: hiding is a per-account sidebar state.
	other, err := w.svc.List(t.Context(), w.bob, "", 20)
	if err != nil {
		t.Fatalf("Bob's List: %v", err)
	}
	if len(other.Conversations) != 1 {
		t.Errorf("Bob sees %d conversations, want the dialogue still there", len(other.Conversations))
	}

	seedLastMessage(w.store, conversation.ID, w.bob, 5, "новое")
	w.store.conversations[conversation.ID].Unread = 2
	page, err = w.svc.List(t.Context(), w.anna, "", 20)
	if err != nil {
		t.Fatalf("List after a new message: %v", err)
	}
	if len(page.Conversations) != 1 {
		t.Fatalf("a new message did not bring the conversation back: %+v", page.Conversations)
	}
	summary, err := w.svc.Unread(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("Unread: %v", err)
	}
	if summary.Unread != 2 {
		t.Errorf("unread after the new message = %d, want 2", summary.Unread)
	}
}

// A secret chat is not in the list, the badge or the group operations: it has
// its own endpoints and its own metadata rules.
func TestSecretChatsStayOutOfTheConversationList(t *testing.T) {
	w := newWorld(t)
	if _, _, err := w.svc.EnsureSecret(t.Context(), "secret:"+uuid.NewString(), []uuid.UUID{w.anna, w.bob}); err != nil {
		t.Fatalf("EnsureSecret: %v", err)
	}
	page, err := w.svc.List(t.Context(), w.anna, "", 20)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Conversations) != 0 {
		t.Errorf("secret chat appeared in the list: %+v", page.Conversations)
	}
	summary, err := w.svc.Unread(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("Unread: %v", err)
	}
	if summary.Unread != 0 || summary.Mentions != 0 {
		t.Errorf("secret chat counted in the badge: %+v", summary)
	}
}

// The handler answers the generated page shape, and a bad cursor is a 400.
func TestHandlerListReturnsAPage(t *testing.T) {
	w := newWorld(t)
	if _, _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := NewHandler(w.svc, discardLogger())
	ctx := sessionContext(t, w.anna)

	response, err := h.ListConversations(ctx, api.ListConversationsRequestObject{})
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	ok, isPage := response.(api.ListConversations200JSONResponse)
	if !isPage {
		t.Fatalf("response = %T, want 200", response)
	}
	if len(ok.Conversations) != 1 {
		t.Errorf("conversations = %d, want 1", len(ok.Conversations))
	}

	bad := "not-a-cursor"
	response, err = h.ListConversations(ctx, api.ListConversationsRequestObject{
		Params: api.ListConversationsParams{Cursor: &bad},
	})
	if err != nil {
		t.Fatalf("ListConversations with a bad cursor: %v", err)
	}
	if _, isBadRequest := response.(api.ListConversations400JSONResponse); !isBadRequest {
		t.Errorf("response = %T, want 400", response)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func sessionContext(t *testing.T, userID uuid.UUID) context.Context {
	t.Helper()
	return auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: userID}})
}
