package privacy

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

// Handler serves the generated privacy operations; the rules live in Service.
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

func (h *Handler) ListUserBlocks(ctx context.Context, _ api.ListUserBlocksRequestObject) (api.ListUserBlocksResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListUserBlocks401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	blocked, err := h.svc.ListBlocks(ctx, userID)
	if err != nil {
		h.log.ErrorContext(ctx, "list blocks", slog.Any("error", err))
		return api.ListUserBlocks500JSONResponse(internal("could not list the blocks")), nil
	}

	out := make(api.ListUserBlocks200JSONResponse, 0, len(blocked))
	for _, user := range blocked {
		out = append(out, api.BlockedUser{
			Id:        user.ID,
			Nickname:  user.Nickname,
			CreatedAt: user.BlockedAt,
		})
	}
	return out, nil
}

func (h *Handler) BlockUser(ctx context.Context, req api.BlockUserRequestObject) (api.BlockUserResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.BlockUser401JSONResponse(api.Error(unauthorized())), nil
	}

	err := h.svc.Block(ctx, userID, req.UserId)
	switch {
	case errors.Is(err, ErrCannotBlockSelf):
		return api.BlockUser400JSONResponse{ErrorJSONResponse: badRequest("you cannot block yourself")}, nil
	case errors.Is(err, ErrNotFound):
		// The same answer as a refusal elsewhere: blocking must not tell whether
		// the id names an account.
		return api.BlockUser404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "block account", slog.Any("error", err))
		return api.BlockUser500JSONResponse(internal("could not block the account")), nil
	}
	return api.BlockUser204Response{}, nil
}

func (h *Handler) UnblockUser(ctx context.Context, req api.UnblockUserRequestObject) (api.UnblockUserResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.UnblockUser401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	if err := h.svc.Unblock(ctx, userID, req.UserId); err != nil {
		h.log.ErrorContext(ctx, "unblock account", slog.Any("error", err))
		return api.UnblockUser500JSONResponse(internal("could not unblock the account")), nil
	}
	return api.UnblockUser204Response{}, nil
}

func (h *Handler) GetDmPolicy(ctx context.Context, _ api.GetDmPolicyRequestObject) (api.GetDmPolicyResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.GetDmPolicy401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	policy, err := h.svc.Policy(ctx, userID)
	if err != nil {
		h.log.ErrorContext(ctx, "get dm policy", slog.Any("error", err))
		return api.GetDmPolicy500JSONResponse(internal("could not read the direct message policy")), nil
	}
	return api.GetDmPolicy200JSONResponse(api.DmPolicySetting{DmPolicy: api.DmPolicy(policy)}), nil
}

func (h *Handler) SetDmPolicy(ctx context.Context, req api.SetDmPolicyRequestObject) (api.SetDmPolicyResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.SetDmPolicy401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.SetDmPolicy400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	policy, err := ParsePolicy(string(req.Body.DmPolicy))
	if err != nil {
		return api.SetDmPolicy400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	}
	if err := h.svc.SetPolicy(ctx, userID, policy); err != nil {
		if errors.Is(err, ErrInvalidPolicy) {
			return api.SetDmPolicy400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
		}
		h.log.ErrorContext(ctx, "set dm policy", slog.Any("error", err))
		return api.SetDmPolicy500JSONResponse(internal("could not change the direct message policy")), nil
	}
	return api.SetDmPolicy200JSONResponse(api.DmPolicySetting{DmPolicy: api.DmPolicy(policy)}), nil
}

func unauthorized() api.ErrorJSONResponse {
	return errorBody(httpx.CodeUnauthorized, "sign in first")
}

// notFound answers both "no such account" and "you may not block it".
func notFound() api.ErrorJSONResponse {
	return errorBody(httpx.CodeNotFound, "not found")
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
