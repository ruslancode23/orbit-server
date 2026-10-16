// Package health serves liveness and readiness probes.
package health

import (
	"context"
	"time"

	"github.com/ruslancode23/orbit-server/internal/api"
)

// Pinger is satisfied by *pgxpool.Pool.
type Pinger interface {
	Ping(ctx context.Context) error
}

type Handler struct {
	db Pinger
}

func NewHandler(db Pinger) *Handler {
	return &Handler{db: db}
}

// GetLiveness reports that the process is running.
func (h *Handler) GetLiveness(context.Context, api.GetLivenessRequestObject) (api.GetLivenessResponseObject, error) {
	return api.GetLiveness200JSONResponse{Status: api.LivenessStatusOk}, nil
}

// GetReadiness reports whether the instance can serve traffic: the database must respond.
func (h *Handler) GetReadiness(ctx context.Context, _ api.GetReadinessRequestObject) (api.GetReadinessResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		return api.GetReadiness503JSONResponse{Status: api.ReadinessStatusUnavailable, Database: api.Down}, nil
	}
	return api.GetReadiness200JSONResponse{Status: api.ReadinessStatusOk, Database: api.Up}, nil
}
