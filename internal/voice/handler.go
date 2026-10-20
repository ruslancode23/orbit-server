package voice

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

// Access answers whether the caller may speak in a channel, after checking
// that the channel exists, is a voice channel and belongs to their server.
type Access interface {
	VoiceAccess(ctx context.Context, channelID, callerID uuid.UUID) (bool, error)
}

type Handler struct {
	issuer *TokenIssuer
	access Access
	log    *slog.Logger
}

func NewHandler(issuer *TokenIssuer, access Access, log *slog.Logger) *Handler {
	return &Handler{issuer: issuer, access: access, log: log}
}

// CreateChannelVoiceToken handles POST /api/v1/channels/{channelId}/voice-token.
// A member of the server always gets a token; only the right to publish follows
// from the channel's role rules, so a listener can join without a microphone.
func (h *Handler) CreateChannelVoiceToken(ctx context.Context, req api.CreateChannelVoiceTokenRequestObject) (api.CreateChannelVoiceTokenResponseObject, error) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return api.CreateChannelVoiceToken401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	canSpeak, err := h.access.VoiceAccess(ctx, req.ChannelId, session.User.ID)
	switch {
	case errors.Is(err, channels.ErrNotFound), errors.Is(err, channels.ErrForbidden):
		return api.CreateChannelVoiceToken404JSONResponse(api.Error{Error: notFound()}), nil
	case err != nil:
		h.log.ErrorContext(ctx, "check voice access", slog.Any("error", err))
		return api.CreateChannelVoiceToken500JSONResponse{Error: api.ErrorDetail{
			Code: httpx.CodeInternal, Message: "could not issue token",
		}}, nil
	}

	token, err := h.issuer.Issue(req.ChannelId.String(), session.User.ID.String(), session.User.Nickname, canSpeak)
	if err != nil {
		if IsValidationError(err) {
			return api.CreateChannelVoiceToken500JSONResponse{Error: api.ErrorDetail{
				Code: httpx.CodeInternal, Message: err.Error(),
			}}, nil
		}
		h.log.ErrorContext(ctx, "issue voice token", slog.Any("error", err))
		return api.CreateChannelVoiceToken500JSONResponse{Error: api.ErrorDetail{
			Code: httpx.CodeInternal, Message: "could not issue token",
		}}, nil
	}

	return api.CreateChannelVoiceToken201JSONResponse{
		Token:     token.Value,
		Url:       token.URL,
		Identity:  token.Identity,
		ExpiresAt: token.ExpiresAt,
	}, nil
}

func unauthorized() api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{
		Code: httpx.CodeUnauthorized, Message: "sign in first",
	}}
}

func notFound() api.ErrorDetail {
	return api.ErrorDetail{Code: httpx.CodeNotFound, Message: "voice channel not found"}
}
