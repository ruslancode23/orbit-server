package messages

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

// Handler serves the generated message operations; the rules live in Service.
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

func (h *Handler) ListMessages(ctx context.Context, req api.ListMessagesRequestObject) (api.ListMessagesResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListMessages401JSONResponse(api.Error(unauthorized())), nil
	}

	var limit int
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}

	page, err := h.svc.History(ctx, req.ChannelId, userID, req.Params.Before, limit)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return api.ListMessages404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "list messages", slog.Any("error", err))
		return api.ListMessages500JSONResponse(internal("could not read the history")), nil
	}

	out := api.MessagePage{Messages: make([]api.Message, 0, len(page.Messages)), NextBefore: page.NextBefore}
	for _, m := range page.Messages {
		out.Messages = append(out.Messages, APIMessage(m))
	}
	return api.ListMessages200JSONResponse(out), nil
}

func (h *Handler) SendMessage(ctx context.Context, req api.SendMessageRequestObject) (api.SendMessageResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.SendMessage401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.SendMessage400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	message, existed, err := h.svc.Send(ctx, userID, NewMessage{
		ChannelID:       req.ChannelId,
		ClientMessageID: req.Body.ClientMessageId,
		Content:         req.Body.Content,
		ReplyToID:       req.Body.ReplyToId,
		Mentions:        value(req.Body.Mentions),
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return api.SendMessage404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrForbidden):
		return api.SendMessage403JSONResponse(api.Error(forbidden("this channel is read-only"))), nil
	case errors.Is(err, ErrInvalidContent), errors.Is(err, ErrInvalidReply):
		return api.SendMessage400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "send message", slog.Any("error", err))
		return api.SendMessage500JSONResponse(internal("could not send the message")), nil
	}

	// A resend of the same client id is not a new message: 200, not 201.
	if existed {
		return api.SendMessage200JSONResponse(APIMessage(message)), nil
	}
	return api.SendMessage201JSONResponse(APIMessage(message)), nil
}

func (h *Handler) EditMessage(ctx context.Context, req api.EditMessageRequestObject) (api.EditMessageResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.EditMessage401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.EditMessage400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	message, err := h.svc.Edit(ctx, req.MessageId, userID, req.Body.Content)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.EditMessage404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrForbidden):
		return api.EditMessage403JSONResponse(api.Error(forbidden("only the author edits a message"))), nil
	case errors.Is(err, ErrInvalidContent):
		return api.EditMessage400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "edit message", slog.Any("error", err))
		return api.EditMessage500JSONResponse(internal("could not edit the message")), nil
	}
	return api.EditMessage200JSONResponse(APIMessage(message)), nil
}

func (h *Handler) DeleteMessage(ctx context.Context, req api.DeleteMessageRequestObject) (api.DeleteMessageResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.DeleteMessage401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	message, err := h.svc.Delete(ctx, req.MessageId, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.DeleteMessage404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrForbidden):
		return api.DeleteMessage403JSONResponse(api.Error(forbidden("only the author deletes a message"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "delete message", slog.Any("error", err))
		return api.DeleteMessage500JSONResponse(internal("could not delete the message")), nil
	}
	return api.DeleteMessage200JSONResponse(APIMessage(message)), nil
}

func (h *Handler) AddReaction(ctx context.Context, req api.AddReactionRequestObject) (api.AddReactionResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.AddReaction401JSONResponse(api.Error(unauthorized())), nil
	}

	message, err := h.svc.React(ctx, req.MessageId, userID, req.Emoji)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.AddReaction404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrForbidden):
		return api.AddReaction403JSONResponse(api.Error(forbidden("you cannot write in this channel"))), nil
	case errors.Is(err, ErrInvalidEmoji):
		return api.AddReaction400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "add reaction", slog.Any("error", err))
		return api.AddReaction500JSONResponse(internal("could not add the reaction")), nil
	}
	return api.AddReaction200JSONResponse(APIMessage(message)), nil
}

func (h *Handler) RemoveReaction(ctx context.Context, req api.RemoveReactionRequestObject) (api.RemoveReactionResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.RemoveReaction401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	message, err := h.svc.Unreact(ctx, req.MessageId, userID, req.Emoji)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden), errors.Is(err, ErrInvalidEmoji):
		return api.RemoveReaction404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "remove reaction", slog.Any("error", err))
		return api.RemoveReaction500JSONResponse(internal("could not remove the reaction")), nil
	}
	return api.RemoveReaction200JSONResponse(APIMessage(message)), nil
}

// APIMessage is the shape a message takes on the wire, for HTTP and for the
// live stream alike: the client parses one thing.
func APIMessage(m Message) api.Message {
	message := api.Message{
		Id:        m.ID,
		ChannelId: m.ChannelID,
		Seq:       m.Seq,
		Content:   m.Content,
		ReplyToId: m.ReplyToID,
		Mentions:  m.Mentions,
		Reactions: make([]api.Reaction, 0, len(m.Reactions)),
		CreatedAt: m.CreatedAt,
		EditedAt:  m.EditedAt,
		DeletedAt: m.DeletedAt,
	}
	if message.Mentions == nil {
		message.Mentions = []openapi_types.UUID{}
	}
	for _, r := range m.Reactions {
		message.Reactions = append(message.Reactions, api.Reaction{
			Emoji: r.Emoji, Count: r.Count, Reacted: r.Reacted,
		})
	}
	if m.Author != nil {
		author := api.User{
			Id:        m.Author.ID,
			Nickname:  m.Author.Nickname,
			IsGuest:   m.Author.IsGuest,
			CreatedAt: m.Author.CreatedAt,
		}
		if m.Author.Email != "" {
			mail := openapi_types.Email(m.Author.Email)
			author.Email = &mail
		}
		message.Author = &author
	}
	return message
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

// notFound answers both "no such message" and "a channel you cannot read".
func notFound() api.ErrorJSONResponse {
	return errorBody(httpx.CodeNotFound, "not found")
}

func forbidden(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeForbidden, message)
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
