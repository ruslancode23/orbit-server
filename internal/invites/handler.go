package invites

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

// Cookies writes the session cookie for a guest who just came in by link.
type Cookies interface {
	SessionCookie(creds auth.Credentials) string
}

type Handler struct {
	svc     *Service
	cookies Cookies
	log     *slog.Logger
}

func NewHandler(svc *Service, cookies Cookies, log *slog.Logger) *Handler {
	return &Handler{svc: svc, cookies: cookies, log: log}
}

func caller(ctx context.Context) (uuid.UUID, bool) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return uuid.Nil, false
	}
	return session.User.ID, true
}

func (h *Handler) CreateInvite(ctx context.Context, req api.CreateInviteRequestObject) (api.CreateInviteResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.CreateInvite401JSONResponse(api.Error(unauthorized())), nil
	}

	var wanted New
	if req.Body != nil {
		if req.Body.ExpiresInHours != nil {
			lifetime := time.Duration(*req.Body.ExpiresInHours) * time.Hour
			wanted.ExpiresIn = &lifetime
		}
		wanted.MaxUses = req.Body.MaxUses
	}

	invite, err := h.svc.Create(ctx, req.WorkspaceId, userID, wanted)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.CreateInvite404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrForbidden):
		return api.CreateInvite403JSONResponse(api.Error(forbidden("this server does not let members invite"))), nil
	case errors.Is(err, ErrInvalidLifetime), errors.Is(err, ErrInvalidUses):
		return api.CreateInvite400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "create invite", slog.Any("error", err))
		return api.CreateInvite500JSONResponse(api.Error(internal("could not create the invite"))), nil
	}
	return api.CreateInvite201JSONResponse(toAPIInvite(invite)), nil
}

func (h *Handler) ListInvites(ctx context.Context, req api.ListInvitesRequestObject) (api.ListInvitesResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListInvites401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	list, err := h.svc.List(ctx, req.WorkspaceId, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.ListInvites404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrForbidden):
		return api.ListInvites403JSONResponse(api.Error(forbidden("the invite list needs the admin role"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "list invites", slog.Any("error", err))
		return api.ListInvites500JSONResponse(api.Error(internal("could not list the invites"))), nil
	}

	out := make(api.ListInvites200JSONResponse, 0, len(list))
	for _, invite := range list {
		out = append(out, toAPIInvite(invite))
	}
	return out, nil
}

func (h *Handler) RevokeInvite(ctx context.Context, req api.RevokeInviteRequestObject) (api.RevokeInviteResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.RevokeInvite401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	err := h.svc.Revoke(ctx, req.InviteId, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.RevokeInvite404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrForbidden):
		return api.RevokeInvite403JSONResponse(api.Error(forbidden("revoking an invite needs the admin role"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "revoke invite", slog.Any("error", err))
		return api.RevokeInvite500JSONResponse(api.Error(internal("could not revoke the invite"))), nil
	}
	return api.RevokeInvite204Response{}, nil
}

// PreviewInvite needs no session: the page has to say what the link leads to
// before it asks anyone for a name.
func (h *Handler) PreviewInvite(ctx context.Context, req api.PreviewInviteRequestObject) (api.PreviewInviteResponseObject, error) {
	preview, err := h.svc.Preview(ctx, req.Code)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.PreviewInvite404JSONResponse{ErrorJSONResponse: notFound()}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "preview invite", slog.Any("error", err))
		return api.PreviewInvite500JSONResponse(api.Error(internal("could not read the invite"))), nil
	}
	return api.PreviewInvite200JSONResponse{
		WorkspaceId:   preview.WorkspaceID,
		WorkspaceName: preview.WorkspaceName,
		MemberCount:   preview.MemberCount,
	}, nil
}

func (h *Handler) AcceptInvite(ctx context.Context, req api.AcceptInviteRequestObject) (api.AcceptInviteResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.AcceptInvite401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	membership, err := h.svc.Accept(ctx, req.Code, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.AcceptInvite404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrBanned):
		return api.AcceptInvite403JSONResponse(api.Error(forbidden("you are banned from this server"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "accept invite", slog.Any("error", err))
		return api.AcceptInvite500JSONResponse(api.Error(internal("could not join the server"))), nil
	}
	return api.AcceptInvite200JSONResponse(toAPIMembership(membership)), nil
}

func (h *Handler) JoinAsGuest(ctx context.Context, req api.JoinAsGuestRequestObject) (api.JoinAsGuestResponseObject, error) {
	if req.Body == nil {
		return api.JoinAsGuest400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	user, creds, membership, err := h.svc.JoinAsGuest(ctx, req.Code, req.Body.Nickname)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.JoinAsGuest404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrNicknameTaken):
		return api.JoinAsGuest409JSONResponse(api.Error(conflict("this nickname is taken"))), nil
	case errors.Is(err, ErrBanned):
		// A ban follows the account, and a guest is a new one; this can only
		// happen if the name was reused, so answer as for a dead link.
		return api.JoinAsGuest404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "join as guest", slog.Any("error", err))
		return api.JoinAsGuest500JSONResponse(api.Error(internal("could not enter as a guest"))), nil
	}

	cookie := h.cookies.SessionCookie(creds)
	return api.JoinAsGuest201JSONResponse{
		Body: api.GuestJoin{
			User:       toAPIUser(user),
			Membership: toAPIMembership(membership),
		},
		Headers: api.JoinAsGuest201ResponseHeaders{SetCookie: &cookie},
	}, nil
}

func toAPIInvite(invite Invite) api.Invite {
	out := api.Invite{
		Id:        invite.ID,
		Code:      invite.Code,
		Uses:      invite.Uses,
		CreatedAt: invite.CreatedAt,
	}
	if invite.ExpiresAt != nil {
		expiresAt := *invite.ExpiresAt
		out.ExpiresAt = &expiresAt
	}
	if invite.MaxUses != nil {
		maxUses := *invite.MaxUses
		out.MaxUses = &maxUses
	}
	return out
}

func toAPIMembership(m servers.Membership) api.WorkspaceMembership {
	return api.WorkspaceMembership{
		Workspace: api.Workspace{
			Id:        m.Workspace.ID,
			Name:      m.Workspace.Name,
			OwnerId:   m.Workspace.OwnerID,
			CreatedAt: m.Workspace.CreatedAt,
		},
		Role: api.WorkspaceRole(m.Role),
	}
}

func toAPIUser(u auth.User) api.User {
	user := api.User{
		Id:        u.ID,
		Nickname:  u.Nickname,
		IsGuest:   u.IsGuest,
		CreatedAt: u.CreatedAt,
	}
	if u.Email != "" {
		mail := openapi_types.Email(u.Email)
		user.Email = &mail
	}
	return user
}

// cookieWriter turns auth's cookie rules into the Cookies port, so this package
// does not spell out the cookie's flags a second time.
type cookieWriter struct{ handler *auth.Handler }

// NewCookies adapts the auth handler to the Cookies port.
func NewCookies(handler *auth.Handler) Cookies { return cookieWriter{handler: handler} }

func (c cookieWriter) SessionCookie(creds auth.Credentials) string {
	return c.handler.SessionCookie(creds)
}

func unauthorized() api.ErrorJSONResponse {
	return errorBody(httpx.CodeUnauthorized, "sign in first")
}

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

func internal(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeInternal, message)
}

func errorBody(code, message string) api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{Code: code, Message: message}}
}
