package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

type fakeResolver struct {
	session Session
	err     error
	calls   int
}

func (r *fakeResolver) Authenticate(context.Context, string) (Session, error) {
	r.calls++
	return r.session, r.err
}

// probe records whether the middleware attached a session.
func probe(seen *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := SessionFrom(r.Context())
		*seen = ok
		w.WriteHeader(http.StatusOK)
	})
}

func request(t *testing.T, h http.Handler, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/auth/me", nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookie})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMiddlewareAttachesLiveSession(t *testing.T) {
	want := Session{ID: uuid.New(), User: User{ID: uuid.New(), Nickname: "Anna"}}
	resolver := &fakeResolver{session: want}
	var seen bool
	h := Middleware(resolver, discardLogger())(probe(&seen))

	rec := request(t, h, "a-token")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !seen {
		t.Error("the handler got no session")
	}
	if resolver.calls != 1 {
		t.Errorf("resolver calls = %d, want 1", resolver.calls)
	}
}

func TestMiddlewareWithoutCookieDoesNotResolve(t *testing.T) {
	resolver := &fakeResolver{}
	var seen bool
	h := Middleware(resolver, discardLogger())(probe(&seen))

	rec := request(t, h, "")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: public endpoints stay public", rec.Code)
	}
	if seen {
		t.Error("a session appeared without a cookie")
	}
	if resolver.calls != 0 {
		t.Errorf("resolver calls = %d, want 0", resolver.calls)
	}
}

// An expired or revoked token is the same as none: the request goes on without a
// session, and anything that needs one answers 401.
func TestMiddlewareIgnoresDeadSession(t *testing.T) {
	resolver := &fakeResolver{err: ErrNoSession}
	var seen bool
	h := Middleware(resolver, discardLogger())(probe(&seen))

	rec := request(t, h, "an-old-token")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if seen {
		t.Error("a dead token produced a session")
	}
}

func TestMiddlewareFailsClosedOnStoreError(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("database is down")}
	var seen bool
	h := Middleware(resolver, discardLogger())(probe(&seen))

	rec := request(t, h, "a-token")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if seen {
		t.Error("the request reached the handler although the session could not be checked")
	}
}

func TestRequireSession(t *testing.T) {
	guarded := RequireSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("without a session", func(t *testing.T) {
		rec := request(t, Middleware(&fakeResolver{err: ErrNoSession}, discardLogger())(guarded), "dead-token")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("with a session", func(t *testing.T) {
		resolver := &fakeResolver{session: Session{ID: uuid.New()}}
		rec := request(t, Middleware(resolver, discardLogger())(guarded), "live-token")
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
}
