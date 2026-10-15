package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type pingerFunc func(ctx context.Context) error

func (f pingerFunc) Ping(ctx context.Context) error { return f(ctx) }

func TestReady(t *testing.T) {
	tests := []struct {
		name   string
		ping   error
		status int
	}{
		{name: "database up", ping: nil, status: http.StatusOK},
		{name: "database down", ping: errors.New("connection refused"), status: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler(pingerFunc(func(context.Context) error { return tt.ping }))
			rec := httptest.NewRecorder()

			h.Ready(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))

			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
		})
	}
}
