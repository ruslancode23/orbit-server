package notifysettings

import (
	"context"
	"errors"
	"log/slog"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

// Handler serves the notification levels of the signed-in account.
type Handler struct {
	svc *Service
	log *slog.Logger
}

// NewHandler returns the HTTP front of the service.
func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) ListNotificationSettings(ctx context.Context, _ api.ListNotificationSettingsRequestObject) (api.ListNotificationSettingsResponseObject, error) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return api.ListNotificationSettings401JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeUnauthorized, "sign in first")}, nil
	}
	rows, err := h.svc.List(ctx, session.User.ID)
	if err != nil {
		h.log.ErrorContext(ctx, "list notification settings", slog.Any("error", err))
		return api.ListNotificationSettings500JSONResponse(api.Error(errorBody(httpx.CodeInternal, "could not read the settings"))), nil
	}
	items := make([]api.NotificationSetting, 0, len(rows))
	for _, row := range rows {
		items = append(items, settingResponse(row))
	}
	return api.ListNotificationSettings200JSONResponse(api.NotificationSettings{Items: items}), nil
}

func (h *Handler) SetNotificationSetting(ctx context.Context, req api.SetNotificationSettingRequestObject) (api.SetNotificationSettingResponseObject, error) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return api.SetNotificationSetting401JSONResponse(api.Error(errorBody(httpx.CodeUnauthorized, "sign in first"))), nil
	}
	if req.Body == nil {
		return api.SetNotificationSetting400JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeInvalidRequest, "send a level, mutedUntil, or both as null")}, nil
	}
	scope := Scope(req.Scope)
	var level *Level
	// `inherit` is the API's word for a row without a level of its own: the
	// database keeps NULL, and the channel takes its server's level.
	if req.Body.Level != api.NotificationSettingUpdateLevelInherit {
		converted := Level(req.Body.Level)
		level = &converted
	}
	stored, err := h.svc.Set(ctx, session.User.ID, scope, req.TargetId, level, req.Body.MutedUntil)
	switch {
	case errors.Is(err, ErrInvalidScope):
		return api.SetNotificationSetting400JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeInvalidRequest, "scope: workspace, channel or conversation")}, nil
	case errors.Is(err, ErrInvalidLevel):
		return api.SetNotificationSetting400JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeInvalidRequest, "level: all, mentions or none")}, nil
	case errors.Is(err, ErrNotFound):
		return api.SetNotificationSetting404JSONResponse(api.Error(errorBody(httpx.CodeNotFound, "no such server, channel or conversation"))), nil
	case errors.Is(err, ErrLimit):
		return api.SetNotificationSetting409JSONResponse(api.Error(errorBody(httpx.CodeConflict, "too many notification settings; drop one first"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "set notification setting", slog.Any("error", err))
		return api.SetNotificationSetting500JSONResponse(api.Error(errorBody(httpx.CodeInternal, "could not save the setting"))), nil
	}
	return api.SetNotificationSetting200JSONResponse(settingResponse(stored)), nil
}

func (h *Handler) DeleteNotificationSetting(ctx context.Context, req api.DeleteNotificationSettingRequestObject) (api.DeleteNotificationSettingResponseObject, error) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return api.DeleteNotificationSetting401JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeUnauthorized, "sign in first")}, nil
	}
	err := h.svc.Delete(ctx, session.User.ID, Scope(req.Scope), req.TargetId)
	switch {
	case errors.Is(err, ErrInvalidScope):
		return api.DeleteNotificationSetting404JSONResponse(api.Error(errorBody(httpx.CodeNotFound, "no such setting"))), nil
	case errors.Is(err, ErrNotFound):
		return api.DeleteNotificationSetting404JSONResponse(api.Error(errorBody(httpx.CodeNotFound, "no such setting"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "delete notification setting", slog.Any("error", err))
		return api.DeleteNotificationSetting500JSONResponse(api.Error(errorBody(httpx.CodeInternal, "could not remove the setting"))), nil
	}
	return api.DeleteNotificationSetting204Response{}, nil
}

func settingResponse(row Setting) api.NotificationSetting {
	out := api.NotificationSetting{
		Scope:      api.NotificationScope(row.Scope),
		TargetId:   row.TargetID,
		MutedUntil: row.MutedUntil,
		UpdatedAt:  row.UpdatedAt,
	}
	out.Level = api.NotificationSettingLevelInherit
	if row.Level != nil {
		out.Level = api.NotificationSettingLevel(*row.Level)
	}
	return out
}

func errorBody(code, message string) api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{Code: code, Message: message}}
}
