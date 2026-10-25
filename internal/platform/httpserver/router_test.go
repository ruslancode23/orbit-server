package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/feedback"
	"github.com/ruslancode23/orbit-server/internal/files"
	"github.com/ruslancode23/orbit-server/internal/friends"
	"github.com/ruslancode23/orbit-server/internal/health"
	"github.com/ruslancode23/orbit-server/internal/invites"
	"github.com/ruslancode23/orbit-server/internal/linkpreview"
	"github.com/ruslancode23/orbit-server/internal/messages"
	"github.com/ruslancode23/orbit-server/internal/notifysettings"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
	"github.com/ruslancode23/orbit-server/internal/privacy"
	"github.com/ruslancode23/orbit-server/internal/profile"
	"github.com/ruslancode23/orbit-server/internal/push"
	"github.com/ruslancode23/orbit-server/internal/ratelimit"
	"github.com/ruslancode23/orbit-server/internal/reads"
	"github.com/ruslancode23/orbit-server/internal/secrets"
	"github.com/ruslancode23/orbit-server/internal/servers"
	"github.com/ruslancode23/orbit-server/internal/voice"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func newRouter(t *testing.T, dbErr error, adjust ...func(*Deps)) http.Handler {
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
		MaxAvatarBytes: 1 << 15,
		Conversations:  conversations.NewHandler(conversations.NewService(nil, 10, nil), log),
		Secrets:        secrets.NewHandler(secrets.NewService(nil, nil, nil), log),
		Privacy:        privacy.NewHandler(privacy.NewService(nil), log),
		Profiles:       profile.NewHandler(profile.NewService(nil, nil, log), log),
		Friends:        friends.NewHandler(friends.NewService(nil, nil, friends.Limits{}), log),
		Feedback:       feedback.NewHandler(feedback.NewService(nil, nil, 1<<14), log),
		Push:           push.NewHandler(push.NewService(nil, nil, nil, "", log), log),
		NotificationSettings: notifysettings.NewHandler(
			notifysettings.NewService(nil, nil, nil), log),
		LinkPreviews: linkpreview.NewHandler(
			linkpreview.NewService(nil, nil, false), log),
		MaxScreenshotBytes: 1 << 14,
		Invites: invites.NewHandler(
			invites.NewService(nil, nil, nil), invites.NewCookies(auth.NewHandler(sessions, false, log)), log),
		Voice: voice.NewHandler(
			voice.NewTokenIssuer("devkey", "a-secret-that-is-long-enough-for-hs256", "ws://lk"), nil, log),
	}
	for _, change := range adjust {
		change(&d)
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
	avatarPath := "/api/v1/users/me/avatar"

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

	t.Run("avatar over the limit", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, avatarPath, strings.NewReader(""))
		req.ContentLength = 1<<15 + multipartSlack + 1
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

// bugReportForm is a multipart bug report with the given text fields.
func bugReportForm(t *testing.T, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for name, value := range fields {
		if err := form.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, form.FormDataContentType()
}

// The bug report form passes the schema as multipart and reaches the handler,
// which wants a session; what the schema refuses never gets that far.
func TestBugReportRoute(t *testing.T) {
	router := newRouter(t, nil)
	valid := map[string]string{"description": "Кнопка не нажимается", "appVersion": "0.1.0", "route": "/s/1/2"}
	tests := []struct {
		name   string
		change func(map[string]string)
		status int
	}{
		{"a valid form needs a session", func(map[string]string) {}, http.StatusUnauthorized},
		{"description is required", func(f map[string]string) { delete(f, "description") }, http.StatusBadRequest},
		{"description over 4000 characters", func(f map[string]string) { f["description"] = strings.Repeat("я", 4001) }, http.StatusBadRequest},
		{"route must be a path", func(f map[string]string) { f["route"] = "https://evil.test/" }, http.StatusBadRequest},
		{"unknown field", func(f map[string]string) { f["token"] = "secret" }, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := map[string]string{}
			for k, v := range valid {
				fields[k] = v
			}
			tt.change(fields)
			body, contentType := bugReportForm(t, fields)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/bug-reports", body)
			req.Header.Set("Content-Type", contentType)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
			}
		})
	}

	t.Run("a screenshot passes the schema as a file", func(t *testing.T) {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		for name, value := range valid {
			if err := form.WriteField(name, value); err != nil {
				t.Fatal(err)
			}
		}
		file, err := form.CreateFormFile("screenshot", "shot.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("\x89PNG\r\n\x1a\n binary \x00\xff")); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/bug-reports", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 (the schema refused the file?); body %s", rec.Code, rec.Body)
		}
	})

	t.Run("over the screenshot limit and the text room", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/bug-reports", strings.NewReader(""))
		req.ContentLength = 1<<14 + bugReportTextBytes + 1
		req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", rec.Code)
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

// The identity routes are part of the contract too: without a session they must
// answer 401, which proves the validator accepted the body and the handler ran.
func TestIdentityRoutesExist(t *testing.T) {
	router := newRouter(t, nil)
	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPatch, "/api/v1/users/me/nickname", `{"nickname":"Bob","currentPassword":"secret"}`},
		{http.MethodPatch, "/api/v1/users/me/email", `{"email":"bob@example.com","currentPassword":"secret"}`},
		{http.MethodDelete, "/api/v1/users/me/avatar", ""},
	}
	for _, route := range routes {
		rec := do(t, router, route.method, route.path, route.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 (route missing?), body %s", route.method, route.path, rec.Code, rec.Body)
		}
	}
}

// A refused sign-in reaches the browser with its wait: the web client lives on
// another origin, so Retry-After must be exposed through CORS or the page could
// only say "later".
func TestTooManyRequestsExposesRetryAfter(t *testing.T) {
	failures := ratelimit.NewHourly(1)
	failures.AllowKey("anna") // the one wrong password this hour is spent
	h := newRouter(t, nil, func(d *Deps) {
		d.CORSOrigins = []string{"http://web.test"}
		d.Auth.SetLimits(auth.Limits{LoginFailures: failures})
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"login":"Anna","password":"whatever-it-is"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://web.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header")
	}
	if exposed := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(exposed, "Retry-After") {
		t.Errorf("Access-Control-Expose-Headers = %q, want Retry-After in it", exposed)
	}
}
