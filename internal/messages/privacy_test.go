package messages

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

func (s *fakeStore) HasMessages(_ context.Context, channelID uuid.UUID) (bool, error) {
	for _, m := range s.messages {
		if m.ChannelID == channelID {
			return true, nil
		}
	}
	return false, nil
}

// fakePrivacy answers the blocking questions the messages module asks; the
// rules themselves live in internal/privacy and are tested there.
type fakePrivacy struct {
	// blockedBy maps a blocker to the accounts it blocked.
	blockedBy map[uuid.UUID]map[uuid.UUID]bool
}

func (p *fakePrivacy) BlockedEitherWay(_ context.Context, a, b uuid.UUID) (bool, error) {
	return p.blockedBy[a][b] || p.blockedBy[b][a], nil
}

func (p *fakePrivacy) BlockedIDs(_ context.Context, userID uuid.UUID) (map[uuid.UUID]bool, error) {
	return p.blockedBy[userID], nil
}

// addConversation registers one more channel without a server, for a test that
// needs a second dialogue.
func (w world) addConversation(t *testing.T, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	w.access.channels[id] = channels.Channel{ID: id, Name: "conversation"}
	w.access.readers[id] = map[uuid.UUID]bool{}
	w.access.writers[id] = map[uuid.UUID]bool{}
	for _, member := range members {
		w.access.readers[id][member] = true
		w.access.writers[id][member] = true
	}
	fake, ok := w.svc.conversations.(*fakeConversations)
	if !ok {
		t.Fatalf("conversations dependency = %T, want the fake", w.svc.conversations)
	}
	fake.members[id] = members
	fake.kinds[id] = conversations.KindGroup
	return id
}

// A blocked author's messages stay in a group's history, marked only for the
// person who did the blocking.
func TestGroupHistoryMarksBlockedAuthorsForTheBlocker(t *testing.T) {
	w := newWorld(t)
	group := w.addConversation(t, w.anna, w.bob, w.admin)
	w.svc.SetPrivacy(&fakePrivacy{blockedBy: map[uuid.UUID]map[uuid.UUID]bool{
		w.bob: {w.anna: true},
	}})

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: group, ClientMessageID: "g-1", Content: "от Анны",
	}); err != nil {
		t.Fatalf("Send by Anna: %v", err)
	}
	if _, _, err := w.svc.Send(t.Context(), w.bob, NewMessage{
		ChannelID: group, ClientMessageID: "g-2", Content: "от Боба",
	}); err != nil {
		t.Fatalf("Send by Bob: %v", err)
	}

	page, err := w.svc.History(t.Context(), group, w.bob, nil, 50)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	for _, m := range page.Messages {
		switch m.Author.ID {
		case w.anna:
			if !m.Blocked {
				t.Error("Anna's message is not marked for Bob, who blocked her")
			}
		case w.bob:
			if m.Blocked {
				t.Error("Bob's own message is marked")
			}
		}
	}

	// A third person did not block anyone, so nothing is marked for them.
	page, err = w.svc.History(t.Context(), group, w.admin, nil, 50)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	for _, m := range page.Messages {
		if m.Blocked {
			t.Errorf("message %v is marked for an outsider", m.ID)
		}
	}
}

// A live event in a group is personal, so the mark travels with it: the blocker
// gets it, the rest of the group does not.
func TestGroupLiveEventCarriesTheMarkOnlyToTheBlocker(t *testing.T) {
	w := newWorld(t)
	group := w.addConversation(t, w.anna, w.bob, w.admin)
	w.svc.SetPrivacy(&fakePrivacy{blockedBy: map[uuid.UUID]map[uuid.UUID]bool{
		w.bob: {w.anna: true},
	}})

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: group, ClientMessageID: "g-1", Content: "привет",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	seen := map[uuid.UUID]bool{}
	for i, recipient := range w.published.personal {
		message, ok := w.published.published[i].Payload.(api.Message)
		if !ok {
			t.Fatalf("payload = %T, want api.Message", w.published.published[i].Payload)
		}
		seen[recipient] = true
		marked := message.Blocked != nil && *message.Blocked
		switch recipient {
		case w.bob:
			if !marked {
				t.Error("the blocker got the event without the mark")
			}
		case w.anna, w.admin:
			if marked {
				t.Errorf("recipient %v got a mark that is not theirs", recipient)
			}
		}
	}
	if len(seen) != 3 {
		t.Errorf("published to %d participants, want 3", len(seen))
	}
}

// A block freezes the direct dialogue in both directions: no new message is
// stored, and the refusal is the same 404 as for a channel one cannot read.
func TestDirectDialogueIsFrozenByABlock(t *testing.T) {
	w := newWorld(t)
	w.svc.SetPrivacy(&fakePrivacy{blockedBy: map[uuid.UUID]map[uuid.UUID]bool{
		w.bob: {w.anna: true},
	}})

	_, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.direct, ClientMessageID: "d-1", Content: "привет",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(w.store.messages) != 0 {
		t.Error("a message was stored in a frozen dialogue")
	}

	// The blocker cannot write there either: the dialogue is closed both ways.
	_, _, err = w.svc.Send(t.Context(), w.bob, NewMessage{
		ChannelID: w.direct, ClientMessageID: "d-2", Content: "привет",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("the blocker's send: err = %v, want ErrNotFound", err)
	}
}

// The first message of a new dialogue spends the per-account token; later
// messages in the same dialogue do not.
func TestFirstMessageOfANewDialogueIsRateLimited(t *testing.T) {
	w := newWorld(t)
	w.svc.SetLimiter(ratelimit.New(1))

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.direct, ClientMessageID: "d-1", Content: "первое",
	}); err != nil {
		t.Fatalf("first message: %v", err)
	}
	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.direct, ClientMessageID: "d-2", Content: "второе",
	}); err != nil {
		t.Fatalf("second message in the same dialogue: %v", err)
	}

	another := w.addConversation(t, w.anna, w.bob)
	_, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: another, ClientMessageID: "g-1", Content: "новый диалог",
	})
	var limited *RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want *RateLimitedError", err)
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want it to wrap ErrRateLimited", err)
	}
	if limited.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want a positive wait", limited.RetryAfter)
	}
}

// The HTTP answer is 429 with Retry-After, so the client knows when to retry.
func TestHandlerAnswersRateLimitWithRetryAfter(t *testing.T) {
	w := newWorld(t)
	w.svc.SetLimiter(ratelimit.New(1))
	h := NewHandler(w.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: w.anna}})

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.direct, ClientMessageID: "d-1", Content: "первое",
	}); err != nil {
		t.Fatalf("first message: %v", err)
	}
	another := w.addConversation(t, w.anna, w.bob)
	body := api.SendMessageJSONRequestBody{ClientMessageId: "g-1", Content: "новый диалог"}

	response, err := h.SendMessage(ctx, api.SendMessageRequestObject{ChannelId: another, Body: &body})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	limited, ok := response.(api.SendMessage429JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 429", response)
	}
	if limited.Headers.RetryAfter == nil || *limited.Headers.RetryAfter < 1 {
		t.Errorf("Retry-After = %v, want at least one second", limited.Headers.RetryAfter)
	}
	if limited.Body.Error.Code == "" {
		t.Errorf("error body = %+v, want a machine-readable code", limited.Body)
	}
}
