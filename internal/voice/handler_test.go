package voice

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

func postToken(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(NewTokenIssuer(testKey, testSecret, testURL), slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/voice/token", strings.NewReader(body))
	h.CreateToken(rec, req)
	return rec
}

func TestCreateToken(t *testing.T) {
	rec := postToken(t, `{"room":"dev-room","displayName":"Anna"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	var resp createTokenResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.URL != testURL || resp.Identity == "" || resp.ExpiresAt.IsZero() {
		t.Errorf("incomplete response: %+v", resp)
	}
	if g := verify(t, resp.Token); g.Video.Room != "dev-room" {
		t.Errorf("room = %q, want dev-room", g.Video.Room)
	}
}

func TestCreateTokenRejectsBadInput(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
	}{
		{"invalid room", `{"room":"../admin","displayName":"Anna"}`, http.StatusBadRequest},
		{"missing name", `{"room":"dev-room"}`, http.StatusBadRequest},
		{"unknown field", `{"room":"dev-room","displayName":"Anna","canPublish":false}`, http.StatusBadRequest},
		{"not json", `room=dev-room`, http.StatusBadRequest},
		{"too large", `{"room":"dev-room","displayName":"` + strings.Repeat("a", 2000) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postToken(t, tt.body)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
			}
			var body httpx.ErrorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil || body.Error.Code == "" {
				t.Errorf("error body = %s, want {error:{code,message}}", rec.Body)
			}
		})
	}
}
