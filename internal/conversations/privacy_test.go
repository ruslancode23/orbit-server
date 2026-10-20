package conversations

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

// Whatever reason the privacy rules give, the caller sees one answer and no
// conversation is written.
func TestStartIsRefusedWhenThePrivacyRulesSayNo(t *testing.T) {
	tests := []struct {
		name  string
		rules *fakePrivacy
		with  func(w world) []uuid.UUID
	}{
		{
			name:  "a direct dialogue",
			rules: &fakePrivacy{allowDirect: func(uuid.UUID, uuid.UUID) bool { return false }},
			with:  func(w world) []uuid.UUID { return []uuid.UUID{w.bob} },
		},
		{
			name:  "a group",
			rules: &fakePrivacy{allowGroup: func(uuid.UUID, []uuid.UUID) bool { return false }},
			with:  func(w world) []uuid.UUID { return []uuid.UUID{w.bob, w.vika} },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.svc = NewService(w.store, maxMembers, tt.rules)

			if _, err := w.svc.Start(t.Context(), w.anna, tt.with(w)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Start: err = %v, want ErrNotFound", err)
			}
			if w.store.creates != 0 {
				t.Errorf("created %d conversations, want none", w.store.creates)
			}
		})
	}
}

// Opening a new dialogue spends a token; reopening the one a pair already has
// does not, and the cap is reported with the time to wait.
func TestNewConversationsAreRateLimited(t *testing.T) {
	w := newWorld(t)
	w.svc.SetLimiter(ratelimit.New(1))

	direct, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := w.svc.Start(t.Context(), w.bob, []uuid.UUID{w.anna}); err != nil {
		t.Fatalf("reopening the dialogue: %v", err)
	}
	if direct.ID == uuid.Nil {
		t.Fatal("the first conversation has no id")
	}

	_, err = w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob, w.vika})
	var limited *RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("Start over the limit: err = %v, want *RateLimitedError", err)
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want it to wrap ErrRateLimited", err)
	}
	if limited.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want a positive wait", limited.RetryAfter)
	}
	if w.store.creates != 1 {
		t.Errorf("created %d conversations, want only the first", w.store.creates)
	}
}

// The HTTP answer is 429 with Retry-After, not a generic 500.
func TestHandlerAnswers429WithRetryAfter(t *testing.T) {
	w := newWorld(t)
	w.svc.SetLimiter(ratelimit.New(1))
	h := NewHandler(w.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: w.anna}})

	body := api.StartConversationJSONRequestBody{UserIds: []uuid.UUID{w.bob}}
	first, err := h.StartConversation(ctx, api.StartConversationRequestObject{Body: &body})
	if err != nil {
		t.Fatalf("StartConversation: %v", err)
	}
	if _, ok := first.(api.StartConversation201JSONResponse); !ok {
		t.Fatalf("first response = %T, want 201", first)
	}

	group := api.StartConversationJSONRequestBody{UserIds: []uuid.UUID{w.bob, w.vika}}
	response, err := h.StartConversation(ctx, api.StartConversationRequestObject{Body: &group})
	if err != nil {
		t.Fatalf("StartConversation: %v", err)
	}
	limited, ok := response.(api.StartConversation429JSONResponse)
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
