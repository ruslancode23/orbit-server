package voice

import (
	"context"
	"log/slog"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

type Handler struct {
	issuer *TokenIssuer
	log    *slog.Logger
}

func NewHandler(issuer *TokenIssuer, log *slog.Logger) *Handler {
	return &Handler{issuer: issuer, log: log}
}

// CreateVoiceToken handles POST /api/v1/voice/token. The request has already
// been validated against api/openapi.yaml; Issue validates again as the domain rule.
//
// Development only: anyone who can reach the endpoint may join any room.
// It is replaced by session-based authorization together with accounts.
func (h *Handler) CreateVoiceToken(ctx context.Context, req api.CreateVoiceTokenRequestObject) (api.CreateVoiceTokenResponseObject, error) {
	if req.Body == nil {
		return badRequest("request body is required"), nil
	}

	token, err := h.issuer.Issue(req.Body.Room, req.Body.DisplayName)
	if err != nil {
		if IsValidationError(err) {
			return badRequest(err.Error()), nil
		}
		h.log.ErrorContext(ctx, "issue voice token", slog.Any("error", err))
		return api.CreateVoiceToken500JSONResponse{
			Error: api.ErrorDetail{Code: httpx.CodeInternal, Message: "could not issue token"},
		}, nil
	}

	return api.CreateVoiceToken201JSONResponse{
		Token:     token.Value,
		Url:       token.URL,
		Identity:  token.Identity,
		ExpiresAt: token.ExpiresAt,
	}, nil
}

func badRequest(message string) api.CreateVoiceToken400JSONResponse {
	return api.CreateVoiceToken400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{
		Error: api.ErrorDetail{Code: httpx.CodeInvalidRequest, Message: message},
	}}
}
