package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

// Profiles is the public part of an account, owned by internal/profile. It
// answers in the generated shape rather than the module's own type: the profile
// handler reads sessions from this package, so this package must not import it
// back. One call serves a whole list, which is how /auth/me stays one query.
type Profiles interface {
	OwnProfiles(ctx context.Context, ids []openapi_types.UUID) (map[openapi_types.UUID]OwnProfile, error)
}

// OwnProfile is one account's profile as its owner sees it: the public part and
// the data only the owner may read. OnboardedAt is when the one-time "fill in
// your profile" step was passed; nil means the client still has to show it.
type OwnProfile struct {
	Profile     api.UserProfile
	OnboardedAt *time.Time
}

// ProfileEvents is the profile module's announcement scheduler. A nickname
// change is a profile change, so it must reach the same audience as a new
// avatar or bio; this package asks for the event instead of publishing one.
type ProfileEvents interface {
	ProfileChanged(ctx context.Context, userID uuid.UUID)
}

// Handler serves the generated auth operations. The request body is already
// validated against api/openapi.yaml when a method runs.
type Handler struct {
	svc *Service
	// secureCookies is false only for plain-http local development.
	secureCookies bool
	log           *slog.Logger
	// profiles is optional at construction; main wires it in.
	profiles Profiles
	// profileEvents is optional too: without it a change simply is not announced.
	profileEvents ProfileEvents
}

func NewHandler(svc *Service, secureCookies bool, log *slog.Logger) *Handler {
	return &Handler{svc: svc, secureCookies: secureCookies, log: log}
}

// SetProfiles wires the profile module in after both exist.
func (h *Handler) SetProfiles(profiles Profiles) { h.profiles = profiles }

// SetProfileEvents wires in the announcement scheduler of the profile module.
func (h *Handler) SetProfileEvents(events ProfileEvents) { h.profileEvents = events }

func (h *Handler) Register(ctx context.Context, req api.RegisterRequestObject) (api.RegisterResponseObject, error) {
	if req.Body == nil {
		return api.Register400JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeInvalidRequest, "request body is required")}, nil
	}

	user, creds, err := h.svc.Register(ctx, req.Body.Nickname, email(req.Body.Email), req.Body.Password)
	switch {
	case errors.Is(err, ErrNicknameReserved):
		return api.Register409JSONResponse(errorBody(httpx.CodeConflict, "this nickname was released recently and is not available yet")), nil
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

	if h.profiles != nil {
		profiles, err := h.profiles.OwnProfiles(ctx, []openapi_types.UUID{session.User.ID})
		if err != nil {
			h.log.ErrorContext(ctx, "read own profile", slog.Any("error", err))
			return api.GetCurrentUser500JSONResponse(errorBody(httpx.CodeInternal, "could not read the account")), nil
		}
		if profile, ok := profiles[session.User.ID]; ok {
			return api.GetCurrentUser200JSONResponse(selfProfile(profile, session.User.Email)), nil
		}
	}
	// Without the profile module the account itself is still known: answer the
	// auth part, with the nickname standing in for the display name.
	return api.GetCurrentUser200JSONResponse(selfProfile(
		OwnProfile{Profile: toAPIProfile(session.User)}, session.User.Email)), nil
}

// selfProfile is the answer about oneself: the public profile plus the email,
// which nobody else may read, and the onboarding flag, which only the owner
// needs. A guest has no address, so the field is null.
func selfProfile(own OwnProfile, email string) api.SelfProfile {
	profile := own.Profile
	self := api.SelfProfile{
		AccentColor:        profile.AccentColor,
		AvatarUrl:          profile.AvatarUrl,
		Bio:                profile.Bio,
		CreatedAt:          profile.CreatedAt,
		CustomStatus:       profile.CustomStatus,
		DisplayName:        profile.DisplayName,
		Id:                 profile.Id,
		IsGuest:            profile.IsGuest,
		Nickname:           profile.Nickname,
		ProfileOnboardedAt: own.OnboardedAt,
	}
	if email != "" {
		address := openapi_types.Email(email)
		self.Email = &address
	}
	return self
}

// toAPIProfile is the fallback shape of /auth/me when the profile module is not
// wired in. Production always wires it.
func toAPIProfile(u User) api.UserProfile {
	return api.UserProfile{
		Id:          u.ID,
		Nickname:    u.Nickname,
		DisplayName: u.Nickname,
		AccentColor: api.Slate,
		Bio:         "",
		IsGuest:     u.IsGuest,
		CreatedAt:   u.CreatedAt,
	}
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

// UpdateMyNickname changes the unique handle. The current password is required,
// and the answer carries the new profile so the client does not refetch it.
func (h *Handler) UpdateMyNickname(ctx context.Context, req api.UpdateMyNicknameRequestObject) (api.UpdateMyNicknameResponseObject, error) {
	session, ok := SessionFrom(ctx)
	if !ok {
		return api.UpdateMyNickname401JSONResponse(api.Error(errorBody(httpx.CodeUnauthorized, "sign in first"))), nil
	}
	if req.Body == nil {
		return api.UpdateMyNickname400JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeInvalidRequest, "request body is required")}, nil
	}

	user, err := h.svc.ChangeNickname(ctx, session.User.ID, req.Body.CurrentPassword, req.Body.Nickname)
	var limited *RateLimitedError
	switch {
	case errors.As(err, &limited):
		seconds := int((limited.RetryAfter + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		return api.UpdateMyNickname429JSONResponse{TooManyRequestsJSONResponse: api.TooManyRequestsJSONResponse{
			Body:    api.Error(errorBody(httpx.CodeTooManyRequests, "too many nickname changes, try again later")),
			Headers: api.TooManyRequestsResponseHeaders{RetryAfter: &seconds},
		}}, nil
	case errors.Is(err, ErrNoPassword):
		return api.UpdateMyNickname403JSONResponse(api.Error(errorBody(httpx.CodeForbidden,
			"a guest has no password to confirm this change; sign up first"))), nil
	case errors.Is(err, ErrCredentials):
		return api.UpdateMyNickname401JSONResponse(api.Error(errorBody(httpx.CodeUnauthorized, "the current password is wrong"))), nil
	case errors.Is(err, ErrInvalidNickname):
		return api.UpdateMyNickname400JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeInvalidRequest, err.Error())}, nil
	case errors.Is(err, ErrNicknameReserved):
		return api.UpdateMyNickname409JSONResponse(api.Error(errorBody(httpx.CodeConflict,
			"this nickname was released recently and is not available yet"))), nil
	case errors.Is(err, ErrTaken):
		return api.UpdateMyNickname409JSONResponse(api.Error(errorBody(httpx.CodeConflict,
			"this nickname is already taken"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "change nickname", slog.Any("error", err))
		return api.UpdateMyNickname500JSONResponse(api.Error(errorBody(httpx.CodeInternal, "could not change the nickname"))), nil
	}

	h.changed(ctx, user.ID)
	self, err := h.selfProfileFor(ctx, user)
	if err != nil {
		h.log.ErrorContext(ctx, "read own profile after rename", slog.Any("error", err))
		return api.UpdateMyNickname500JSONResponse(api.Error(errorBody(httpx.CodeInternal, "could not read the account"))), nil
	}
	return api.UpdateMyNickname200JSONResponse(self), nil
}

// UpdateMyEmail changes or clears the address. No confirmation mail is sent
// yet, so the store marks the address unverified.
func (h *Handler) UpdateMyEmail(ctx context.Context, req api.UpdateMyEmailRequestObject) (api.UpdateMyEmailResponseObject, error) {
	session, ok := SessionFrom(ctx)
	if !ok {
		return api.UpdateMyEmail401JSONResponse(api.Error(errorBody(httpx.CodeUnauthorized, "sign in first"))), nil
	}
	if req.Body == nil {
		return api.UpdateMyEmail400JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeInvalidRequest, "request body is required")}, nil
	}

	user, err := h.svc.ChangeEmail(ctx, session.User.ID, req.Body.CurrentPassword, req.Body.Email)
	switch {
	case errors.Is(err, ErrNoPassword):
		return api.UpdateMyEmail403JSONResponse(api.Error(errorBody(httpx.CodeForbidden,
			"a guest has no password to confirm this change; sign up first"))), nil
	case errors.Is(err, ErrCredentials):
		return api.UpdateMyEmail401JSONResponse(api.Error(errorBody(httpx.CodeUnauthorized, "the current password is wrong"))), nil
	case errors.Is(err, ErrInvalidEmail):
		return api.UpdateMyEmail400JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeInvalidRequest, err.Error())}, nil
	case errors.Is(err, ErrTaken):
		return api.UpdateMyEmail409JSONResponse(api.Error(errorBody(httpx.CodeConflict, "this email is already used by another account"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "change email", slog.Any("error", err))
		return api.UpdateMyEmail500JSONResponse(api.Error(errorBody(httpx.CodeInternal, "could not change the email"))), nil
	}

	h.changed(ctx, user.ID)
	self, err := h.selfProfileFor(ctx, user)
	if err != nil {
		h.log.ErrorContext(ctx, "read own profile after email change", slog.Any("error", err))
		return api.UpdateMyEmail500JSONResponse(api.Error(errorBody(httpx.CodeInternal, "could not read the account"))), nil
	}
	return api.UpdateMyEmail200JSONResponse(self), nil
}

// selfProfileFor rebuilds the self view around an account that just changed:
// the public profile comes from the profile module, the email from the account.
func (h *Handler) selfProfileFor(ctx context.Context, user User) (api.SelfProfile, error) {
	if h.profiles != nil {
		profiles, err := h.profiles.OwnProfiles(ctx, []openapi_types.UUID{user.ID})
		if err != nil {
			return api.SelfProfile{}, err
		}
		if profile, ok := profiles[user.ID]; ok {
			return selfProfile(profile, user.Email), nil
		}
	}
	return selfProfile(OwnProfile{Profile: toAPIProfile(user)}, user.Email), nil
}

// changed asks the profile module to announce the account once the debounce
// passes. A nil hook means the change is simply not announced live.
func (h *Handler) changed(ctx context.Context, userID uuid.UUID) {
	if h.profileEvents != nil {
		h.profileEvents.ProfileChanged(ctx, userID)
	}
}

// The Secure flag follows h.secureCookies: local development runs on plain http,
// where the browser would never send a Secure cookie.
// SessionCookie is the Set-Cookie value for a new session. Other modules that
// start a session — the guest entrance, for one — use it instead of spelling
// the flags out again.
func (h *Handler) SessionCookie(creds Credentials) string {
	return h.sessionCookie(creds)
}

// ClaimGuestAccount turns the guest of this session into a full account. The
// session stays alive: the person signed up, they did not sign out.
func (h *Handler) ClaimGuestAccount(ctx context.Context, req api.ClaimGuestAccountRequestObject) (api.ClaimGuestAccountResponseObject, error) {
	session, ok := SessionFrom(ctx)
	if !ok {
		return api.ClaimGuestAccount401JSONResponse(errorBody(httpx.CodeUnauthorized, "sign in first")), nil
	}
	if req.Body == nil {
		return api.ClaimGuestAccount400JSONResponse{ErrorJSONResponse: errorBody(httpx.CodeInvalidRequest, "request body is required")}, nil
	}

	user, err := h.svc.ClaimGuest(ctx, session.User.ID, req.Body.Nickname, email(req.Body.Email), req.Body.Password)
	switch {
	case errors.Is(err, ErrNotGuest):
		return api.ClaimGuestAccount403JSONResponse(errorBody(httpx.CodeForbidden, "this account already has a password")), nil
	case errors.Is(err, ErrNicknameReserved):
		return api.ClaimGuestAccount409JSONResponse(errorBody(httpx.CodeConflict, "this nickname was released recently and is not available yet")), nil
	case errors.Is(err, ErrTaken):
		return api.ClaimGuestAccount409JSONResponse(errorBody(httpx.CodeConflict, "this nickname or email is already taken")), nil
	case err != nil:
		h.log.ErrorContext(ctx, "claim guest account", slog.Any("error", err))
		return api.ClaimGuestAccount500JSONResponse(errorBody(httpx.CodeInternal, "could not finish the sign-up")), nil
	}
	return api.ClaimGuestAccount200JSONResponse(toAPIUser(user)), nil
}

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
