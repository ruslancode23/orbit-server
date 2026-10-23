package push

import (
	"context"
	"errors"
	"log/slog"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) GetPushConfig(ctx context.Context, _ api.GetPushConfigRequestObject) (api.GetPushConfigResponseObject, error) {
	if _, ok := auth.SessionFrom(ctx); !ok {
		return api.GetPushConfig401JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeUnauthorized, "sign in first")}, nil
	}
	config := api.PushConfig{Enabled: h.svc.Enabled()}
	if key := h.svc.PublicKey(); key != "" {
		config.PublicKey = &key
	}
	return api.GetPushConfig200JSONResponse(config), nil
}

func (h *Handler) CreatePushSubscription(ctx context.Context, req api.CreatePushSubscriptionRequestObject) (api.CreatePushSubscriptionResponseObject, error) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return api.CreatePushSubscription401JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeUnauthorized, "sign in first")}, nil
	}
	if req.Body == nil {
		return api.CreatePushSubscription400JSONResponse(api.Error(errorBody(httpx.CodeInvalidRequest, "send the browser's push subscription"))), nil
	}
	sub, err := h.svc.Subscribe(ctx, session.User.ID, req.Body.Endpoint, req.Body.Keys.P256dh, req.Body.Keys.Auth)
	switch {
	case errors.Is(err, ErrDisabled):
		return api.CreatePushSubscription503JSONResponse(api.Error(errorBody("push_disabled", "push notifications are off on this server"))), nil
	case errors.Is(err, ErrInvalidEndpoint):
		return api.CreatePushSubscription400JSONResponse(api.Error(errorBody(httpx.CodeInvalidRequest, "endpoint: not a known push service"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "create push subscription", slog.Any("error", err))
		return api.CreatePushSubscription500JSONResponse(api.Error(errorBody(httpx.CodeInternal, "could not save the subscription"))), nil
	}
	return api.CreatePushSubscription201JSONResponse{Id: sub.ID}, nil
}

func (h *Handler) DeletePushSubscription(ctx context.Context, req api.DeletePushSubscriptionRequestObject) (api.DeletePushSubscriptionResponseObject, error) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return api.DeletePushSubscription401JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeUnauthorized, "sign in first")}, nil
	}
	err := h.svc.Unsubscribe(ctx, session.User.ID, req.SubscriptionId)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.DeletePushSubscription404JSONResponse(api.Error(errorBody(httpx.CodeNotFound, "no such subscription"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "delete push subscription", slog.Any("error", err))
		return api.DeletePushSubscription500JSONResponse(api.Error(errorBody(httpx.CodeInternal, "could not remove the subscription"))), nil
	}
	return api.DeletePushSubscription204Response{}, nil
}

func errorBody(code, message string) api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{Code: code, Message: message}}
}
