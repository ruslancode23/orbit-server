package servers

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

// Handler serves the generated workspace operations. It only maps HTTP to the
// service and back: every permission rule lives in Service.
type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// caller returns the account of the session, or false when there is none.
func caller(ctx context.Context) (uuid.UUID, bool) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return uuid.Nil, false
	}
	return session.User.ID, true
}

func (h *Handler) ListWorkspaces(ctx context.Context, _ api.ListWorkspacesRequestObject) (api.ListWorkspacesResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListWorkspaces401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	memberships, err := h.svc.List(ctx, userID)
	if err != nil {
		h.log.ErrorContext(ctx, "list workspaces", slog.Any("error", err))
		return api.ListWorkspaces500JSONResponse(internal("could not list the servers")), nil
	}

	out := make(api.ListWorkspaces200JSONResponse, 0, len(memberships))
	for _, m := range memberships {
		out = append(out, toAPIMembership(m))
	}
	return out, nil
}

func (h *Handler) CreateWorkspace(ctx context.Context, req api.CreateWorkspaceRequestObject) (api.CreateWorkspaceResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.CreateWorkspace401JSONResponse(unauthorized()), nil
	}
	if req.Body == nil {
		return api.CreateWorkspace400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	membership, err := h.svc.Create(ctx, req.Body.Name, userID)
	switch {
	case errors.Is(err, ErrInvalidName):
		return api.CreateWorkspace400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "create workspace", slog.Any("error", err))
		return api.CreateWorkspace500JSONResponse(internal("could not create the server")), nil
	}
	return api.CreateWorkspace201JSONResponse(toAPIMembership(membership)), nil
}

func (h *Handler) GetWorkspace(ctx context.Context, req api.GetWorkspaceRequestObject) (api.GetWorkspaceResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.GetWorkspace401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	membership, err := h.svc.Get(ctx, req.WorkspaceId, userID)
	switch {
	case errors.Is(err, ErrNotMember), errors.Is(err, ErrForbidden):
		return api.GetWorkspace404JSONResponse(notFound()), nil
	case err != nil:
		h.log.ErrorContext(ctx, "get workspace", slog.Any("error", err))
		return api.GetWorkspace500JSONResponse(internal("could not read the server")), nil
	}
	return api.GetWorkspace200JSONResponse(toAPIMembership(membership)), nil
}

func (h *Handler) UpdateWorkspace(ctx context.Context, req api.UpdateWorkspaceRequestObject) (api.UpdateWorkspaceResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.UpdateWorkspace401JSONResponse(unauthorized()), nil
	}
	if req.Body == nil {
		return api.UpdateWorkspace400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	membership, err := h.svc.Rename(ctx, req.WorkspaceId, userID, req.Body.Name)
	switch {
	case errors.Is(err, ErrNotMember):
		return api.UpdateWorkspace404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden):
		return api.UpdateWorkspace403JSONResponse(forbidden("renaming a server needs the admin role")), nil
	case errors.Is(err, ErrInvalidName):
		return api.UpdateWorkspace400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "rename workspace", slog.Any("error", err))
		return api.UpdateWorkspace500JSONResponse(internal("could not rename the server")), nil
	}
	return api.UpdateWorkspace200JSONResponse(toAPIMembership(membership)), nil
}

func (h *Handler) DeleteWorkspace(ctx context.Context, req api.DeleteWorkspaceRequestObject) (api.DeleteWorkspaceResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.DeleteWorkspace401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	err := h.svc.Delete(ctx, req.WorkspaceId, userID)
	switch {
	case errors.Is(err, ErrNotMember):
		return api.DeleteWorkspace404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden):
		return api.DeleteWorkspace403JSONResponse(forbidden("only the owner deletes a server")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "delete workspace", slog.Any("error", err))
		return api.DeleteWorkspace500JSONResponse(internal("could not delete the server")), nil
	}
	return api.DeleteWorkspace204Response{}, nil
}

func (h *Handler) ListWorkspaceMembers(ctx context.Context, req api.ListWorkspaceMembersRequestObject) (api.ListWorkspaceMembersResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListWorkspaceMembers401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	members, err := h.svc.ListMembers(ctx, req.WorkspaceId, userID)
	switch {
	case errors.Is(err, ErrNotMember), errors.Is(err, ErrForbidden):
		return api.ListWorkspaceMembers404JSONResponse(notFound()), nil
	case err != nil:
		h.log.ErrorContext(ctx, "list workspace members", slog.Any("error", err))
		return api.ListWorkspaceMembers500JSONResponse(internal("could not list the members")), nil
	}

	out := make(api.ListWorkspaceMembers200JSONResponse, 0, len(members))
	for _, m := range members {
		out = append(out, toAPIMember(m))
	}
	return out, nil
}

func (h *Handler) SetWorkspaceMemberRole(ctx context.Context, req api.SetWorkspaceMemberRoleRequestObject) (api.SetWorkspaceMemberRoleResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.SetWorkspaceMemberRole401JSONResponse(unauthorized()), nil
	}
	if req.Body == nil {
		return api.SetWorkspaceMemberRole400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	member, err := h.svc.SetRole(ctx, req.WorkspaceId, userID, req.UserId, Role(req.Body.Role))
	switch {
	case errors.Is(err, ErrNotMember):
		return api.SetWorkspaceMemberRole404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden):
		return api.SetWorkspaceMemberRole403JSONResponse(forbidden("only the owner changes roles")), nil
	case errors.Is(err, ErrLastOwner):
		return api.SetWorkspaceMemberRole400JSONResponse{ErrorJSONResponse: badRequest("a server keeps an owner: hand the role over instead")}, nil
	case errors.Is(err, ErrInvalidRole):
		return api.SetWorkspaceMemberRole400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "set workspace role", slog.Any("error", err))
		return api.SetWorkspaceMemberRole500JSONResponse(internal("could not change the role")), nil
	}
	return api.SetWorkspaceMemberRole200JSONResponse(toAPIMember(member)), nil
}

func (h *Handler) RemoveWorkspaceMember(ctx context.Context, req api.RemoveWorkspaceMemberRequestObject) (api.RemoveWorkspaceMemberResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.RemoveWorkspaceMember401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	err := h.svc.Remove(ctx, req.WorkspaceId, userID, req.UserId)
	switch {
	case errors.Is(err, ErrNotMember):
		return api.RemoveWorkspaceMember404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden):
		return api.RemoveWorkspaceMember403JSONResponse(forbidden("this member is not yours to remove")), nil
	case errors.Is(err, ErrLastOwner):
		return api.RemoveWorkspaceMember403JSONResponse(forbidden("hand the owner role over before leaving")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "remove workspace member", slog.Any("error", err))
		return api.RemoveWorkspaceMember500JSONResponse(internal("could not remove the member")), nil
	}
	return api.RemoveWorkspaceMember204Response{}, nil
}

func (h *Handler) BanWorkspaceMember(ctx context.Context, req api.BanWorkspaceMemberRequestObject) (api.BanWorkspaceMemberResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.BanWorkspaceMember401JSONResponse(unauthorized()), nil
	}

	var reason string
	if req.Body != nil && req.Body.Reason != nil {
		reason = *req.Body.Reason
	}

	ban, err := h.svc.Ban(ctx, req.WorkspaceId, userID, req.UserId, reason)
	switch {
	case errors.Is(err, ErrNotMember):
		return api.BanWorkspaceMember404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden):
		return api.BanWorkspaceMember403JSONResponse(forbidden("this member is not yours to ban")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "ban workspace member", slog.Any("error", err))
		return api.BanWorkspaceMember500JSONResponse(internal("could not ban the member")), nil
	}
	return api.BanWorkspaceMember200JSONResponse(toAPIBan(ban)), nil
}

func (h *Handler) UnbanWorkspaceMember(ctx context.Context, req api.UnbanWorkspaceMemberRequestObject) (api.UnbanWorkspaceMemberResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.UnbanWorkspaceMember401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	err := h.svc.Unban(ctx, req.WorkspaceId, userID, req.UserId)
	switch {
	case errors.Is(err, ErrNotMember), errors.Is(err, ErrNotBanned):
		return api.UnbanWorkspaceMember404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden):
		return api.UnbanWorkspaceMember403JSONResponse(forbidden("lifting a ban needs the admin role")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "unban workspace member", slog.Any("error", err))
		return api.UnbanWorkspaceMember500JSONResponse(internal("could not lift the ban")), nil
	}
	return api.UnbanWorkspaceMember204Response{}, nil
}

func (h *Handler) ListWorkspaceBans(ctx context.Context, req api.ListWorkspaceBansRequestObject) (api.ListWorkspaceBansResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListWorkspaceBans401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	bans, err := h.svc.ListBans(ctx, req.WorkspaceId, userID)
	switch {
	case errors.Is(err, ErrNotMember):
		return api.ListWorkspaceBans404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden):
		return api.ListWorkspaceBans403JSONResponse(forbidden("the ban list needs the admin role")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "list workspace bans", slog.Any("error", err))
		return api.ListWorkspaceBans500JSONResponse(internal("could not list the bans")), nil
	}

	out := make(api.ListWorkspaceBans200JSONResponse, 0, len(bans))
	for _, b := range bans {
		out = append(out, toAPIBan(b))
	}
	return out, nil
}

func toAPIMembership(m Membership) api.WorkspaceMembership {
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

func toAPIMember(m Member) api.WorkspaceMember {
	return api.WorkspaceMember{
		User:     toAPIUser(m.User),
		Role:     api.WorkspaceRole(m.Role),
		JoinedAt: m.JoinedAt,
	}
}

func toAPIBan(b Ban) api.WorkspaceBan {
	ban := api.WorkspaceBan{User: toAPIUser(b.User), CreatedAt: b.CreatedAt}
	if b.Reason != "" {
		reason := b.Reason
		ban.Reason = &reason
	}
	if b.BannedBy != uuid.Nil {
		bannedBy := b.BannedBy
		ban.BannedBy = &bannedBy
	}
	return ban
}

func toAPIUser(a Account) api.User {
	user := api.User{
		Id:        a.ID,
		Nickname:  a.Nickname,
		IsGuest:   a.IsGuest,
		CreatedAt: a.CreatedAt,
	}
	if a.Email != "" {
		mail := openapi_types.Email(a.Email)
		user.Email = &mail
	}
	return user
}

func unauthorized() api.ErrorJSONResponse {
	return errorBody(httpx.CodeUnauthorized, "sign in first")
}

// notFound answers both "no such server" and "you are not a member": telling them
// apart would reveal which servers exist.
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
