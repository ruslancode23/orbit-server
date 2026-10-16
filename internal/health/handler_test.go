package health

import (
	"context"
	"errors"
	"testing"

	"github.com/ruslancode23/orbit-server/internal/api"
)

type pingerFunc func(ctx context.Context) error

func (f pingerFunc) Ping(ctx context.Context) error { return f(ctx) }

func TestGetReadiness(t *testing.T) {
	tests := []struct {
		name string
		ping error
		want api.GetReadinessResponseObject
	}{
		{
			name: "database up",
			want: api.GetReadiness200JSONResponse{Status: api.ReadinessStatusOk, Database: api.Up},
		},
		{
			name: "database down",
			ping: errors.New("connection refused"),
			want: api.GetReadiness503JSONResponse{Status: api.ReadinessStatusUnavailable, Database: api.Down},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler(pingerFunc(func(context.Context) error { return tt.ping }))

			got, err := h.GetReadiness(t.Context(), api.GetReadinessRequestObject{})
			if err != nil {
				t.Fatalf("GetReadiness() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("GetReadiness() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
