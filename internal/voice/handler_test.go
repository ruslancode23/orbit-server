package voice

import (
	"io"
	"log/slog"
	"testing"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

func newTestHandler() *Handler {
	return NewHandler(NewTokenIssuer(testKey, testSecret, testURL), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestCreateToken(t *testing.T) {
	resp, err := newTestHandler().CreateVoiceToken(t.Context(), api.CreateVoiceTokenRequestObject{
		Body: &api.VoiceTokenRequest{Room: "dev-room", DisplayName: "Anna"},
	})
	if err != nil {
		t.Fatalf("CreateVoiceToken() error = %v", err)
	}

	created, ok := resp.(api.CreateVoiceToken201JSONResponse)
	if !ok {
		t.Fatalf("response = %#v, want 201", resp)
	}
	if created.Url != testURL || created.Identity == "" || created.ExpiresAt.IsZero() {
		t.Errorf("incomplete response: %+v", created)
	}
	if g := verify(t, created.Token); g.Video.Room != "dev-room" {
		t.Errorf("room = %q, want dev-room", g.Video.Room)
	}
}

// The OpenAPI schema cannot express every rule (control characters), so the
// domain validation still answers 400 on its own.
func TestCreateTokenDomainValidation(t *testing.T) {
	resp, err := newTestHandler().CreateVoiceToken(t.Context(), api.CreateVoiceTokenRequestObject{
		Body: &api.VoiceTokenRequest{Room: "dev-room", DisplayName: "An\u0000na"},
	})
	if err != nil {
		t.Fatalf("CreateVoiceToken() error = %v", err)
	}

	bad, ok := resp.(api.CreateVoiceToken400JSONResponse)
	if !ok {
		t.Fatalf("response = %#v, want 400", resp)
	}
	if bad.Error.Code != httpx.CodeInvalidRequest {
		t.Errorf("code = %q, want %q", bad.Error.Code, httpx.CodeInvalidRequest)
	}
}
