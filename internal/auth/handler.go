package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

// Handler serves the generated auth operations. The request body is already
// validated against api/openapi.yaml when a method runs.
type Handler struct {
	svc *Service
	// secureCookies is false only for plain-http local development.
	secureCookies bool
	log           *slog.Logger
}

func NewHandler(svc *Service, secureCookies bool, log *slog.Logger) *Handler {
	return &Handler{svc: svc, secureCookies: secureCookies, log: log}
}

func (h *Handler) Register(ctx context.Context, req api.RegisterRequestObject) (api.RegisterResponseObject, error) {
	if req.Body == nil {
		return api.Register400JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeInvalidRequest, "request body is required")}, nil
	}

	user, creds, err := h.svc.Register(ctx, req.Body.Nickname, email(req.Body.Email), req.Body.Password)
	switch {
	case errors.Is(err, ErrTaken):
		return api.Register409JSONResponse(errorBody(httpx.CodeConflict, "this nickname or email is already taken")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "register", slog.Any("error", err))
		return api.Register500JSONResponse(errorBody(httpx.CodeInternal, "could not create the account")), nil
	}

	cookie := h.sessionCookie(creds)
	return api.Register201JSONResponse{
		Body:    toAPIUser(user),
		Headers: api.Register201ResponseHeaders{SetCookie: &cookie},
	}, nil
}

func (h *Handler) Login(ctx context.Context, req api.LoginRequestObject) (api.LoginResponseObject, error) {
	if req.Body == nil {
		return api.Login400JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeInvalidRequest, "request body is required")}, nil
	}

	user, creds, err := h.svc.Login(ctx, req.Body.Login, req.Body.Password)
	switch {
	case errors.Is(err, ErrCredentials):
		return api.Login401JSONResponse(errorBody(httpx.CodeUnauthorized, "wrong login or password")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "login", slog.Any("error", err))
		return api.Login500JSONResponse(errorBody(httpx.CodeInternal, "could not sign in")), nil
	}

	cookie := h.sessionCookie(creds)
	return api.Login200JSONResponse{
		Body:    toAPIUser(user),
		Headers: api.Login200ResponseHeaders{SetCookie: &cookie},
	}, nil
}

func (h *Handler) Logout(ctx context.Context, _ api.LogoutRequestObject) (api.LogoutResponseObject, error) {
	session, ok := SessionFrom(ctx)
	if !ok {
		return api.Logout401JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeUnauthorized, "sign in first")}, nil
	}

	if err := h.svc.Logout(ctx, session.ID); err != nil {
		h.log.ErrorContext(ctx, "logout", slog.Any("error", err))
		return api.Logout500JSONResponse(errorBody(httpx.CodeInternal, "could not sign out")), nil
	}

	cookie := h.clearedCookie()
	return api.Logout204Response{Headers: api.Logout204ResponseHeaders{SetCookie: &cookie}}, nil
}

func (h *Handler) GetCurrentUser(ctx context.Context, _ api.GetCurrentUserRequestObject) (api.GetCurrentUserResponseObject, error) {
	session, ok := SessionFrom(ctx)
	if !ok {
		return api.GetCurrentUser401JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeUnauthorized, "sign in first")}, nil
	}
	return api.GetCurrentUser200JSONResponse(toAPIUser(session.User)), nil
}

func (h *Handler) ChangePassword(ctx context.Context, req api.ChangePasswordRequestObject) (api.ChangePasswordResponseObject, error) {
	session, ok := SessionFrom(ctx)
	if !ok {
		return api.ChangePassword401JSONResponse(errorBody(httpx.CodeUnauthorized, "sign in first")), nil
	}
	if req.Body == nil {
		return api.ChangePassword400JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeInvalidRequest, "request body is required")}, nil
	}

	err := h.svc.ChangePassword(ctx, session.User.ID, req.Body.CurrentPassword, req.Body.NewPassword)
	switch {
	case errors.Is(err, ErrCredentials):
		return api.ChangePassword401JSONResponse(errorBody(httpx.CodeUnauthorized, "the current password is wrong")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "change password", slog.Any("error", err))
		return api.ChangePassword500JSONResponse(errorBody(httpx.CodeInternal, "could not change the password")), nil
	}

	// Every session is gone, this one too: the client signs in again.
	cookie := h.clearedCookie()
	return api.ChangePassword204Response{Headers: api.ChangePassword204ResponseHeaders{SetCookie: &cookie}}, nil
}

// The Secure flag follows h.secureCookies: local development runs on plain http,
// where the browser would never send a Secure cookie.
func (h *Handler) sessionCookie(creds Credentials) string {
	return (&http.Cookie{ //nolint:gosec // Secure is set from configuration, see above
		Name:     SessionCookieName,
		Value:    creds.Token,
		Path:     "/",
		Expires:  creds.ExpiresAt,
		HttpOnly: true,
		Secure:   h.secureCookies,
		// Lax, not Strict: the browser must send the cookie when the user arrives
		// by an invite link from another site (FR-03).
		SameSite: http.SameSiteLaxMode,
	}).String()
}

func (h *Handler) clearedCookie() string {
	return (&http.Cookie{ //nolint:gosec // Secure is set from configuration, see sessionCookie
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
	}).String()
}

func toAPIUser(u User) api.User {
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

func email(e *openapi_types.Email) string {
	if e == nil {
		return ""
	}
	return string(*e)
}

func errorBody(code, message string) api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{Code: code, Message: message}}
}
