package profile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

// Handler serves the generated profile operations. The request body is already
// validated against api/openapi.yaml when a method runs; the rules that the
// schema cannot express live in Service.
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

func (h *Handler) UpdateMyProfile(ctx context.Context, req api.UpdateMyProfileRequestObject) (api.UpdateMyProfileResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.UpdateMyProfile401JSONResponse(errorBody(httpx.CodeUnauthorized, "sign in first")), nil
	}
	if req.Body == nil {
		return api.UpdateMyProfile400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	update := Update{DisplayName: req.Body.DisplayName, Bio: req.Body.Bio}
	if req.Body.AccentColor != nil {
		color := AccentColor(*req.Body.AccentColor)
		update.AccentColor = &color
	}

	profile, err := h.svc.Update(ctx, userID, update)
	switch {
	case invalid(err):
		return api.UpdateMyProfile400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "update profile", slog.Any("error", err))
		return api.UpdateMyProfile500JSONResponse(internal("could not change the profile")), nil
	}
	return api.UpdateMyProfile200JSONResponse(APIProfile(profile)), nil
}

// CompleteMyProfileOnboarding marks the one-time profile step as passed. The
// client calls it for both "save" and "skip", and a retry is fine: the service
// keeps the first timestamp. A guest is refused until it signs up or claims the
// account, because the step belongs to the flow that starts after that.
func (h *Handler) CompleteMyProfileOnboarding(ctx context.Context, _ api.CompleteMyProfileOnboardingRequestObject) (api.CompleteMyProfileOnboardingResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.CompleteMyProfileOnboarding401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	err := h.svc.CompleteOnboarding(ctx, userID)
	switch {
	case errors.Is(err, ErrGuestOnboarding):
		return api.CompleteMyProfileOnboarding403JSONResponse(api.Error(errorBody(httpx.CodeForbidden, err.Error()))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "complete profile onboarding", slog.Any("error", err))
		return api.CompleteMyProfileOnboarding500JSONResponse(internal("could not mark the profile step as passed")), nil
	}
	return api.CompleteMyProfileOnboarding204Response{}, nil
}

func (h *Handler) SetMyStatus(ctx context.Context, req api.SetMyStatusRequestObject) (api.SetMyStatusResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.SetMyStatus401JSONResponse(errorBody(httpx.CodeUnauthorized, "sign in first")), nil
	}
	if req.Body == nil {
		return api.SetMyStatus400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	update := StatusUpdate{Emoji: value(req.Body.Emoji), Text: value(req.Body.Text)}
	if req.Body.ExpiresIn != nil {
		update.ExpiresIn = Expiry(*req.Body.ExpiresIn)
	}

	profile, err := h.svc.SetStatus(ctx, userID, update)
	switch {
	case invalid(err):
		return api.SetMyStatus400JSONResponse{ErrorJSONResponse: badRequest(err.Error())}, nil
	case err != nil:
		h.log.ErrorContext(ctx, "set status", slog.Any("error", err))
		return api.SetMyStatus500JSONResponse(internal("could not set the status")), nil
	}
	return api.SetMyStatus200JSONResponse(APIProfile(profile)), nil
}

func (h *Handler) ClearMyStatus(ctx context.Context, _ api.ClearMyStatusRequestObject) (api.ClearMyStatusResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ClearMyStatus401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	profile, err := h.svc.ClearStatus(ctx, userID)
	if err != nil {
		h.log.ErrorContext(ctx, "clear status", slog.Any("error", err))
		return api.ClearMyStatus500JSONResponse(internal("could not remove the status")), nil
	}
	return api.ClearMyStatus200JSONResponse(APIProfile(profile)), nil
}

func (h *Handler) UploadMyAvatar(ctx context.Context, req api.UploadMyAvatarRequestObject) (api.UploadMyAvatarResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.UploadMyAvatar401JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeUnauthorized, "sign in first")}, nil
	}
	if req.Body == nil {
		return api.UploadMyAvatar400JSONResponse(api.Error(badRequest("a file is required"))), nil
	}

	part, err := req.Body.NextPart()
	if err != nil {
		return api.UploadMyAvatar400JSONResponse(api.Error(badRequest("a file is required"))), nil
	}
	defer func() { _ = part.Close() }()
	if part.FormName() != "file" {
		return api.UploadMyAvatar400JSONResponse(api.Error(badRequest(`the form field must be named "file"`))), nil
	}

	profile, err := h.svc.SetAvatar(ctx, userID, part)
	switch {
	case errors.Is(err, ErrAvatarTooLarge):
		return api.UploadMyAvatar413JSONResponse(api.Error(errorBody(httpx.CodeBodyTooLarge,
			fmt.Sprintf("the image is larger than this server allows (%d bytes)", h.svc.MaxAvatarBytes())))), nil
	case errors.Is(err, ErrAvatarType):
		return api.UploadMyAvatar415JSONResponse(api.Error(errorBody(httpx.CodeInvalidRequest, err.Error()))), nil
	case errors.Is(err, ErrAvatarEmpty), errors.Is(err, ErrAvatarInvalid), errors.Is(err, ErrAvatarDimensions):
		return api.UploadMyAvatar400JSONResponse(api.Error(errorBody(httpx.CodeInvalidRequest, err.Error()))), nil
	case errors.Is(err, ErrAvatarsUnavailable):
		return api.UploadMyAvatar500JSONResponse(internal("avatars are not configured")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "upload avatar", slog.Any("error", err))
		return api.UploadMyAvatar500JSONResponse(internal("could not store the avatar")), nil
	}
	return api.UploadMyAvatar200JSONResponse(APIProfile(profile)), nil
}

func (h *Handler) DeleteMyAvatar(ctx context.Context, _ api.DeleteMyAvatarRequestObject) (api.DeleteMyAvatarResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.DeleteMyAvatar401JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeUnauthorized, "sign in first")}, nil
	}

	profile, err := h.svc.ClearAvatar(ctx, userID)
	if err != nil {
		h.log.ErrorContext(ctx, "delete avatar", slog.Any("error", err))
		return api.DeleteMyAvatar500JSONResponse(internal("could not remove the avatar")), nil
	}
	return api.DeleteMyAvatar200JSONResponse(APIProfile(profile)), nil
}

func (h *Handler) GetUserProfile(ctx context.Context, req api.GetUserProfileRequestObject) (api.GetUserProfileResponseObject, error) {
	viewerID, ok := caller(ctx)
	if !ok {
		return api.GetUserProfile401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	profile, err := h.svc.PublicProfile(ctx, viewerID, req.UserId)
	switch {
	case errors.Is(err, ErrNotFound):
		// A stranger and an unknown id answer the same: the endpoint must not
		// become a way to test which accounts exist.
		return api.GetUserProfile404JSONResponse(notFound()), nil
	case err != nil:
		h.log.ErrorContext(ctx, "get profile", slog.Any("error", err))
		return api.GetUserProfile500JSONResponse(internal("could not read the profile")), nil
	}
	return api.GetUserProfile200JSONResponse(APIProfile(profile)), nil
}

func (h *Handler) GetMutual(ctx context.Context, req api.GetMutualRequestObject) (api.GetMutualResponseObject, error) {
	viewerID, ok := caller(ctx)
	if !ok {
		return api.GetMutual401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}

	mutual, err := h.svc.Mutual(ctx, viewerID, req.UserId)
	switch {
	case errors.Is(err, ErrSelf):
		// A mutual list is about a pair, so asking about yourself is a mistake in
		// the request rather than a hidden answer.
		return api.GetMutual400JSONResponse(api.Error(badRequest(err.Error()))), nil
	case errors.Is(err, ErrNotFound):
		// A stranger, an unknown id and a blocked pair answer the same way.
		return api.GetMutual404JSONResponse(notFound()), nil
	case err != nil:
		h.log.ErrorContext(ctx, "get mutual lists", slog.Any("error", err))
		return api.GetMutual500JSONResponse(internal("could not read the mutual lists")), nil
	}

	response := api.GetMutual200JSONResponse{
		Servers: make([]api.MutualServer, 0, len(mutual.Servers)),
		Friends: make([]api.UserProfile, 0, len(mutual.Friends)),
	}
	for _, server := range mutual.Servers {
		item := api.MutualServer{
			Id:   server.ID,
			Name: server.Name,
			Role: api.WorkspaceRole(server.Role),
		}
		if server.IconURL != "" {
			url := server.IconURL
			item.IconUrl = &url
		}
		response.Servers = append(response.Servers, item)
	}
	for _, friend := range mutual.Friends {
		response.Friends = append(response.Friends, APIProfile(friend))
	}
	return response, nil
}

// invalid reports an error the caller can fix by sending a different value.
func invalid(err error) bool {
	switch {
	case errors.Is(err, ErrNothingToUpdate),
		errors.Is(err, ErrInvalidDisplayName),
		errors.Is(err, ErrInvalidDisplayNameChars),
		errors.Is(err, ErrInvalidBio),
		errors.Is(err, ErrInvalidBioChars),
		errors.Is(err, ErrInvalidAccent),
		errors.Is(err, ErrInvalidStatusEmoji),
		errors.Is(err, ErrInvalidStatusText),
		errors.Is(err, ErrInvalidStatusChars),
		errors.Is(err, ErrEmptyStatus),
		errors.Is(err, ErrInvalidExpiry):
		return true
	}
	return false
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

// notFound answers both "no such account" and "you may not see this profile".
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
