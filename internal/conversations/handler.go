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
	"github.com/ruslancode23/orbit-server/internal/profile"
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

func (h *Handler) ListConversations(ctx context.Context, req api.ListConversationsRequestObject) (api.ListConversationsResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListConversations401JSONResponse(api.Error(unauthorized())), nil
	}

	page, err := h.svc.List(ctx, userID, value(req.Params.Cursor), value(req.Params.Limit))
	switch {
	case errors.Is(err, ErrInvalidCursor):
		return api.ListConversations400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "list conversations", slog.Any("error", err))
		return api.ListConversations500JSONResponse(api.Error(internal("could not list the conversations"))), nil
	}

	out := api.ConversationPage{
		Conversations: make([]api.ConversationSummary, 0, len(page.Conversations)),
	}
	for _, conversation := range page.Conversations {
		out.Conversations = append(out.Conversations, toAPIConversationSummary(conversation))
	}
	if page.NextCursor != "" {
		next := page.NextCursor
		out.NextCursor = &next
	}
	return api.ListConversations200JSONResponse(out), nil
}

func (h *Handler) StartConversation(ctx context.Context, req api.StartConversationRequestObject) (api.StartConversationResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.StartConversation401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.StartConversation400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	conversation, created, err := h.svc.Start(ctx, userID, req.Body.UserIds)
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
	if !created {
		return api.StartConversation200JSONResponse(toAPIConversation(conversation)), nil
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

func (h *Handler) GetConversationUnread(ctx context.Context, _ api.GetConversationUnreadRequestObject) (api.GetConversationUnreadResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.GetConversationUnread401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	summary, err := h.svc.Unread(ctx, userID)
	if err != nil {
		h.log.ErrorContext(ctx, "count conversation unread", slog.Any("error", err))
		return api.GetConversationUnread500JSONResponse(api.Error(internal("could not count the unread messages"))), nil
	}
	return api.GetConversationUnread200JSONResponse(api.ConversationUnread{
		Unread:   summary.Unread,
		Mentions: summary.Mentions,
	}), nil
}

func (h *Handler) UpdateConversation(ctx context.Context, req api.UpdateConversationRequestObject) (api.UpdateConversationResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.UpdateConversation401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.UpdateConversation400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	conversation, err := h.svc.Rename(ctx, req.ConversationId, userID, req.Body.Name)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.UpdateConversation404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrNotGroup):
		return api.UpdateConversation409JSONResponse(api.Error(conflict("a direct dialogue cannot be renamed; create a group instead"))), nil
	case errors.Is(err, ErrInvalidName):
		return api.UpdateConversation400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "rename conversation", slog.Any("error", err))
		return api.UpdateConversation500JSONResponse(api.Error(internal("could not rename the conversation"))), nil
	}
	return api.UpdateConversation200JSONResponse(toAPIConversation(conversation)), nil
}

func (h *Handler) AddConversationMembers(ctx context.Context, req api.AddConversationMembersRequestObject) (api.AddConversationMembersResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.AddConversationMembers401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.AddConversationMembers400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	conversation, err := h.svc.AddMembers(ctx, req.ConversationId, userID, req.Body.UserIds)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.AddConversationMembers404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrNotGroup):
		return api.AddConversationMembers409JSONResponse(api.Error(conflict("a direct dialogue cannot become a group; create a new group instead"))), nil
	case errors.Is(err, ErrTooManyMembers):
		return api.AddConversationMembers409JSONResponse(api.Error(conflict(err.Error()))), nil
	case errors.Is(err, ErrNoMembers):
		return api.AddConversationMembers400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "add conversation members", slog.Any("error", err))
		return api.AddConversationMembers500JSONResponse(api.Error(internal("could not add the members"))), nil
	}
	return api.AddConversationMembers200JSONResponse(toAPIConversation(conversation)), nil
}

func (h *Handler) LeaveConversation(ctx context.Context, req api.LeaveConversationRequestObject) (api.LeaveConversationResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.LeaveConversation401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	err := h.svc.Leave(ctx, req.ConversationId, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.LeaveConversation404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrNotGroup):
		return api.LeaveConversation409JSONResponse(api.Error(conflict("a direct dialogue cannot be left; hide it instead"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "leave conversation", slog.Any("error", err))
		return api.LeaveConversation500JSONResponse(api.Error(internal("could not leave the conversation"))), nil
	}
	return api.LeaveConversation204Response{}, nil
}

func (h *Handler) HideConversation(ctx context.Context, req api.HideConversationRequestObject) (api.HideConversationResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.HideConversation401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	err := h.svc.SetHidden(ctx, req.ConversationId, userID, true)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.HideConversation404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "hide conversation", slog.Any("error", err))
		return api.HideConversation500JSONResponse(api.Error(internal("could not hide the conversation"))), nil
	}
	return api.HideConversation204Response{}, nil
}

func (h *Handler) UnhideConversation(ctx context.Context, req api.UnhideConversationRequestObject) (api.UnhideConversationResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.UnhideConversation401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	err := h.svc.SetHidden(ctx, req.ConversationId, userID, false)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.UnhideConversation404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "unhide conversation", slog.Any("error", err))
		return api.UnhideConversation500JSONResponse(api.Error(internal("could not show the conversation"))), nil
	}
	return api.UnhideConversation204Response{}, nil
}

// toAPIConversation is the viewer-neutral shape: the group, its people and its
// title. The per-viewer counters live in the list summary and in events.
func toAPIConversation(c Conversation) api.Conversation {
	conversation := api.Conversation{
		Id:        c.ID,
		Kind:      api.ConversationKind(c.Kind),
		Members:   toAPIMembers(c.Members),
		CreatedAt: c.CreatedAt,
	}
	if c.Name != "" {
		name := c.Name
		conversation.Name = &name
	}
	return conversation
}

func toAPIConversationSummary(c Conversation) api.ConversationSummary {
	summary := api.ConversationSummary{
		Id:          c.ID,
		Kind:        api.ConversationSummaryKind(c.Kind),
		Members:     toAPIMembers(c.Members),
		CreatedAt:   c.CreatedAt,
		LastReadSeq: c.LastReadSeq,
		Unread:      c.Unread,
		Mentions:    c.Mentions,
	}
	if c.Name != "" {
		name := c.Name
		summary.Name = &name
	}
	if c.LastMessage != nil {
		last := toAPILastMessage(*c.LastMessage)
		summary.LastMessage = &last
	}
	return summary
}

func toAPILastMessage(m LastMessage) api.ConversationLastMessage {
	last := api.ConversationLastMessage{
		Id:        m.ID,
		Seq:       m.Seq,
		Kind:      api.MessageKind(m.Kind),
		Content:   m.Content,
		CreatedAt: m.CreatedAt,
		DeletedAt: m.DeletedAt,
	}
	if m.Author != nil {
		author := profile.APIProfile(*m.Author)
		last.Author = &author
	}
	last.System = toAPISystem(m.System)
	return last
}

func toAPISystem(system *SystemMessage) *api.SystemMessage {
	if system == nil {
		return nil
	}
	out := api.SystemMessage{Type: api.SystemMessageType(system.Type)}
	if system.TargetID != nil {
		target := *system.TargetID
		out.TargetId = &target
	}
	if system.Name != "" {
		name := system.Name
		out.Name = &name
	}
	return &out
}

func toAPIMembers(members []Member) []api.ConversationMember {
	out := make([]api.ConversationMember, 0, len(members))
	for _, m := range members {
		// The same public fields as everywhere else, plus when they joined.
		user := profile.APIProfile(m.Public)
		out = append(out, api.ConversationMember{
			Id:           user.Id,
			Nickname:     user.Nickname,
			DisplayName:  user.DisplayName,
			AvatarUrl:    user.AvatarUrl,
			AccentColor:  user.AccentColor,
			Bio:          user.Bio,
			CustomStatus: user.CustomStatus,
			IsGuest:      user.IsGuest,
			CreatedAt:    user.CreatedAt,
			JoinedAt:     m.JoinedAt,
		})
	}
	return out
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

func notFound() api.ErrorJSONResponse {
	return errorBody(httpx.CodeNotFound, "not found")
}

func badRequest(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeInvalidRequest, message)
}

func conflict(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeConflict, message)
}

func internal(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeInternal, message)
}

func errorBody(code, message string) api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{Code: code, Message: message}}
}
