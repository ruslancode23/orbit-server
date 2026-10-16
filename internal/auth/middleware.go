package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

// contextKey keeps the session out of reach of other packages' keys.
type contextKey struct{}

// WithSession puts a resolved session into the context.
func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}

// SessionFrom returns the session of the request, if the middleware resolved one.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(contextKey{}).(Session)
	return s, ok
}

// Resolver turns a session token into a session. *Service implements it.
type Resolver interface {
	Authenticate(ctx context.Context, token string) (Session, error)
}

// Middleware attaches the session of the cookie to the request context when the
// token is live. It never rejects a request on its own: public endpoints stay
// public, and a handler that needs a session answers 401 itself (see RequireSession).
func Middleware(resolver Resolver, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(SessionCookieName)
			if err != nil || cookie.Value == "" {
				next.ServeHTTP(w, r)
				return
			}

			session, err := resolver.Authenticate(r.Context(), cookie.Value)
			switch {
			case err == nil:
				r = r.WithContext(WithSession(r.Context(), session))
			case errors.Is(err, ErrNoSession):
				// Expired, revoked or unknown: the request goes on without a session.
			default:
				log.ErrorContext(r.Context(), "resolve session", slog.Any("error", err))
				httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireSession guards anything outside the generated handlers — the WebSocket
// upgrade first of all, where the browser can send a cookie but not a header.
func RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := SessionFrom(r.Context()); !ok {
			httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "sign in first")
			return
		}
		next.ServeHTTP(w, r)
	})
}
