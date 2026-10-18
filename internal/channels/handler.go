package channels

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

// Handler serves the generated channel operations; every rule lives in Service.
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

func (h *Handler) ListChannels(ctx context.Context, req api.ListChannelsRequestObject) (api.ListChannelsResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListChannels401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	list, err := h.svc.List(ctx, req.WorkspaceId, userID)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return api.ListChannels404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "list channels", slog.Any("error", err))
		return api.ListChannels500JSONResponse(internal("could not list the channels")), nil
	}

	out := make(api.ListChannels200JSONResponse, 0, len(list))
	for _, v := range list {
		out = append(out, toAPIChannel(v))
	}
	return out, nil
}

func (h *Handler) CreateChannel(ctx context.Context, req api.CreateChannelRequestObject) (api.CreateChannelResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.CreateChannel401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.CreateChannel400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	view, err := h.svc.Create(ctx, req.WorkspaceId, userID, NewChannel{
		Name:       req.Body.Name,
		Topic:      value(req.Body.Topic),
		IsReadOnly: value(req.Body.IsReadOnly),
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return api.CreateChannel404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrForbidden):
		return api.CreateChannel403JSONResponse(api.Error(forbidden("creating a channel needs the admin role"))), nil
	case errors.Is(err, ErrNameTaken):
		return api.CreateChannel409JSONResponse(api.Error(conflict(err.Error()))), nil
	case errors.Is(err, ErrInvalidName), errors.Is(err, ErrInvalidTopic):
		return api.CreateChannel400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "create channel", slog.Any("error", err))
		return api.CreateChannel500JSONResponse(internal("could not create the channel")), nil
	}
	return api.CreateChannel201JSONResponse(toAPIChannel(view)), nil
}

func (h *Handler) GetChannel(ctx context.Context, req api.GetChannelRequestObject) (api.GetChannelResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.GetChannel401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	view, err := h.svc.Get(ctx, req.ChannelId, userID)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return api.GetChannel404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "get channel", slog.Any("error", err))
		return api.GetChannel500JSONResponse(internal("could not read the channel")), nil
	}
	return api.GetChannel200JSONResponse(toAPIChannel(view)), nil
}

func (h *Handler) UpdateChannel(ctx context.Context, req api.UpdateChannelRequestObject) (api.UpdateChannelResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.UpdateChannel401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.UpdateChannel400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	view, err := h.svc.Update(ctx, req.ChannelId, userID, Update{
		Name:       req.Body.Name,
		Topic:      req.Body.Topic,
		IsReadOnly: req.Body.IsReadOnly,
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return api.UpdateChannel404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrForbidden):
		return api.UpdateChannel403JSONResponse(api.Error(forbidden("changing a channel needs the admin role"))), nil
	case errors.Is(err, ErrNameTaken):
		return api.UpdateChannel409JSONResponse(api.Error(conflict(err.Error()))), nil
	case errors.Is(err, ErrInvalidName), errors.Is(err, ErrInvalidTopic):
		return api.UpdateChannel400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "update channel", slog.Any("error", err))
		return api.UpdateChannel500JSONResponse(internal("could not change the channel")), nil
	}
	return api.UpdateChannel200JSONResponse(toAPIChannel(view)), nil
}

func (h *Handler) DeleteChannel(ctx context.Context, req api.DeleteChannelRequestObject) (api.DeleteChannelResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.DeleteChannel401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	err := h.svc.Delete(ctx, req.ChannelId, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.DeleteChannel404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrForbidden):
		return api.DeleteChannel403JSONResponse(api.Error(forbidden("deleting a channel needs the admin role"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "delete channel", slog.Any("error", err))
		return api.DeleteChannel500JSONResponse(internal("could not delete the channel")), nil
	}
	return api.DeleteChannel204Response{}, nil
}

func (h *Handler) ReorderChannels(ctx context.Context, req api.ReorderChannelsRequestObject) (api.ReorderChannelsResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ReorderChannels401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.ReorderChannels400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	list, err := h.svc.Reorder(ctx, req.WorkspaceId, userID, req.Body.ChannelIds)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.ReorderChannels404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrForbidden):
		return api.ReorderChannels403JSONResponse(api.Error(forbidden("reordering channels needs the admin role"))), nil
	case errors.Is(err, ErrIncompleteOrder):
		return api.ReorderChannels400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "reorder channels", slog.Any("error", err))
		return api.ReorderChannels500JSONResponse(internal("could not reorder the channels")), nil
	}

	out := make(api.ReorderChannels200JSONResponse, 0, len(list))
	for _, v := range list {
		out = append(out, toAPIChannel(v))
	}
	return out, nil
}

func toAPIChannel(v View) api.Channel {
	channel := api.Channel{
		Id:          v.Channel.ID,
		WorkspaceId: v.Channel.WorkspaceID,
		Name:        v.Channel.Name,
		Position:    v.Channel.Position,
		IsReadOnly:  v.Channel.IsReadOnly,
		CanWrite:    v.CanWrite,
		CreatedAt:   v.Channel.CreatedAt,
	}
	if v.Channel.Topic != "" {
		topic := v.Channel.Topic
		channel.Topic = &topic
	}
	return channel
}

func value[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func unauthorized() api.ErrorJSONResponse {
	return errorBody(httpx.CodeUnauthorized, "sign in first")
}

// notFound answers both "no such channel" and "not your server".
func notFound() api.ErrorJSONResponse {
	return errorBody(httpx.CodeNotFound, "not found")
}

func forbidden(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeForbidden, message)
}

func conflict(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeConflict, message)
}

func badRequest(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeInvalidRequest, message)
}

func internal(message string) api.Error {
	return api.Error(errorBody(httpx.CodeInternal, message))
}

func errorBody(code, message string) api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{Code: code, Message: message}}
}
