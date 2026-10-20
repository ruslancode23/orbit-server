package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/files"
	"github.com/ruslancode23/orbit-server/internal/friends"
	"github.com/ruslancode23/orbit-server/internal/health"
	"github.com/ruslancode23/orbit-server/internal/invites"
	"github.com/ruslancode23/orbit-server/internal/messages"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
	"github.com/ruslancode23/orbit-server/internal/privacy"
	"github.com/ruslancode23/orbit-server/internal/reads"
	"github.com/ruslancode23/orbit-server/internal/secrets"
	"github.com/ruslancode23/orbit-server/internal/servers"
	"github.com/ruslancode23/orbit-server/internal/voice"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func newRouter(t *testing.T, dbErr error) http.Handler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The auth store is never reached here: these tests send no session cookie.
	sessions := auth.NewService(nil, time.Hour)
	d := Deps{
		Logger:   log,
		Health:   health.NewHandler(pinger{dbErr}),
		Auth:     auth.NewHandler(sessions, false, log),
		Sessions: sessions,
		// Reached only with a session, and these tests send no cookie.
		Workspaces: servers.NewHandler(servers.NewService(nil, nil, servers.Icons{}), log),
		Channels:   channels.NewHandler(channels.NewService(nil, nil), log),
		Messages:   messages.NewHandler(messages.NewService(nil, nil, nil, nil, nil), log),
		Reads:      reads.NewHandler(reads.NewService(nil, nil, nil, nil), log),
		Files: files.NewHandler(
			files.NewService(nil, nil, nil, nil, nil, 1<<20, time.Minute), log),
		MaxUploadBytes: 1 << 20,
		MaxIconBytes:   1 << 16,
		Conversations:  conversations.NewHandler(conversations.NewService(nil, 10, nil), log),
		Secrets:        secrets.NewHandler(secrets.NewService(nil, nil, nil), log),
		Privacy:        privacy.NewHandler(privacy.NewService(nil), log),
		Friends:        friends.NewHandler(friends.NewService(nil, nil, friends.Limits{}), log),
		Invites: invites.NewHandler(
			invites.NewService(nil, nil, nil), invites.NewCookies(auth.NewHandler(sessions, false, log)), log),
		Voice: voice.NewHandler(
			voice.NewTokenIssuer("devkey", "a-secret-that-is-long-enough-for-hs256", "ws://lk"), nil, log),
	}
	h, err := NewRouter(d)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	return h
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body httpx.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

func TestChannelVoiceToken(t *testing.T) {
	const channelID = "11111111-1111-4111-8111-111111111111"
	tests := []struct {
		name   string
		path   string
		status int
		code   string
	}{
		{"needs a session", "/api/v1/channels/" + channelID + "/voice-token", http.StatusUnauthorized, httpx.CodeUnauthorized},
		{"checks the channel id", "/api/v1/channels/not-a-uuid/voice-token", http.StatusBadRequest, httpx.CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, newRouter(t, nil), http.MethodPost, tt.path, "")

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
			}
			if got := errorCode(t, rec); got != tt.code {
				t.Errorf("code = %q, want %q", got, tt.code)
			}
		})
	}
}

func TestProbes(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		dbErr  error
		status int
		body   string
	}{
		{"liveness", "/healthz", nil, http.StatusOK, `{"status":"ok"}`},
		{"ready", "/readyz", nil, http.StatusOK, `{"database":"up","status":"ok"}`},
		{"database down", "/readyz", errors.New("refused"), http.StatusServiceUnavailable, `{"database":"down","status":"unavailable"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, newRouter(t, tt.dbErr), http.MethodGet, tt.path, "")
			if rec.Code != tt.status || strings.TrimSpace(rec.Body.String()) != tt.body {
				t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body, tt.status, tt.body)
			}
		})
	}
}

func TestUnknownRoute(t *testing.T) {
	rec := do(t, newRouter(t, nil), http.MethodGet, "/api/v1/nope", "")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != httpx.CodeNotFound {
		t.Errorf("got %d %s, want 404 not_found", rec.Code, rec.Body)
	}
}

// Each upload has its own cap, checked before a byte is read, and a JSON body
// with a long description still fits the general one.
func TestBodyLimits(t *testing.T) {
	h := newRouter(t, nil)
	iconPath := "/api/v1/workspaces/7d1f3c56-9f8e-4b52-9d0c-6f2f4d1e0a11/icon"

	t.Run("icon over the limit", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, iconPath, strings.NewReader(""))
		req.ContentLength = 1<<16 + multipartSlack + 1
		req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", rec.Code)
		}
	})

	t.Run("a long description is not too large", func(t *testing.T) {
		body := `{"name":"Команда","description":"` + strings.Repeat("я", 500) + `"}`
		rec := do(t, h, http.MethodPost, "/api/v1/workspaces", body)
		// No session: the request got past the body cap and the schema.
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401; body %s", rec.Code, rec.Body)
		}
	})

	t.Run("a description over 500 characters", func(t *testing.T) {
		body := `{"name":"Команда","description":"` + strings.Repeat("я", 501) + `"}`
		rec := do(t, h, http.MethodPost, "/api/v1/workspaces", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400; body %s", rec.Code, rec.Body)
		}
	})
}

// The friend routes are part of the generated contract; a missing /accept would
// otherwise only show up as a 404 in a client. Without a session every one of
// them answers 401, which proves the path matched and the handler ran.
func TestFriendRoutesExist(t *testing.T) {
	const userID = "11111111-1111-4111-8111-111111111111"
	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/friends", ""},
		{http.MethodDelete, "/api/v1/friends/" + userID, ""},
		{http.MethodPost, "/api/v1/friends/requests", `{"nickname":"Anna"}`},
		{http.MethodGet, "/api/v1/friends/requests?direction=incoming", ""},
		{http.MethodPost, "/api/v1/friends/requests/" + userID + "/accept", ""},
		{http.MethodDelete, "/api/v1/friends/requests/" + userID, ""},
		{http.MethodGet, "/api/v1/users/me/friend-request-policy", ""},
		{http.MethodPut, "/api/v1/users/me/friend-request-policy", `{"friendRequestPolicy":"all"}`},
	}
	router := newRouter(t, nil)
	for _, route := range routes {
		rec := do(t, router, route.method, route.path, route.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 (route missing?), body %s", route.method, route.path, rec.Code, rec.Body)
			continue
		}
		if got := errorCode(t, rec); got != httpx.CodeUnauthorized {
			t.Errorf("%s %s code = %q, want %q", route.method, route.path, got, httpx.CodeUnauthorized)
		}
	}
}
