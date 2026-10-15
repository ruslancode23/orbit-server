package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ruslancode23/orbit-server/internal/health"
	"github.com/ruslancode23/orbit-server/internal/voice"
)

type okPinger struct{}

func (okPinger) Ping(context.Context) error { return nil }

func TestVoiceTokenRouteOnlyWhenEnabled(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	base := Deps{Logger: log, Health: health.NewHandler(okPinger{})}

	withVoice := base
	withVoice.Voice = voice.NewHandler(voice.NewTokenIssuer("devkey", "a-secret-that-is-long-enough-for-hs256", "ws://lk"), log)

	tests := []struct {
		name   string
		deps   Deps
		status int
	}{
		{"production: no route", base, http.StatusNotFound},
		{"development: token issued", withVoice, http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/voice/token",
				strings.NewReader(`{"room":"dev-room","displayName":"Anna"}`))
			NewRouter(tt.deps).ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
		})
	}
}
