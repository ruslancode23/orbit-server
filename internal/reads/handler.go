package reads

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

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

func caller(ctx context.Context) (uuid.UUID, bool) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return uuid.Nil, false
	}
	return session.User.ID, true
}

func (h *Handler) ListUnread(ctx context.Context, req api.ListUnreadRequestObject) (api.ListUnreadResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListUnread401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	list, err := h.svc.Workspace(ctx, req.WorkspaceId, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.ListUnread404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "list unread", slog.Any("error", err))
		return api.ListUnread500JSONResponse(api.Error(internal("could not count the unread messages"))), nil
	}

	out := make(api.ListUnread200JSONResponse, 0, len(list))
	for _, state := range list {
		out = append(out, toAPIUnread(state))
	}
	return out, nil
}

func (h *Handler) MarkChannelRead(ctx context.Context, req api.MarkChannelReadRequestObject) (api.MarkChannelReadResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.MarkChannelRead401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.MarkChannelRead400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	state, err := h.svc.Mark(ctx, req.ChannelId, userID, req.Body.Seq)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.MarkChannelRead404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "mark channel read", slog.Any("error", err))
		return api.MarkChannelRead500JSONResponse(api.Error(internal("could not move the read cursor"))), nil
	}
	return api.MarkChannelRead200JSONResponse(toAPIUnread(state)), nil
}

func toAPIUnread(state Unread) api.ChannelUnread {
	return api.ChannelUnread{
		ChannelId:   state.ChannelID,
		LastReadSeq: state.LastReadSeq,
		Unread:      state.Unread,
		Mentions:    state.Mentions,
	}
}

func unauthorized() api.ErrorJSONResponse {
	return errorBody(httpx.CodeUnauthorized, "sign in first")
}

func notFound() api.ErrorJSONResponse {
	return errorBody(httpx.CodeNotFound, "not found")
}

func badRequest(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeInvalidRequest, message)
}

func internal(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeInternal, message)
}

func errorBody(code, message string) api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{Code: code, Message: message}}
}
