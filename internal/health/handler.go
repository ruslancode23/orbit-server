// Package health serves liveness and readiness probes.
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
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

// Live reports that the process is running.
func (h *Handler) Live(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Ready reports whether the instance can serve traffic: the database must respond.
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "database": "down"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "database": "up"})
}
