package messages

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

// slowmode makes the server channel wait between two messages of one author.
func (w world) slowmode(seconds int) {
	channel := w.access.channels[w.channel]
	channel.SlowmodeSeconds = seconds
	w.access.channels[w.channel] = channel
}

// rules are what the server imposes on writing: a timeout and the caps.
func (w world) rules(rules servers.SendRules) {
	w.svc.members.(*fakeMembers).rules[w.workspace] = rules
}

// age moves every message of one author in one channel into the past, so a
// test does not have to wait for a slowmode to end.
func (w world) age(channelID, authorID uuid.UUID, age time.Duration) {
	for _, m := range w.store.messages {
		if m.ChannelID == channelID && m.Author != nil && m.Author.ID == authorID {
			m.CreatedAt = time.Now().Add(-age)
		}
	}
}

func (w world) stored() int { return len(w.store.messages) }

// Between two messages of one author the channel's slowmode must pass; the
// second one is refused with the time left, and nothing is stored.
func TestSlowmodeHoldsTheNextMessage(t *testing.T) {
	w := newWorld(t)
	w.slowmode(60)
	send(t, w, w.anna, "s-1", "первое")

	_, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "s-2", Content: "второе",
	})
	var slow *SlowmodeError
	if !errors.As(err, &slow) {
		t.Fatalf("err = %v, want *SlowmodeError", err)
	}
	if !errors.Is(err, ErrSlowmode) {
		t.Errorf("err = %v, want it to wrap ErrSlowmode", err)
	}
	if slow.RetryAfter <= 0 || slow.RetryAfter > 60*time.Second {
		t.Errorf("RetryAfter = %v, want between 0 and the slowmode", slow.RetryAfter)
	}
	if got := w.stored(); got != 1 {
		t.Errorf("stored %d messages, want only the first one", got)
	}
}

// A resend of the same message is not a second message: the idempotent retry
// after a network failure must not turn into a 429.
func TestSlowmodeDoesNotHoldBackAResend(t *testing.T) {
	w := newWorld(t)
	w.slowmode(60)
	first := send(t, w, w.anna, "s-1", "первое")

	second, existed, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "s-1", Content: "первое",
	})
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if !existed || second.ID != first.ID {
		t.Errorf("resend = %v (existed %t), want the stored %v", second.ID, existed, first.ID)
	}
}

// The wait ends: an old enough message no longer holds the next one back.
func TestSlowmodeEndsAfterItsWait(t *testing.T) {
	w := newWorld(t)
	w.slowmode(60)
	send(t, w, w.anna, "s-1", "первое")
	w.age(w.channel, w.anna, 61*time.Second)

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "s-2", Content: "второе",
	}); err != nil {
		t.Fatalf("second message after the wait: %v", err)
	}
}

// Slowmode is per author: one person waiting does not silence another.
func TestSlowmodeIsPerAuthor(t *testing.T) {
	w := newWorld(t)
	w.slowmode(60)
	send(t, w, w.anna, "s-1", "первое")

	if _, _, err := w.svc.Send(t.Context(), w.bob, NewMessage{
		ChannelID: w.channel, ClientMessageID: "b-1", Content: "от боба",
	}); err != nil {
		t.Fatalf("another author: %v", err)
	}
}

// A moderator who manages the channel or its messages is not held back.
func TestSlowmodeLetsModeratorsThrough(t *testing.T) {
	w := newWorld(t)
	w.slowmode(600)
	send(t, w, w.admin, "a-1", "первое")
	send(t, w, w.admin, "a-2", "второе")
}

// The mute is a refusal, not a rate limit: sending answers ErrMuted with the
// moment it ends, and nothing is stored.
func TestMutedMemberCannotSend(t *testing.T) {
	w := newWorld(t)
	until := time.Now().Add(time.Hour)
	w.rules(servers.SendRules{MutedUntil: &until})

	_, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "m-1", Content: "привет",
	})
	var muted *MutedError
	if !errors.As(err, &muted) {
		t.Fatalf("err = %v, want *MutedError", err)
	}
	if !errors.Is(err, ErrMuted) {
		t.Errorf("err = %v, want it to wrap ErrMuted", err)
	}
	if !muted.Until.Equal(until) {
		t.Errorf("Until = %v, want %v", muted.Until, until)
	}
	if got := w.stored(); got != 0 {
		t.Errorf("stored %d messages, want none", got)
	}
}

// Muting stops writing only: history, reactions and events stay open, and an
// expired timeout no longer refuses anything.
func TestMuteDoesNotStopReading(t *testing.T) {
	w := newWorld(t)
	first := send(t, w, w.anna, "m-1", "до мьюта")
	until := time.Now().Add(time.Hour)
	w.rules(servers.SendRules{MutedUntil: &until})

	if _, err := w.svc.History(t.Context(), w.channel, w.anna, nil, 50); err != nil {
		t.Errorf("History while muted: %v", err)
	}
	if _, err := w.svc.React(t.Context(), first.ID, w.anna, "👍"); err != nil {
		t.Errorf("React while muted: %v", err)
	}

	over := time.Now().Add(-time.Minute)
	w.rules(servers.SendRules{MutedUntil: &over})
	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "m-2", Content: "после мьюта",
	}); err != nil {
		t.Errorf("send after the timeout: %v", err)
	}
}

// An edit is new text in the same feed: without the check a muted member would
// rewrite a message instead of sending one.
func TestMutedMemberCannotEdit(t *testing.T) {
	w := newWorld(t)
	first := send(t, w, w.anna, "m-1", "старое")
	until := time.Now().Add(time.Hour)
	w.rules(servers.SendRules{MutedUntil: &until})

	if _, err := w.svc.Edit(t.Context(), first.ID, w.anna, "новое"); !errors.Is(err, ErrMuted) {
		t.Errorf("Edit = %v, want ErrMuted", err)
	}
}

// A conversation has no server settings and no timeout: the caps below do not
// reach it.
func TestConversationsHaveNoServerRules(t *testing.T) {
	w := newWorld(t)
	until := time.Now().Add(time.Hour)
	w.rules(servers.SendRules{MaxMentions: 1, MaxLinks: 1, MutedUntil: &until})

	_, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID:       w.direct,
		ClientMessageID: "d-1",
		Content:         "https://a.example https://b.example https://c.example",
		Mentions:        []uuid.UUID{w.anna, w.bob},
	})
	if err != nil {
		t.Fatalf("send in a conversation: %v", err)
	}
}

// The server's cap on mentions refuses the whole message instead of cutting the
// list down, and nothing is stored.
func TestMentionCapRefusesTheMessage(t *testing.T) {
	w := newWorld(t)
	w.rules(servers.SendRules{MaxMentions: 2, MaxLinks: 5})

	_, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID:       w.channel,
		ClientMessageID: "c-1",
		Content:         "эй",
		Mentions:        []uuid.UUID{w.bob, w.admin, w.anna},
	})
	var tooMany *TooManyMentionsError
	if !errors.As(err, &tooMany) {
		t.Fatalf("err = %v, want *TooManyMentionsError", err)
	}
	if tooMany.Limit != 2 || !errors.Is(err, ErrTooManyMentions) {
		t.Errorf("err = %v, want the cap of two mentions", err)
	}
	if got := w.stored(); got != 0 {
		t.Errorf("stored %d messages, want none", got)
	}
}

// The same number counts unique accounts: naming one twice is one mention.
func TestMentionCapCountsAccountsOnce(t *testing.T) {
	w := newWorld(t)
	w.rules(servers.SendRules{MaxMentions: 2, MaxLinks: 5})

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID:       w.channel,
		ClientMessageID: "c-1",
		Content:         "эй",
		Mentions:        []uuid.UUID{w.bob, w.bob, w.admin},
	}); err != nil {
		t.Fatalf("two accounts named four times: %v", err)
	}
}

// The cap on links uses the same pattern as the preview fetch and refuses the
// message as a whole.
func TestLinkCapRefusesTheMessage(t *testing.T) {
	w := newWorld(t)
	w.rules(servers.SendRules{MaxMentions: 5, MaxLinks: 2})

	_, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID:       w.channel,
		ClientMessageID: "c-1",
		Content:         "https://a.example https://b.example https://c.example",
	})
	var tooMany *TooManyLinksError
	if !errors.As(err, &tooMany) {
		t.Fatalf("err = %v, want *TooManyLinksError", err)
	}
	if tooMany.Limit != 2 || !errors.Is(err, ErrTooManyLinks) {
		t.Errorf("err = %v, want the cap of two links", err)
	}
	if got := w.stored(); got != 0 {
		t.Errorf("stored %d messages, want none", got)
	}
}

// A zero cap turns the limit off, as the API promises.
func TestZeroCapsTurnTheLimitsOff(t *testing.T) {
	w := newWorld(t)
	w.rules(servers.SendRules{MaxMentions: 0, MaxLinks: 0})

	mentions := make([]uuid.UUID, 0, 30)
	for range 30 {
		mentions = append(mentions, uuid.New())
	}
	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID:       w.channel,
		ClientMessageID: "c-1",
		Content:         "https://a.example https://b.example https://c.example https://d.example",
		Mentions:        mentions,
	}); err != nil {
		t.Fatalf("send with the caps off: %v", err)
	}
}

// manageMessages lifts both caps: a moderator posts announcements with many
// links and can already delete an abusive message; everyone else is held to the
// number the server set.
func TestModeratorsAreNotCapped(t *testing.T) {
	w := newWorld(t)
	w.rules(servers.SendRules{MaxMentions: 1, MaxLinks: 1})

	if _, _, err := w.svc.Send(t.Context(), w.admin, NewMessage{
		ChannelID:       w.channel,
		ClientMessageID: "a-1",
		Content:         "https://a.example https://b.example",
		Mentions:        []uuid.UUID{w.anna, w.bob},
	}); err != nil {
		t.Fatalf("moderator over the caps: %v", err)
	}
}

// The HTTP answer to slowmode is 429 with Retry-After, like the other limits.
func TestHandlerAnswersSlowmodeWithRetryAfter(t *testing.T) {
	w := newWorld(t)
	w.slowmode(120)
	send(t, w, w.anna, "s-1", "первое")
	h := NewHandler(w.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: w.anna}})
	body := api.SendMessageJSONRequestBody{ClientMessageId: "s-2", Content: "второе"}

	response, err := h.SendMessage(ctx, api.SendMessageRequestObject{ChannelId: w.channel, Body: &body})
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
	if code := limited.Body.Error.Code; code != httpx.CodeTooManyRequests {
		t.Errorf("code = %q, want %q", code, httpx.CodeTooManyRequests)
	}
}

// A timeout is a refusal, so the answer is 403 and not 429, and it says when
// the mute ends.
func TestHandlerAnswersMuteWithForbidden(t *testing.T) {
	w := newWorld(t)
	until := time.Now().Add(time.Hour)
	w.rules(servers.SendRules{MutedUntil: &until})
	h := NewHandler(w.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: w.anna}})
	body := api.SendMessageJSONRequestBody{ClientMessageId: "m-1", Content: "привет"}

	response, err := h.SendMessage(ctx, api.SendMessageRequestObject{ChannelId: w.channel, Body: &body})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	forbidden, ok := response.(api.SendMessage403JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 403", response)
	}
	if code := forbidden.Error.Code; code != httpx.CodeForbidden {
		t.Errorf("code = %q, want %q", code, httpx.CodeForbidden)
	}
}

// A message over a cap is a bad request with a code, not a silent truncation.
func TestHandlerAnswersCapsWithBadRequest(t *testing.T) {
	w := newWorld(t)
	w.rules(servers.SendRules{MaxMentions: 1, MaxLinks: 1})
	h := NewHandler(w.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: w.anna}})
	body := api.SendMessageJSONRequestBody{
		ClientMessageId: "c-1",
		Content:         "эй",
		Mentions:        &[]uuid.UUID{w.bob, w.admin},
	}

	response, err := h.SendMessage(ctx, api.SendMessageRequestObject{ChannelId: w.channel, Body: &body})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	bad, ok := response.(api.SendMessage400JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 400", response)
	}
	if code := bad.Error.Code; code != httpx.CodeInvalidRequest {
		t.Errorf("code = %q, want %q", code, httpx.CodeInvalidRequest)
	}
}
