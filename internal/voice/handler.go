package voice

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

const maxRequestBytes = 1 << 10

type Handler struct {
	issuer *TokenIssuer
	log    *slog.Logger
}

func NewHandler(issuer *TokenIssuer, log *slog.Logger) *Handler {
	return &Handler{issuer: issuer, log: log}
}

type createTokenRequest struct {
	Room        string `json:"room"`
	DisplayName string `json:"displayName"`
}

type createTokenResponse struct {
	Token     string    `json:"token"`
	URL       string    `json:"url"`
	Identity  string    `json:"identity"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// CreateToken handles POST /api/v1/voice/token.
//
// Development only: anyone who can reach the endpoint may join any room.
// It is replaced by session-based authorization together with accounts.
func (h *Handler) CreateToken(w http.ResponseWriter, r *http.Request) {
	var req createTokenRequest
	if !httpx.DecodeJSON(w, r, &req, maxRequestBytes) {
		return
	}

	token, err := h.issuer.Issue(req.Room, req.DisplayName)
	if err != nil {
		if IsValidationError(err) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, err.Error())
			return
		}
		h.log.ErrorContext(r.Context(), "issue voice token", slog.Any("error", err))
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not issue token")
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, createTokenResponse{
		Token:     token.Value,
		URL:       token.URL,
		Identity:  token.Identity,
		ExpiresAt: token.ExpiresAt,
	})
}
