package auth

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/ruslancode23/orbit-server/internal/api"
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
