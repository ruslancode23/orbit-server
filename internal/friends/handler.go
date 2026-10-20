package friends

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

// requestAcceptedMessage is the only body a friendship request ever gets. It
// must not change with the outcome: a different answer for an account that
// exists, allows requests and is not blocked would turn the endpoint into an
// existence oracle.
const requestAcceptedMessage = "If such a user exists, they will receive your request."

// Handler serves the generated friends operations; the rules live in Service.
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

func (h *Handler) ListFriends(ctx context.Context, _ api.ListFriendsRequestObject) (api.ListFriendsResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListFriends401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	list, err := h.svc.Friends(ctx, userID)
	if err != nil {
		h.log.ErrorContext(ctx, "list friends", slog.Any("error", err))
		return api.ListFriends500JSONResponse(internal("could not list the friends")), nil
	}

	ids := make([]uuid.UUID, 0, len(list))
	for _, friend := range list {
		ids = append(ids, friend.ID)
	}
	statuses := h.svc.Statuses(ids)

	out := make(api.ListFriends200JSONResponse, 0, len(list))
	for _, friend := range list {
		out = append(out, api.Friend{
			Id:       friend.ID,
			Nickname: friend.Nickname,
			Presence: statuses[friend.ID],
			Since:    friend.Since,
		})
	}
	return out, nil
}

func (h *Handler) CreateFriendRequest(ctx context.Context, req api.CreateFriendRequestRequestObject) (api.CreateFriendRequestResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.CreateFriendRequest401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.CreateFriendRequest400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	err := h.svc.Request(ctx, userID, req.Body.Nickname)
	var limited *RateLimitedError
	switch {
	case errors.As(err, &limited):
		return api.CreateFriendRequest429JSONResponse{TooManyRequestsJSONResponse: rateLimited(limited.RetryAfter)}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "send friendship request", slog.Any("error", err))
		return api.CreateFriendRequest500JSONResponse(internal("could not send the request")), nil
	}
	// Success, a missing account and every refusal share this answer.
	return api.CreateFriendRequest202JSONResponse{Message: requestAcceptedMessage}, nil
}

func (h *Handler) ListFriendRequests(ctx context.Context, req api.ListFriendRequestsRequestObject) (api.ListFriendRequestsResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListFriendRequests401JSONResponse(api.Error(unauthorized())), nil
	}
	if !req.Params.Direction.Valid() {
		return api.ListFriendRequests400JSONResponse{ErrorJSONResponse: badRequest("direction must be incoming or outgoing")}, nil
	}

	var (
		requests []Request
		err      error
	)
	if req.Params.Direction == api.Incoming {
		requests, err = h.svc.Incoming(ctx, userID)
	} else {
		requests, err = h.svc.Outgoing(ctx, userID)
	}
	if err != nil {
		h.log.ErrorContext(ctx, "list friendship requests", slog.Any("error", err))
		return api.ListFriendRequests500JSONResponse(internal("could not list the requests")), nil
	}

	out := make(api.ListFriendRequests200JSONResponse, 0, len(requests))
	for _, request := range requests {
		out = append(out, api.FriendRequest{
			UserId:    request.User.ID,
			Nickname:  request.User.Nickname,
			Direction: api.FriendRequestDirection(request.Direction),
			CreatedAt: request.CreatedAt,
		})
	}
	return out, nil
}

func (h *Handler) AcceptFriendRequest(ctx context.Context, req api.AcceptFriendRequestRequestObject) (api.AcceptFriendRequestResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.AcceptFriendRequest401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	friend, err := h.svc.Accept(ctx, userID, req.UserId)
	switch {
	case errors.Is(err, ErrNotFound):
		// Not addressed to the caller, already answered, blocked, or no request:
		// one answer, so the endpoint cannot be used to probe requests.
		return api.AcceptFriendRequest404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrFriendLimit):
		return api.AcceptFriendRequest409JSONResponse(api.Error(conflict(err.Error()))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "accept friendship request", slog.Any("error", err))
		return api.AcceptFriendRequest500JSONResponse(internal("could not accept the request")), nil
	}

	statuses := h.svc.Statuses([]uuid.UUID{friend.ID})
	return api.AcceptFriendRequest200JSONResponse(api.Friend{
		Id:       friend.ID,
		Nickname: friend.Nickname,
		Presence: statuses[friend.ID],
		Since:    friend.Since,
	}), nil
}

func (h *Handler) DeleteFriendRequest(ctx context.Context, req api.DeleteFriendRequestRequestObject) (api.DeleteFriendRequestResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.DeleteFriendRequest401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	err := h.svc.Delete(ctx, userID, req.UserId)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.DeleteFriendRequest404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "delete friendship request", slog.Any("error", err))
		return api.DeleteFriendRequest500JSONResponse(internal("could not delete the request")), nil
	}
	return api.DeleteFriendRequest204Response{}, nil
}

func (h *Handler) RemoveFriend(ctx context.Context, req api.RemoveFriendRequestObject) (api.RemoveFriendResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.RemoveFriend401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	if err := h.svc.Remove(ctx, userID, req.UserId); err != nil {
		h.log.ErrorContext(ctx, "remove friend", slog.Any("error", err))
		return api.RemoveFriend500JSONResponse(internal("could not remove the friend")), nil
	}
	return api.RemoveFriend204Response{}, nil
}

func rateLimited(retryAfter time.Duration) api.TooManyRequestsJSONResponse {
	seconds := int((retryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return api.TooManyRequestsJSONResponse{
		Body:    api.Error(errorBody(httpx.CodeTooManyRequests, "too many friendship requests, try again later")),
		Headers: api.TooManyRequestsResponseHeaders{RetryAfter: &seconds},
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

func internal(message string) api.Error {
	return api.Error(errorBody(httpx.CodeInternal, message))
}

func conflict(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeConflict, message)
}

func errorBody(code, message string) api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{Code: code, Message: message}}
}
