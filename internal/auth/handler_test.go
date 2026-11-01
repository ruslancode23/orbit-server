package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/ruslancode23/orbit-server/internal/api"
	orbitmail "github.com/ruslancode23/orbit-server/internal/email"
	"github.com/ruslancode23/orbit-server/internal/platform/clientip"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

func newTestHandler(t *testing.T, secureCookies bool) (*Handler, *Service, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	svc, _ := newTestService(t)
	return NewHandler(svc, secureCookies, log), svc, logs
}

// parseCookie reads the Set-Cookie value the handler returned.
func parseCookie(t *testing.T, header *string) *http.Cookie {
	t.Helper()
	if header == nil {
		t.Fatal("no Set-Cookie header")
	}
	resp := http.Response{Header: http.Header{"Set-Cookie": []string{*header}}}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cookies))
	}
	return cookies[0]
}

// Every test here registers the same person; only the email and password vary.
func registerBody(email string) *api.RegisterJSONRequestBody {
	body := api.RegisterJSONRequestBody{Nickname: "Anna", Password: testPassword}
	if email != "" {
		mail := openapi_types.Email(email)
		body.Email = &mail
	}
	return &body
}

func TestRegisterSetsHttpOnlySessionCookie(t *testing.T) {
	h, svc, _ := newTestHandler(t, true)

	resp, err := h.Register(context.Background(), api.RegisterRequestObject{
		Body: registerBody("anna@example.com"),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	created, ok := resp.(api.Register201JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 201", resp)
	}
	if created.Body.Nickname != "Anna" || created.Body.Email == nil {
		t.Errorf("body = %+v, want the nickname and email back", created.Body)
	}

	cookie := parseCookie(t, created.Headers.SetCookie)
	if cookie.Name != SessionCookieName {
		t.Errorf("cookie name = %q, want %q", cookie.Name, SessionCookieName)
	}
	if !cookie.HttpOnly {
		t.Error("cookie is not HttpOnly: a script could read the session")
	}
	if !cookie.Secure {
		t.Error("cookie is not Secure although the server asked for secure cookies")
	}
	// Lax, so the cookie survives arriving by an invite link from another site.
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if !cookie.Expires.After(time.Now()) {
		t.Errorf("Expires = %v, want a moment in the future", cookie.Expires)
	}

	// The cookie really carries a working session.
	if _, err := svc.Authenticate(context.Background(), cookie.Value); err != nil {
		t.Errorf("the cookie value is not a live session: %v", err)
	}
}

// Local development runs on plain http, where a Secure cookie would never be sent.
func TestRegisterCookieNotSecureInDevelopment(t *testing.T) {
	h, _, _ := newTestHandler(t, false)

	resp, err := h.Register(context.Background(), api.RegisterRequestObject{
		Body: registerBody(""),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	cookie := parseCookie(t, resp.(api.Register201JSONResponse).Headers.SetCookie)
	if cookie.Secure {
		t.Error("cookie is Secure in development: the browser would not send it over http")
	}
}

func TestRegisterRejectsTakenNickname(t *testing.T) {
	h, _, _ := newTestHandler(t, true)
	body := registerBody("anna@example.com")
	if _, err := h.Register(context.Background(), api.RegisterRequestObject{Body: body}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	resp, err := h.Register(context.Background(), api.RegisterRequestObject{Body: body})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	conflict, ok := resp.(api.Register409JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 409", resp)
	}
	if conflict.Error.Code != httpx.CodeConflict {
		t.Errorf("code = %q, want %q", conflict.Error.Code, httpx.CodeConflict)
	}
}

func TestLoginWrongPasswordIsUnauthorizedAndNotLogged(t *testing.T) {
	h, _, logs := newTestHandler(t, true)
	if _, err := h.Register(context.Background(), api.RegisterRequestObject{
		Body: registerBody(""),
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	const attempted = "hunter2-is-my-password"
	resp, err := h.Login(context.Background(), api.LoginRequestObject{
		Body: &api.LoginJSONRequestBody{Login: "Anna", Password: attempted},
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	unauthorized, ok := resp.(api.Login401JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 401", resp)
	}
	if unauthorized.Error.Code != httpx.CodeUnauthorized {
		t.Errorf("code = %q, want %q", unauthorized.Error.Code, httpx.CodeUnauthorized)
	}
	// The message must not say whether it was the login or the password that was wrong.
	if strings.Contains(strings.ToLower(unauthorized.Error.Message), "password is") {
		t.Errorf("message = %q, it tells which half was wrong", unauthorized.Error.Message)
	}
	for _, secret := range []string{attempted, testPassword} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("a password reached the log: %q", logs.String())
		}
	}
}

func TestSessionlessRequestsAreUnauthorized(t *testing.T) {
	h, _, _ := newTestHandler(t, true)
	ctx := context.Background()

	t.Run("me", func(t *testing.T) {
		resp, err := h.GetCurrentUser(ctx, api.GetCurrentUserRequestObject{})
		if err != nil {
			t.Fatalf("GetCurrentUser: %v", err)
		}
		if _, ok := resp.(api.GetCurrentUser401JSONResponse); !ok {
			t.Errorf("response = %T, want 401", resp)
		}
	})

	t.Run("logout", func(t *testing.T) {
		resp, err := h.Logout(ctx, api.LogoutRequestObject{})
		if err != nil {
			t.Fatalf("Logout: %v", err)
		}
		if _, ok := resp.(api.Logout401JSONResponse); !ok {
			t.Errorf("response = %T, want 401", resp)
		}
	})

	t.Run("change password", func(t *testing.T) {
		resp, err := h.ChangePassword(ctx, api.ChangePasswordRequestObject{
			Body: &api.ChangePasswordJSONRequestBody{CurrentPassword: testPassword, NewPassword: "another password"},
		})
		if err != nil {
			t.Fatalf("ChangePassword: %v", err)
		}
		if _, ok := resp.(api.ChangePassword401JSONResponse); !ok {
			t.Errorf("response = %T, want 401", resp)
		}
	})
}

func TestMeReturnsTheSessionAccount(t *testing.T) {
	h, svc, _ := newTestHandler(t, true)
	user, creds := registerTestUser(t, svc)
	session, err := svc.Authenticate(context.Background(), creds.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	resp, err := h.GetCurrentUser(WithSession(context.Background(), session), api.GetCurrentUserRequestObject{})
	if err != nil {
		t.Fatalf("GetCurrentUser: %v", err)
	}

	me, ok := resp.(api.GetCurrentUser200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", resp)
	}
	if user.ID != me.Id {
		t.Errorf("id = %v, want %v", me.Id, user.ID)
	}
}

// The answer about oneself carries the email; nobody else's profile does.
func TestMeCarriesTheEmailAndAGuestHasNone(t *testing.T) {
	h, svc, _ := newTestHandler(t, true)

	user, creds := registerTestUser(t, svc)
	session, err := svc.Authenticate(context.Background(), creds.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	resp, err := h.GetCurrentUser(WithSession(context.Background(), session), api.GetCurrentUserRequestObject{})
	if err != nil {
		t.Fatalf("GetCurrentUser: %v", err)
	}
	me, ok := resp.(api.GetCurrentUser200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", resp)
	}
	if me.Email == nil || string(*me.Email) != user.Email {
		t.Fatalf("email = %v, want %q", me.Email, user.Email)
	}

	guest, guestCreds, err := svc.RegisterGuest(context.Background(), "Guest")
	if err != nil {
		t.Fatalf("RegisterGuest: %v", err)
	}
	guestSession, err := svc.Authenticate(context.Background(), guestCreds.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	resp, err = h.GetCurrentUser(WithSession(context.Background(), guestSession), api.GetCurrentUserRequestObject{})
	if err != nil {
		t.Fatalf("GetCurrentUser: %v", err)
	}
	guestMe, ok := resp.(api.GetCurrentUser200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", resp)
	}
	if guestMe.Email != nil {
		t.Fatalf("guest email = %v, want null", *guestMe.Email)
	}
	if guestMe.Id != guest.ID || guestMe.DisplayName != guest.Nickname {
		t.Fatalf("guest = %+v, want %v / %q", guestMe, guest.ID, guest.Nickname)
	}
}

// /auth/me says whether the one-time profile step is passed, so a client on
// another device does not show it again. A new account answers null.
func TestMeCarriesTheProfileOnboardingFlag(t *testing.T) {
	h, svc, _ := newTestHandler(t, false)
	user, creds := registerTestUser(t, svc)
	session, err := svc.Authenticate(context.Background(), creds.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	ctx := WithSession(context.Background(), session)

	profiles := &fakeProfiles{byID: map[uuid.UUID]OwnProfile{
		user.ID: {Profile: api.UserProfile{
			Id:          user.ID,
			Nickname:    user.Nickname,
			DisplayName: user.Nickname,
			AccentColor: api.Slate,
			IsGuest:     user.IsGuest,
			CreatedAt:   user.CreatedAt,
		}},
	}}
	h.SetProfiles(profiles)

	resp, err := h.GetCurrentUser(ctx, api.GetCurrentUserRequestObject{})
	if err != nil {
		t.Fatalf("GetCurrentUser: %v", err)
	}
	me, ok := resp.(api.GetCurrentUser200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", resp)
	}
	if me.ProfileOnboardedAt != nil {
		t.Fatalf("profileOnboardedAt = %v, want nil on a new account", me.ProfileOnboardedAt)
	}

	onboardedAt := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	own := profiles.byID[user.ID]
	own.OnboardedAt = &onboardedAt
	profiles.byID[user.ID] = own

	resp, err = h.GetCurrentUser(ctx, api.GetCurrentUserRequestObject{})
	if err != nil {
		t.Fatalf("GetCurrentUser: %v", err)
	}
	me, ok = resp.(api.GetCurrentUser200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", resp)
	}
	if me.ProfileOnboardedAt == nil || !me.ProfileOnboardedAt.Equal(onboardedAt) {
		t.Errorf("profileOnboardedAt = %v, want %v", me.ProfileOnboardedAt, onboardedAt)
	}
}

func TestLogoutAndPasswordChangeClearTheCookie(t *testing.T) {
	h, svc, _ := newTestHandler(t, true)
	user, creds := registerTestUser(t, svc)
	session, err := svc.Authenticate(context.Background(), creds.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	ctx := WithSession(context.Background(), session)

	t.Run("logout", func(t *testing.T) {
		resp, err := h.Logout(ctx, api.LogoutRequestObject{})
		if err != nil {
			t.Fatalf("Logout: %v", err)
		}
		done, ok := resp.(api.Logout204Response)
		if !ok {
			t.Fatalf("response = %T, want 204", resp)
		}
		cookie := parseCookie(t, done.Headers.SetCookie)
		if cookie.Value != "" || cookie.MaxAge >= 0 {
			t.Errorf("cookie = %+v, want it cleared", cookie)
		}
		if _, err := svc.Authenticate(ctx, creds.Token); err == nil {
			t.Error("the session still works after logout")
		}
	})

	t.Run("password change", func(t *testing.T) {
		_, fresh, err := svc.Login(t.Context(), "Anna", testPassword)
		if err != nil {
			t.Fatalf("Login: %v", err)
		}
		live, err := svc.Authenticate(t.Context(), fresh.Token)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}

		resp, err := h.ChangePassword(WithSession(t.Context(), live), api.ChangePasswordRequestObject{
			Body: &api.ChangePasswordJSONRequestBody{CurrentPassword: testPassword, NewPassword: "a brand new password"},
		})
		if err != nil {
			t.Fatalf("ChangePassword: %v", err)
		}
		done, ok := resp.(api.ChangePassword204Response)
		if !ok {
			t.Fatalf("response = %T, want 204", resp)
		}
		cookie := parseCookie(t, done.Headers.SetCookie)
		if cookie.Value != "" || cookie.MaxAge >= 0 {
			t.Errorf("cookie = %+v, want it cleared", cookie)
		}
		if _, err := svc.Authenticate(t.Context(), fresh.Token); err == nil {
			t.Error("the session survived the password change")
		}
		_ = user
	})
}

func TestChangePasswordWrongCurrentIsUnauthorized(t *testing.T) {
	h, svc, _ := newTestHandler(t, true)
	_, creds := registerTestUser(t, svc)
	session, err := svc.Authenticate(context.Background(), creds.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	resp, err := h.ChangePassword(WithSession(context.Background(), session), api.ChangePasswordRequestObject{
		Body: &api.ChangePasswordJSONRequestBody{CurrentPassword: "not my password", NewPassword: "a brand new password"},
	})
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if _, ok := resp.(api.ChangePassword401JSONResponse); !ok {
		t.Fatalf("response = %T, want 401", resp)
	}
	if _, err := svc.Authenticate(context.Background(), creds.Token); err != nil {
		t.Errorf("a failed change revoked the session: %v", err)
	}
}

// fakeProfiles stands in for internal/profile: the handler asks for the owner's
// view, which also carries the onboarding timestamp.
type fakeProfiles struct {
	byID map[uuid.UUID]OwnProfile
}

func (f *fakeProfiles) OwnProfiles(_ context.Context, ids []openapi_types.UUID) (map[openapi_types.UUID]OwnProfile, error) {
	out := make(map[openapi_types.UUID]OwnProfile, len(ids))
	for _, id := range ids {
		if profile, ok := f.byID[id]; ok {
			out[id] = profile
		}
	}
	return out, nil
}

type fakeProfileEvents struct {
	users []uuid.UUID
}

func (f *fakeProfileEvents) ProfileChanged(_ context.Context, userID uuid.UUID) {
	f.users = append(f.users, userID)
}

func sessionContext(user User) context.Context {
	return WithSession(context.Background(), Session{ID: uuid.New(), User: user})
}

func TestUpdateMyNickname(t *testing.T) {
	h, svc, _ := newTestHandler(t, false)
	events := &fakeProfileEvents{}
	h.SetProfileEvents(events)
	user, _, err := svc.Register(t.Context(), "Anna", "anna@example.com", testPassword)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	resp, err := h.UpdateMyNickname(sessionContext(user), api.UpdateMyNicknameRequestObject{
		Body: &api.NicknameUpdate{Nickname: "Bob", CurrentPassword: testPassword},
	})
	if err != nil {
		t.Fatalf("UpdateMyNickname: %v", err)
	}
	updated, ok := resp.(api.UpdateMyNickname200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", resp)
	}
	if updated.Nickname != "Bob" || updated.Id != user.ID {
		t.Errorf("profile = %+v, want the account with the new nickname", updated)
	}
	if updated.Email == nil || string(*updated.Email) != "anna@example.com" {
		t.Errorf("email = %v, want the unchanged address", updated.Email)
	}
	if len(events.users) != 1 || events.users[0] != user.ID {
		t.Errorf("announcements = %v, want one for the account", events.users)
	}
}

func TestUpdateMyNicknameRejectsWrongPasswordAndDuplicates(t *testing.T) {
	h, svc, _ := newTestHandler(t, false)
	anna, _, err := svc.Register(t.Context(), "Anna", "anna@example.com", testPassword)
	if err != nil {
		t.Fatalf("Register Anna: %v", err)
	}
	if _, _, err := svc.Register(t.Context(), "Bob", "bob@example.com", testPassword); err != nil {
		t.Fatalf("Register Bob: %v", err)
	}

	wrong, err := h.UpdateMyNickname(sessionContext(anna), api.UpdateMyNicknameRequestObject{
		Body: &api.NicknameUpdate{Nickname: "Vika", CurrentPassword: "not my password"},
	})
	if err != nil {
		t.Fatalf("UpdateMyNickname: %v", err)
	}
	if _, ok := wrong.(api.UpdateMyNickname401JSONResponse); !ok {
		t.Errorf("response = %T, want 401", wrong)
	}

	taken, err := h.UpdateMyNickname(sessionContext(anna), api.UpdateMyNicknameRequestObject{
		Body: &api.NicknameUpdate{Nickname: "bob", CurrentPassword: testPassword},
	})
	if err != nil {
		t.Fatalf("UpdateMyNickname: %v", err)
	}
	if _, ok := taken.(api.UpdateMyNickname409JSONResponse); !ok {
		t.Errorf("response = %T, want 409", taken)
	}
}

func TestUpdateMyNicknameIsRateLimited(t *testing.T) {
	h, svc, _ := newTestHandler(t, false)
	svc.SetNicknameLimiter(ratelimit.NewHourly(1))
	user, _, err := svc.Register(t.Context(), "Anna", "anna@example.com", testPassword)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := h.UpdateMyNickname(sessionContext(user), api.UpdateMyNicknameRequestObject{
		Body: &api.NicknameUpdate{Nickname: "Bob", CurrentPassword: testPassword},
	}); err != nil {
		t.Fatalf("UpdateMyNickname: %v", err)
	}

	resp, err := h.UpdateMyNickname(sessionContext(user), api.UpdateMyNicknameRequestObject{
		Body: &api.NicknameUpdate{Nickname: "Vika", CurrentPassword: testPassword},
	})
	if err != nil {
		t.Fatalf("UpdateMyNickname: %v", err)
	}
	limited, ok := resp.(api.UpdateMyNickname429JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 429", resp)
	}
	if limited.Headers.RetryAfter == nil || *limited.Headers.RetryAfter < 1 {
		t.Error("the 429 has no Retry-After in seconds")
	}
}

func TestUpdateMyEmail(t *testing.T) {
	h, svc, _ := newTestHandler(t, false)
	events := &fakeProfileEvents{}
	h.SetProfileEvents(events)
	user, _, err := svc.Register(t.Context(), "Anna", "anna@example.com", testPassword)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	resp, err := h.UpdateMyEmail(sessionContext(user), api.UpdateMyEmailRequestObject{
		Body: &api.EmailUpdate{Email: "new@example.com", CurrentPassword: testPassword},
	})
	if err != nil {
		t.Fatalf("UpdateMyEmail: %v", err)
	}
	updated, ok := resp.(api.UpdateMyEmail200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", resp)
	}
	if updated.Email == nil || string(*updated.Email) != "new@example.com" {
		t.Errorf("email = %v, want the new address", updated.Email)
	}

	cleared, err := h.UpdateMyEmail(sessionContext(user), api.UpdateMyEmailRequestObject{
		Body: &api.EmailUpdate{Email: "", CurrentPassword: testPassword},
	})
	if err != nil {
		t.Fatalf("UpdateMyEmail: %v", err)
	}
	empty, ok := cleared.(api.UpdateMyEmail200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", cleared)
	}
	if empty.Email != nil {
		t.Errorf("email = %v, want it cleared", empty.Email)
	}
	if len(events.users) != 2 {
		t.Errorf("announcements = %v, want one per change", events.users)
	}
}

func TestGuestCannotChangeIdentityThroughTheHandler(t *testing.T) {
	h, svc, _ := newTestHandler(t, false)
	guest, _, err := svc.RegisterGuest(t.Context(), "Guest")
	if err != nil {
		t.Fatalf("RegisterGuest: %v", err)
	}

	nickname, err := h.UpdateMyNickname(sessionContext(guest), api.UpdateMyNicknameRequestObject{
		Body: &api.NicknameUpdate{Nickname: "Bob", CurrentPassword: ""},
	})
	if err != nil {
		t.Fatalf("UpdateMyNickname: %v", err)
	}
	if _, ok := nickname.(api.UpdateMyNickname403JSONResponse); !ok {
		t.Errorf("nickname response = %T, want 403", nickname)
	}

	email, err := h.UpdateMyEmail(sessionContext(guest), api.UpdateMyEmailRequestObject{
		Body: &api.EmailUpdate{Email: "guest@example.com", CurrentPassword: ""},
	})
	if err != nil {
		t.Fatalf("UpdateMyEmail: %v", err)
	}
	if _, ok := email.(api.UpdateMyEmail403JSONResponse); !ok {
		t.Errorf("email response = %T, want 403", email)
	}
}

func fromAddr(addr string) context.Context {
	return clientip.WithAddr(context.Background(), addr)
}

func loginAs(t *testing.T, h *Handler, ctx context.Context, login, password string) api.LoginResponseObject {
	t.Helper()
	resp, err := h.Login(ctx, api.LoginRequestObject{
		Body: &api.LoginJSONRequestBody{Login: login, Password: password},
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	return resp
}

func retryAfter(t *testing.T, resp api.TooManyRequestsJSONResponse) int {
	t.Helper()
	if resp.Headers.RetryAfter == nil || *resp.Headers.RetryAfter < 1 {
		t.Fatalf("Retry-After = %v, want a positive number of seconds", resp.Headers.RetryAfter)
	}
	return *resp.Headers.RetryAfter
}

// Wrong passwords for one name run out its budget whatever address they come
// from, and the owner's right password is refused too until the wait is over:
// otherwise a guesser would learn the password by the answer changing. A name
// nobody owns answers exactly the same way.
func TestLoginFailuresPerNameAcrossAddresses(t *testing.T) {
	h, _, _ := newTestHandler(t, true)
	h.SetLimits(Limits{LoginFailures: ratelimit.NewHourly(2)})
	if _, err := h.Register(context.Background(), api.RegisterRequestObject{Body: registerBody("")}); err != nil {
		t.Fatal(err)
	}

	for i, addr := range []string{"203.0.113.1", "203.0.113.2"} {
		if _, ok := loginAs(t, h, fromAddr(addr), "anna", "wrong-password").(api.Login401JSONResponse); !ok {
			t.Fatalf("attempt %d was not a plain 401", i+1)
		}
	}
	limited, ok := loginAs(t, h, fromAddr("203.0.113.3"), "ANNA", testPassword).(api.Login429JSONResponse)
	if !ok {
		t.Fatal("the right password went through a spent budget")
	}
	retryAfter(t, limited.TooManyRequestsJSONResponse)

	for range 2 {
		loginAs(t, h, fromAddr("203.0.113.1"), "ghost", "whatever")
	}
	ghost, ok := loginAs(t, h, fromAddr("203.0.113.1"), "ghost", "whatever").(api.Login429JSONResponse)
	if !ok || ghost.Body.Error.Message != limited.Body.Error.Message {
		t.Errorf("an unknown name answered differently: %#v", ghost)
	}
}

// Signing in successfully spends nothing from the failure budget, so the owner
// can come and go as often as they like.
func TestLoginSuccessDoesNotSpendTheFailureBudget(t *testing.T) {
	h, _, _ := newTestHandler(t, true)
	h.SetLimits(Limits{LoginFailures: ratelimit.NewHourly(1)})
	if _, err := h.Register(context.Background(), api.RegisterRequestObject{Body: registerBody("")}); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if _, ok := loginAs(t, h, fromAddr("203.0.113.1"), "anna", testPassword).(api.Login200JSONResponse); !ok {
			t.Fatalf("sign-in %d was refused", i+1)
		}
	}
}

// One address may only try so often, right password or not; another address
// is not affected.
func TestLoginAttemptsPerAddress(t *testing.T) {
	h, _, _ := newTestHandler(t, true)
	h.SetLimits(Limits{LoginPerIP: ratelimit.New(2)})
	if _, err := h.Register(context.Background(), api.RegisterRequestObject{Body: registerBody("")}); err != nil {
		t.Fatal(err)
	}
	loginAs(t, h, fromAddr("203.0.113.1"), "anna", testPassword)
	loginAs(t, h, fromAddr("203.0.113.1"), "anna", testPassword)
	if _, ok := loginAs(t, h, fromAddr("203.0.113.1"), "anna", testPassword).(api.Login429JSONResponse); !ok {
		t.Error("a third attempt in a minute passed a limit of two")
	}
	if _, ok := loginAs(t, h, fromAddr("203.0.113.2"), "anna", testPassword).(api.Login200JSONResponse); !ok {
		t.Error("another address was limited too")
	}
}

func TestRegisterPerAddress(t *testing.T) {
	h, _, _ := newTestHandler(t, true)
	h.SetLimits(Limits{Signups: ratelimit.NewHourly(1)})

	register := func(addr, nickname string) api.RegisterResponseObject {
		t.Helper()
		resp, err := h.Register(fromAddr(addr), api.RegisterRequestObject{
			Body: &api.RegisterJSONRequestBody{Nickname: nickname, Password: testPassword},
		})
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		return resp
	}
	if _, ok := register("203.0.113.1", "Anna").(api.Register201JSONResponse); !ok {
		t.Fatal("the first account was refused")
	}
	limited, ok := register("203.0.113.1", "Bob").(api.Register429JSONResponse)
	if !ok {
		t.Fatal("a second account from the same address passed a limit of one")
	}
	if seconds := retryAfter(t, limited.TooManyRequestsJSONResponse); seconds > 3600 {
		t.Errorf("Retry-After = %d, want at most an hour", seconds)
	}
	if _, ok := register("203.0.113.2", "Bob").(api.Register201JSONResponse); !ok {
		t.Error("another address was limited too")
	}
}

// fakeMailer records what would go out and can pretend SMTP is off.
type fakeMailer struct {
	enabled bool
	sendErr error
	mu      sync.Mutex
	sent    []orbitmail.Message
}

func (m *fakeMailer) Send(_ context.Context, msg orbitmail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return m.sendErr
}

func (m *fakeMailer) Enabled() bool { return m.enabled }

// letters is a safe copy: the sends happen in goroutines.
func (m *fakeMailer) letters() []orbitmail.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]orbitmail.Message(nil), m.sent...)
}

// resetTokenFromLetter digs the raw token out of the link the way a person
// would: the letter is the only place it exists.
func resetTokenFromLetter(t *testing.T, msg orbitmail.Message) string {
	t.Helper()
	const marker = "/reset?token="
	at := strings.Index(msg.Text, marker)
	if at < 0 {
		t.Fatalf("no reset link in the letter: %q", msg.Text)
	}
	rest := msg.Text[at+len(marker):]
	if end := strings.IndexAny(rest, " \r\n"); end >= 0 {
		rest = rest[:end]
	}
	if rest == "" {
		t.Fatal("the reset link has no token")
	}
	return rest
}

func resetRequest(t *testing.T, h *Handler, login string) api.RequestPasswordResetResponseObject {
	t.Helper()
	resp, err := h.RequestPasswordReset(context.Background(), api.RequestPasswordResetRequestObject{
		Body: &api.PasswordResetRequest{Login: login},
	})
	if err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	return resp
}

// The answer must not tell an existing account from an unknown one, an account
// with an email from one without it, or a working SMTP from an absent one.
func TestPasswordResetRequestIsTheSameForEveryAccount(t *testing.T) {
	h, svc, _ := newTestHandler(t, true)
	mailer := &fakeMailer{enabled: true}
	h.SetPasswordReset(mailer, "https://orbit.example", time.Hour, ResetLimits{})

	registerTestUser(t, svc)
	if _, _, err := svc.Register(t.Context(), "NoMail", "", testPassword); err != nil {
		t.Fatalf("Register without email: %v", err)
	}

	for _, login := range []string{"Anna", "anna@example.com", "NoMail", "nobody"} {
		if _, ok := resetRequest(t, h, login).(api.RequestPasswordReset204Response); !ok {
			t.Errorf("login %q did not answer 204", login)
		}
	}
	h.Wait()

	// Two of the four names are the same account (nickname and email), so it
	// gets two letters; the other two get none.
	letters := mailer.letters()
	if len(letters) != 2 {
		t.Fatalf("letters = %d, want only the account with an email", len(letters))
	}
	for _, letter := range letters {
		if letter.To != "anna@example.com" {
			t.Errorf("letter went to %q", letter.To)
		}
		if !strings.Contains(letter.Text, "https://orbit.example/reset?token=") {
			t.Errorf("the link is not absolute: %q", letter.Text)
		}
	}
}

// Without SMTP the endpoint still answers 204, but no token is even issued:
// there is no link to put in the database.
func TestPasswordResetRequestWithoutSMTPIsSilent(t *testing.T) {
	h, svc, _ := newTestHandler(t, true)
	mailer := &fakeMailer{enabled: false}
	h.SetPasswordReset(mailer, "https://orbit.example", time.Hour, ResetLimits{})
	registerTestUser(t, svc)

	if _, ok := resetRequest(t, h, "Anna").(api.RequestPasswordReset204Response); !ok {
		t.Error("the disabled-mail answer is not 204")
	}
	h.Wait()

	if letters := mailer.letters(); len(letters) != 0 {
		t.Errorf("a letter was sent without SMTP: %+v", letters)
	}
	store := svc.store.(*fakeStore)
	if len(store.resets) != 0 {
		t.Errorf("a reset token was stored without SMTP: %d", len(store.resets))
	}
}

func TestPasswordResetTokenIsOneTimeAndRevokesSessions(t *testing.T) {
	h, svc, _ := newTestHandler(t, true)
	mailer := &fakeMailer{enabled: true}
	h.SetPasswordReset(mailer, "https://orbit.example", time.Hour, ResetLimits{})

	user, first := registerTestUser(t, svc)
	_, second, err := svc.Login(t.Context(), "Anna", testPassword)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	resetRequest(t, h, "Anna")
	h.Wait()
	token := resetTokenFromLetter(t, mailer.letters()[0])

	resp, err := h.ResetPassword(context.Background(), api.ResetPasswordRequestObject{
		Body: &api.PasswordResetConfirmRequest{Token: token, Password: "a brand new password"},
	})
	if err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	done, ok := resp.(api.ResetPassword204Response)
	if !ok {
		t.Fatalf("response = %T, want 204", resp)
	}
	cookie := parseCookie(t, done.Headers.SetCookie)
	if cookie.Name != SessionCookieName || cookie.MaxAge >= 0 {
		t.Errorf("cookie = %+v, want the session cookie cleared", cookie)
	}

	for name, credentials := range map[string]Credentials{"this device": first, "the other device": second} {
		if _, err := svc.Authenticate(t.Context(), credentials.Token); !errors.Is(err, ErrNoSession) {
			t.Errorf("%s: err = %v, want ErrNoSession", name, err)
		}
	}
	if _, _, err := svc.Login(t.Context(), "Anna", testPassword); !errors.Is(err, ErrCredentials) {
		t.Errorf("the old password still works: %v", err)
	}
	if _, _, err := svc.Login(t.Context(), "Anna", "a brand new password"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
	if got, err := svc.store.UserByID(t.Context(), user.ID); err != nil || got.Email != "anna@example.com" {
		t.Errorf("the account was disturbed: %+v, %v", got, err)
	}

	again, err := h.ResetPassword(context.Background(), api.ResetPasswordRequestObject{
		Body: &api.PasswordResetConfirmRequest{Token: token, Password: "yet another password"},
	})
	if err != nil {
		t.Fatalf("ResetPassword again: %v", err)
	}
	if _, ok := again.(api.ResetPassword400JSONResponse); !ok {
		t.Errorf("a second use answered %T, want 400", again)
	}
}

func TestExpiredPasswordResetTokenIsRefused(t *testing.T) {
	h, svc, _ := newTestHandler(t, true)
	mailer := &fakeMailer{enabled: true}
	h.SetPasswordReset(mailer, "https://orbit.example", time.Hour, ResetLimits{})
	registerTestUser(t, svc)

	resetRequest(t, h, "Anna")
	h.Wait()
	token := resetTokenFromLetter(t, mailer.letters()[0])

	// The fake store's clock is the service's too: moving it expires the token.
	store := svc.store.(*fakeStore)
	store.now = func() time.Time { return time.Now().Add(2 * time.Hour) }

	resp, err := h.ResetPassword(context.Background(), api.ResetPasswordRequestObject{
		Body: &api.PasswordResetConfirmRequest{Token: token, Password: "a brand new password"},
	})
	if err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	refused, ok := resp.(api.ResetPassword400JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 400", resp)
	}
	if !strings.Contains(refused.Error.Message, "invalid or has expired") {
		t.Errorf("message = %q, want the generic refusal", refused.Error.Message)
	}
}

// fakeAvatarCleaner stands in for the profile module's half of account deletion.
type fakeAvatarCleaner struct {
	prepared bool
	cleaned  bool
	fail     error
}

func (f *fakeAvatarCleaner) PrepareAvatarDeletion(context.Context, uuid.UUID) (func(context.Context), error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.prepared = true
	return func(context.Context) { f.cleaned = true }, nil
}

type cleaningProfiles struct {
	*fakeProfiles
	cleaner *fakeAvatarCleaner
}

func (p *cleaningProfiles) PrepareAvatarDeletion(ctx context.Context, userID uuid.UUID) (func(context.Context), error) {
	return p.cleaner.PrepareAvatarDeletion(ctx, userID)
}

func deleteRequest(t *testing.T, h *Handler, ctx context.Context, password string) api.DeleteCurrentUserResponseObject {
	t.Helper()
	req := api.DeleteCurrentUserRequestObject{}
	if password != "" {
		req.Body = &api.DeleteCurrentUserJSONRequestBody{Password: &password}
	}
	resp, err := h.DeleteCurrentUser(ctx, req)
	if err != nil {
		t.Fatalf("DeleteCurrentUser: %v", err)
	}
	return resp
}

func TestDeleteCurrentUserNeedsThePasswordAndRemovesTheAccount(t *testing.T) {
	h, svc, _ := newTestHandler(t, true)
	cleaner := &fakeAvatarCleaner{}
	h.SetProfiles(&cleaningProfiles{fakeProfiles: &fakeProfiles{}, cleaner: cleaner})

	user, creds := registerTestUser(t, svc)
	session, err := svc.Authenticate(t.Context(), creds.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	ctx := WithSession(context.Background(), session)

	if _, ok := deleteRequest(t, h, ctx, "not my password").(api.DeleteCurrentUser401JSONResponse); !ok {
		t.Error("a wrong password was not refused")
	}
	if cleaner.cleaned {
		t.Error("the avatar was deleted before the account")
	}
	if _, err := svc.store.UserByID(t.Context(), user.ID); err != nil {
		t.Fatalf("a wrong password removed the account: %v", err)
	}

	resp := deleteRequest(t, h, ctx, testPassword)
	done, ok := resp.(api.DeleteCurrentUser204Response)
	if !ok {
		t.Fatalf("response = %T, want 204", resp)
	}
	cookie := parseCookie(t, done.Headers.SetCookie)
	if cookie.Name != SessionCookieName || cookie.MaxAge >= 0 {
		t.Errorf("cookie = %+v, want the session cookie cleared", cookie)
	}
	if !cleaner.cleaned {
		t.Error("the avatar objects were not removed after the account")
	}
	if _, err := svc.store.UserByID(t.Context(), user.ID); !errors.Is(err, ErrNoUser) {
		t.Errorf("the account is still there: %v", err)
	}
	if _, err := svc.Authenticate(t.Context(), creds.Token); !errors.Is(err, ErrNoSession) {
		t.Errorf("the session survived the account: %v", err)
	}
}

// A guest has no password, so its session is the only confirmation; the delete
// must not demand a credential that cannot exist.
func TestGuestDeletesItsAccountWithTheSessionAlone(t *testing.T) {
	h, svc, _ := newTestHandler(t, true)
	guest, creds, err := svc.RegisterGuest(t.Context(), "Guest")
	if err != nil {
		t.Fatalf("RegisterGuest: %v", err)
	}
	session, err := svc.Authenticate(t.Context(), creds.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	resp := deleteRequest(t, h, WithSession(context.Background(), session), "")
	if _, ok := resp.(api.DeleteCurrentUser204Response); !ok {
		t.Fatalf("response = %T, want 204", resp)
	}
	if _, err := svc.store.UserByID(t.Context(), guest.ID); !errors.Is(err, ErrNoUser) {
		t.Errorf("the guest is still there: %v", err)
	}
}

// A server owner cannot delete the account: the server would lose its owner,
// and deleting the server here would silently destroy a community. The avatar
// must survive the refusal too.
func TestDeleteCurrentUserRefusesAServerOwner(t *testing.T) {
	h, svc, _ := newTestHandler(t, true)
	cleaner := &fakeAvatarCleaner{}
	h.SetProfiles(&cleaningProfiles{fakeProfiles: &fakeProfiles{}, cleaner: cleaner})
	user, creds := registerTestUser(t, svc)
	svc.store.(*fakeStore).deleteErr = ErrOwnsServers
	session, err := svc.Authenticate(t.Context(), creds.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	resp := deleteRequest(t, h, WithSession(context.Background(), session), testPassword)
	if _, ok := resp.(api.DeleteCurrentUser409JSONResponse); !ok {
		t.Fatalf("response = %T, want 409", resp)
	}
	if cleaner.cleaned {
		t.Error("the avatar was removed although the account stayed")
	}
	if _, err := svc.store.UserByID(t.Context(), user.ID); err != nil {
		t.Errorf("the account is gone after a refusal: %v", err)
	}
}
