package messages

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

// fakeWorkspaceList answers like internal/servers for one account.
type fakeWorkspaceList struct {
	servers map[uuid.UUID][]uuid.UUID
}

func (l *fakeWorkspaceList) Servers(_ context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	return l.servers[userID], nil
}

func searchWorld(t *testing.T) (world, *fakeStore) {
	t.Helper()
	w := newWorld(t)
	hidden, secret := uuid.New(), uuid.New()
	w.access.channels[hidden] = channels.Channel{ID: hidden, WorkspaceID: w.workspace, Kind: channels.KindText, Name: "скрытый"}
	w.access.channels[secret] = channels.Channel{ID: secret, Kind: channels.KindSecret, Name: "секрет"}
	w.access.readers[secret] = map[uuid.UUID]bool{w.anna: true}
	w.svc.SetWorkspaces(&fakeWorkspaceList{servers: map[uuid.UUID][]uuid.UUID{
		w.anna: {w.workspace},
		w.bob:  {w.workspace},
	}})
	return w, w.store
}

func foundHit(w world, channelID uuid.UUID, seq int64, text string) SearchHit {
	return SearchHit{
		Message: Message{
			ID: uuid.New(), ChannelID: channelID, Seq: seq, Kind: MessageKindUser,
			Content: text, Author: &Author{ID: w.bob, Nickname: "bob"},
		},
		Headline: text,
	}
}

func TestSearchRefusesWhatTheCallerMayNotRead(t *testing.T) {
	w, _ := searchWorld(t)
	hidden := uuid.New()
	w.access.channels[hidden] = channels.Channel{ID: hidden, WorkspaceID: w.workspace, Kind: channels.KindText}
	secret := uuid.New()
	w.access.channels[secret] = channels.Channel{ID: secret, Kind: channels.KindSecret}
	w.access.readers[secret] = map[uuid.UUID]bool{w.anna: true}
	foreignConversation := uuid.New()
	w.access.channels[foreignConversation] = channels.Channel{ID: foreignConversation, Kind: channels.KindText}
	w.access.readers[foreignConversation] = map[uuid.UUID]bool{w.admin: true}

	missingWorkspace := uuid.New()
	cases := []struct {
		name   string
		params SearchParams
	}{
		{"a hidden channel", SearchParams{Query: "привет", ChannelID: &hidden}},
		{"a secret chat as a channel", SearchParams{Query: "привет", ChannelID: &secret}},
		{"a conversation as a channel", SearchParams{Query: "привет", ChannelID: &w.direct}},
		{"a conversation the caller is not in", SearchParams{Query: "привет", ConversationID: &foreignConversation}},
		{"a server the caller is not in", SearchParams{Query: "привет", WorkspaceID: &missingWorkspace}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.svc.Search(t.Context(), w.anna, tc.params); !errors.Is(err, ErrNotFound) {
				t.Errorf("Search = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestSearchRejectsAnEmptyOrOverlongQuery(t *testing.T) {
	w, _ := searchWorld(t)
	for _, query := range []string{"", "   ", string(make([]rune, maxSearchQueryLength+1))} {
		if _, err := w.svc.Search(t.Context(), w.anna, SearchParams{Query: query}); !errors.Is(err, ErrInvalidSearch) {
			t.Errorf("query %q = %v, want ErrInvalidSearch", query, err)
		}
	}
}

func TestSearchReturnsHitsWithTheChannelAndACursor(t *testing.T) {
	w, store := searchWorld(t)
	store.searchHits = []SearchHit{
		foundHit(w, w.channel, 30, "первое"),
		foundHit(w, w.channel, 20, "второе"),
		foundHit(w, w.channel, 10, "третье"),
	}
	// The service asks for one more than the page to know whether a next page exists.
	page, err := w.svc.Search(t.Context(), w.anna, SearchParams{Query: "сообщение", Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 2 {
		t.Fatalf("results = %d, want the page size", len(page.Results))
	}
	if page.NextCursor == nil || *page.NextCursor != 20 {
		t.Errorf("next cursor = %v, want the last result's seq 20", page.NextCursor)
	}
	if page.Results[0].ChannelName != "общий" || page.Results[0].WorkspaceID == nil || *page.Results[0].WorkspaceID != w.workspace {
		t.Errorf("first result names %q / %v, want the channel and its server", page.Results[0].ChannelName, page.Results[0].WorkspaceID)
	}
}

func TestSearchAllCoversServersAndConversationsButNeverSecretChats(t *testing.T) {
	w, store := searchWorld(t)
	// The direct conversation is in newWorld; a secret one is not a conversation.
	secretConversation := uuid.New()
	w.svc.SetConversations(&fakeConversations{
		members: map[uuid.UUID][]uuid.UUID{
			w.direct:           {w.anna, w.bob},
			secretConversation: {w.anna, w.bob},
		},
		kinds: map[uuid.UUID]conversations.Kind{
			w.direct:           conversations.KindDirect,
			secretConversation: conversations.KindSecret,
		},
	})
	store.searchHits = []SearchHit{foundHit(w, w.direct, 5, "личное")}

	if _, err := w.svc.Search(t.Context(), w.anna, SearchParams{Query: "личное"}); err != nil {
		t.Fatalf("Search across everything: %v", err)
	}
	// The SQL got the ordinary conversation and the server's visible channels,
	// never the secret chat.
	if !slices.Contains(store.lastSearch.ChannelIDs, w.direct) {
		t.Error("the ordinary conversation is missing from the search scope")
	}
	if slices.Contains(store.lastSearch.ChannelIDs, secretConversation) {
		t.Error("a secret chat reached the SQL query")
	}
}

func TestSearchAsksTheStoreForVisibleChannelsOnly(t *testing.T) {
	w, store := searchWorld(t)
	hidden := uuid.New()
	w.access.channels[hidden] = channels.Channel{ID: hidden, WorkspaceID: w.workspace, Kind: channels.KindText}
	if _, err := w.svc.Search(t.Context(), w.anna, SearchParams{Query: "привет", WorkspaceID: &w.workspace}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if slices.Contains(store.lastSearch.ChannelIDs, hidden) {
		t.Error("a hidden channel reached the SQL query")
	}
	if !slices.Contains(store.lastSearch.ChannelIDs, w.channel) {
		t.Errorf("the visible channel is missing: %v", store.lastSearch.ChannelIDs)
	}
}

func TestSearchExcludesBlockedAuthors(t *testing.T) {
	w, store := searchWorld(t)
	w.svc.SetPrivacy(&fakePrivacy{blockedBy: map[uuid.UUID]map[uuid.UUID]bool{w.anna: {w.bob: true}}})
	store.searchHits = []SearchHit{foundHit(w, w.channel, 1, "привет")}
	if _, err := w.svc.Search(t.Context(), w.anna, SearchParams{Query: "привет", ChannelID: &w.channel}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !slices.Contains(store.lastSearch.ExcludedAuthors, w.bob) {
		t.Errorf("excluded authors = %v, want Bob", store.lastSearch.ExcludedAuthors)
	}
}

func TestSearchIsRateLimited(t *testing.T) {
	w, _ := searchWorld(t)
	w.svc.SetSearchLimiter(ratelimit.New(1))
	if _, err := w.svc.Search(t.Context(), w.anna, SearchParams{Query: "привет"}); err != nil {
		t.Fatalf("first search: %v", err)
	}
	_, err := w.svc.Search(t.Context(), w.anna, SearchParams{Query: "привет"})
	var limited *SearchRateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("second search = %v, want SearchRateLimitedError", err)
	}
	if limited.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want a positive wait", limited.RetryAfter)
	}
}

func TestSearchWithNoVisibleChannelsSkipsTheQuery(t *testing.T) {
	w, store := searchWorld(t)
	w.access.readers = map[uuid.UUID]map[uuid.UUID]bool{}
	w.svc.SetConversations(&fakeConversations{})
	page, err := w.svc.Search(t.Context(), w.anna, SearchParams{Query: "привет"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 0 {
		t.Errorf("results = %v, want none", page.Results)
	}
	if len(store.lastSearch.ChannelIDs) != 0 {
		t.Error("the store was asked with no channels")
	}
}

// The limiter is per account: one person's searches do not spend another's.
func TestSearchLimitIsPerAccount(t *testing.T) {
	w, _ := searchWorld(t)
	w.svc.SetSearchLimiter(ratelimit.New(1))
	if _, err := w.svc.Search(t.Context(), w.anna, SearchParams{Query: "привет"}); err != nil {
		t.Fatalf("Anna: %v", err)
	}
	if _, err := w.svc.Search(t.Context(), w.bob, SearchParams{Query: "привет"}); err != nil {
		t.Errorf("Bob: %v, want his own bucket", err)
	}
}
