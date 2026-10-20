package conversations

import (
	"context"
	"errors"
	"log/slog"
	"time"

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

func (h *Handler) ListConversations(ctx context.Context, _ api.ListConversationsRequestObject) (api.ListConversationsResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListConversations401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	list, err := h.svc.List(ctx, userID)
	if err != nil {
		h.log.ErrorContext(ctx, "list conversations", slog.Any("error", err))
		return api.ListConversations500JSONResponse(api.Error(internal("could not list the conversations"))), nil
	}

	out := make(api.ListConversations200JSONResponse, 0, len(list))
	for _, conversation := range list {
		out = append(out, toAPIConversation(conversation))
	}
	return out, nil
}

func (h *Handler) StartConversation(ctx context.Context, req api.StartConversationRequestObject) (api.StartConversationResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.StartConversation401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.StartConversation400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	before, err := h.svc.List(ctx, userID)
	if err != nil {
		h.log.ErrorContext(ctx, "list conversations", slog.Any("error", err))
		return api.StartConversation500JSONResponse(api.Error(internal("could not open the conversation"))), nil
	}

	conversation, err := h.svc.Start(ctx, userID, req.Body.UserIds)
	var limited *RateLimitedError
	switch {
	case errors.As(err, &limited):
		seconds := int((limited.RetryAfter + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		return api.StartConversation429JSONResponse{TooManyRequestsJSONResponse: api.TooManyRequestsJSONResponse{
			Body:    api.Error(errorBody(httpx.CodeTooManyRequests, "too many new conversations, try again later")),
			Headers: api.TooManyRequestsResponseHeaders{RetryAfter: &seconds},
		}}, nil
	case errors.Is(err, ErrNotFound):
		// No such account and "you may not talk to them" answer the same.
		return api.StartConversation404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrNoMembers), errors.Is(err, ErrTooManyMembers):
		return api.StartConversation400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "start conversation", slog.Any("error", err))
		return api.StartConversation500JSONResponse(api.Error(internal("could not open the conversation"))), nil
	}

	// A pair that already had a dialogue gets it back: 200, not 201.
	for _, existing := range before {
		if existing.ID == conversation.ID {
			return api.StartConversation200JSONResponse(toAPIConversation(conversation)), nil
		}
	}
	return api.StartConversation201JSONResponse(toAPIConversation(conversation)), nil
}

func (h *Handler) GetConversation(ctx context.Context, req api.GetConversationRequestObject) (api.GetConversationResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.GetConversation401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	conversation, err := h.svc.Get(ctx, req.ConversationId, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.GetConversation404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "get conversation", slog.Any("error", err))
		return api.GetConversation500JSONResponse(api.Error(internal("could not read the conversation"))), nil
	}
	return api.GetConversation200JSONResponse(toAPIConversation(conversation)), nil
}

func toAPIConversation(c Conversation) api.Conversation {
	members := make([]api.ConversationMember, 0, len(c.Members))
	for _, m := range c.Members {
		members = append(members, api.ConversationMember{
			Id:       m.ID,
			Nickname: m.Nickname,
			IsGuest:  m.IsGuest,
			JoinedAt: m.JoinedAt,
		})
	}
	return api.Conversation{
		Id:        c.ID,
		Kind:      api.ConversationKind(c.Kind),
		Members:   members,
		CreatedAt: c.CreatedAt,
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
