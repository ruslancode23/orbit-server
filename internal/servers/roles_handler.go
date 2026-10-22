package servers

import (
	"context"
	"errors"
	"log/slog"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/perms"
)

func (h *Handler) ListRoles(ctx context.Context, req api.ListRolesRequestObject) (api.ListRolesResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListRoles401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}
	roles, err := h.svc.ListRoles(ctx, req.WorkspaceId, userID)
	switch {
	case errors.Is(err, ErrNotMember):
		return api.ListRoles404JSONResponse(notFound()), nil
	case err != nil:
		h.log.ErrorContext(ctx, "list roles", slog.Any("error", err))
		return api.ListRoles500JSONResponse(internal("could not list the roles")), nil
	}
	return api.ListRoles200JSONResponse(toAPIRoles(roles)), nil
}

func (h *Handler) CreateRole(ctx context.Context, req api.CreateRoleRequestObject) (api.CreateRoleResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.CreateRole401JSONResponse(unauthorized()), nil
	}
	if req.Body == nil {
		return api.CreateRole400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}
	n := NewRole{Name: req.Body.Name}
	if req.Body.Color != nil {
		n.Color = string(*req.Body.Color)
	}
	if req.Body.Hoist != nil {
		n.Hoist = *req.Body.Hoist
	}
	if req.Body.Permissions != nil {
		set, ok := parsePermissions(*req.Body.Permissions)
		if !ok {
			return api.CreateRole400JSONResponse{ErrorJSONResponse: badRequest("unknown permission")}, nil
		}
		n.Permissions = set
	}
	role, err := h.svc.CreateRole(ctx, req.WorkspaceId, userID, n)
	switch {
	case errors.Is(err, ErrInvalidRoleName), errors.Is(err, ErrInvalidColor):
		return api.CreateRole400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case errors.Is(err, ErrNotMember):
		return api.CreateRole404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden):
		return api.CreateRole403JSONResponse(forbidden("you may not create this role")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "create role", slog.Any("error", err))
		return api.CreateRole500JSONResponse(internal("could not create the role")), nil
	}
	return api.CreateRole201JSONResponse(toAPIRole(role)), nil
}

func (h *Handler) ReorderRoles(ctx context.Context, req api.ReorderRolesRequestObject) (api.ReorderRolesResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ReorderRoles401JSONResponse(unauthorized()), nil
	}
	if req.Body == nil {
		return api.ReorderRoles400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}
	roles, err := h.svc.ReorderRoles(ctx, req.WorkspaceId, userID, req.Body.RoleIds)
	switch {
	case errors.Is(err, ErrIncompleteRoleOrder):
		return api.ReorderRoles400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case errors.Is(err, ErrNotMember):
		return api.ReorderRoles404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden):
		return api.ReorderRoles403JSONResponse(forbidden("you may not move these roles")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "reorder roles", slog.Any("error", err))
		return api.ReorderRoles500JSONResponse(internal("could not reorder the roles")), nil
	}
	return api.ReorderRoles200JSONResponse(toAPIRoles(roles)), nil
}

func (h *Handler) UpdateRole(ctx context.Context, req api.UpdateRoleRequestObject) (api.UpdateRoleResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.UpdateRole401JSONResponse(unauthorized()), nil
	}
	if req.Body == nil {
		return api.UpdateRole400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}
	u := RoleUpdate{Name: req.Body.Name, Hoist: req.Body.Hoist}
	if req.Body.Color != nil {
		color := string(*req.Body.Color)
		u.Color = &color
	}
	if req.Body.ClearColor != nil {
		u.ClearColor = *req.Body.ClearColor
	}
	if req.Body.Permissions != nil {
		set, ok := parsePermissions(*req.Body.Permissions)
		if !ok {
			return api.UpdateRole400JSONResponse{ErrorJSONResponse: badRequest("unknown permission")}, nil
		}
		u.Permissions = &set
	}
	role, err := h.svc.UpdateRole(ctx, req.RoleId, userID, u)
	switch {
	case errors.Is(err, ErrInvalidRoleName), errors.Is(err, ErrInvalidColor), errors.Is(err, ErrDefaultRole):
		return api.UpdateRole400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case errors.Is(err, ErrRoleNotFound), errors.Is(err, ErrNotMember):
		return api.UpdateRole404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden):
		return api.UpdateRole403JSONResponse(forbidden("you may not change this role")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "update role", slog.Any("error", err))
		return api.UpdateRole500JSONResponse(internal("could not update the role")), nil
	}
	return api.UpdateRole200JSONResponse(toAPIRole(role)), nil
}

func (h *Handler) DeleteRole(ctx context.Context, req api.DeleteRoleRequestObject) (api.DeleteRoleResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.DeleteRole401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}
	err := h.svc.DeleteRole(ctx, req.RoleId, userID)
	switch {
	case errors.Is(err, ErrDefaultRole):
		return api.DeleteRole403JSONResponse(forbidden(err.Error())), nil
	case errors.Is(err, ErrRoleNotFound), errors.Is(err, ErrNotMember):
		return api.DeleteRole404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden):
		return api.DeleteRole403JSONResponse(forbidden("you may not delete this role")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "delete role", slog.Any("error", err))
		return api.DeleteRole500JSONResponse(internal("could not delete the role")), nil
	}
	return api.DeleteRole204Response{}, nil
}

func (h *Handler) GrantRole(ctx context.Context, req api.GrantRoleRequestObject) (api.GrantRoleResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.GrantRole401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}
	member, err := h.svc.GrantRole(ctx, req.WorkspaceId, userID, req.UserId, req.RoleId)
	switch {
	case errors.Is(err, ErrRoleNotFound), errors.Is(err, ErrNotMember):
		return api.GrantRole404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrDefaultRole):
		return api.GrantRole403JSONResponse(forbidden("you may not give this role to this member")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "grant role", slog.Any("error", err))
		return api.GrantRole500JSONResponse(internal("could not give the role")), nil
	}
	return api.GrantRole200JSONResponse(toAPIMember(member)), nil
}

func (h *Handler) RevokeRole(ctx context.Context, req api.RevokeRoleRequestObject) (api.RevokeRoleResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.RevokeRole401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}
	member, err := h.svc.RevokeRole(ctx, req.WorkspaceId, userID, req.UserId, req.RoleId)
	switch {
	case errors.Is(err, ErrRoleNotFound), errors.Is(err, ErrNotMember):
		return api.RevokeRole404JSONResponse(notFound()), nil
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrDefaultRole):
		return api.RevokeRole403JSONResponse(forbidden("you may not take this role from this member")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "revoke role", slog.Any("error", err))
		return api.RevokeRole500JSONResponse(internal("could not take the role")), nil
	}
	return api.RevokeRole200JSONResponse(toAPIMember(member)), nil
}

// APIPermissions spells a set for the API; other modules (channels) use it too.
func APIPermissions(set perms.Set) []api.Permission {
	names := set.Names()
	out := make([]api.Permission, 0, len(names))
	for _, name := range names {
		out = append(out, api.Permission(name))
	}
	return out
}

// ParsePermissions reads permissions from the API; false on an unknown name.
func ParsePermissions(list []api.Permission) (perms.Set, bool) {
	return parsePermissions(list)
}

func parsePermissions(list []api.Permission) (perms.Set, bool) {
	names := make([]string, 0, len(list))
	for _, p := range list {
		names = append(names, string(p))
	}
	return perms.Parse(names)
}

func toAPIRole(r ServerRole) api.Role {
	role := api.Role{
		Id:          r.ID,
		Name:        r.Name,
		Hoist:       r.Hoist,
		Position:    r.Position,
		Permissions: APIPermissions(r.Permissions),
		IsDefault:   r.Default,
	}
	if r.Color != "" {
		color := api.AccentColor(r.Color)
		role.Color = &color
	}
	return role
}

func toAPIRoles(roles []ServerRole) []api.Role {
	out := make([]api.Role, 0, len(roles))
	for _, r := range roles {
		out = append(out, toAPIRole(r))
	}
	return out
}
