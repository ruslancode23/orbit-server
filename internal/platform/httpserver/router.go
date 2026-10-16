// Package httpserver wires HTTP routing and shared middleware.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/health"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
	"github.com/ruslancode23/orbit-server/internal/voice"
)

// maxBodyBytes caps every request body. Raise it per route when uploads arrive.
const maxBodyBytes = 1 << 10

const voiceTokenPath = "/api/v1/voice/token" //nolint:gosec // a URL path, not a credential

type Deps struct {
	Logger      *slog.Logger
	CORSOrigins []string
	Health      *health.Handler
	// Auth serves accounts and sessions; its middleware resolves the session cookie.
	Auth     *auth.Handler
	Sessions auth.Resolver
	// Voice is nil outside development: the token endpoint has no real authorization yet.
	Voice *voice.Handler
}

// NewRouter serves the API described by api/openapi.yaml. Every request is
// validated against the spec before it reaches a handler.
func NewRouter(d Deps) (http.Handler, error) {
	// A nil auth dependency would silently turn every session check into a panic
	// (or worse, into open access): refuse to build such a router.
	if d.Auth == nil || d.Sessions == nil {
		return nil, errors.New("httpserver: Deps.Auth and Deps.Sessions are required")
	}

	spec, err := api.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("load OpenAPI spec: %w", err)
	}
	spec.Servers = nil // validate paths only, whatever host the server runs on

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(d.Logger))
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   d.CORSOrigins,
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete},
		AllowedHeaders:   []string{"Authorization", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           300,
	}))
	// Before validation: a handler reads the session from the context, and the
	// WebSocket upgrade will need it too.
	r.Use(auth.Middleware(d.Sessions, d.Logger))
	if d.Voice == nil {
		// Hide the dev endpoint before validation, so a production instance
		// does not reveal it through 400 responses.
		r.Use(notFound(voiceTokenPath))
	}
	r.Use(limitBody(maxBodyBytes))
	r.Use(nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
		DoNotValidateServers: true,
		ErrorHandlerWithOpts: validationError,
		Options: openapi3filter.Options{
			// Keep messages short: no schema dump in responses.
			SchemaValidationOptions: []openapi3.SchemaValidationOption{openapi3.DisableSchemaErrorDetails()},
		},
	}))

	strict := api.NewStrictHandlerWithOptions(&server{health: d.Health, auth: d.Auth, voice: d.Voice}, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, err.Error())
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			d.Logger.ErrorContext(r.Context(), "handler failed", slog.Any("error", err))
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		},
	})
	return api.HandlerWithOptions(strict, api.ChiServerOptions{
		BaseRouter: r,
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, err.Error())
		},
	}), nil
}

// server assembles the module handlers into the generated interface.
type server struct {
	health *health.Handler
	auth   *auth.Handler
	voice  *voice.Handler
}

var _ api.StrictServerInterface = (*server)(nil)

func (s *server) GetLiveness(ctx context.Context, req api.GetLivenessRequestObject) (api.GetLivenessResponseObject, error) {
	return s.health.GetLiveness(ctx, req)
}

func (s *server) GetReadiness(ctx context.Context, req api.GetReadinessRequestObject) (api.GetReadinessResponseObject, error) {
	return s.health.GetReadiness(ctx, req)
}

func (s *server) CreateVoiceToken(ctx context.Context, req api.CreateVoiceTokenRequestObject) (api.CreateVoiceTokenResponseObject, error) {
	if s.voice == nil {
		return api.CreateVoiceToken404JSONResponse{Error: api.ErrorDetail{Code: httpx.CodeNotFound, Message: "not found"}}, nil
	}
	return s.voice.CreateVoiceToken(ctx, req)
}

func (s *server) Register(ctx context.Context, req api.RegisterRequestObject) (api.RegisterResponseObject, error) {
	return s.auth.Register(ctx, req)
}

func (s *server) Login(ctx context.Context, req api.LoginRequestObject) (api.LoginResponseObject, error) {
	return s.auth.Login(ctx, req)
}

func (s *server) Logout(ctx context.Context, req api.LogoutRequestObject) (api.LogoutResponseObject, error) {
	return s.auth.Logout(ctx, req)
}

func (s *server) GetCurrentUser(ctx context.Context, req api.GetCurrentUserRequestObject) (api.GetCurrentUserResponseObject, error) {
	return s.auth.GetCurrentUser(ctx, req)
}

func (s *server) ChangePassword(ctx context.Context, req api.ChangePasswordRequestObject) (api.ChangePasswordResponseObject, error) {
	return s.auth.ChangePassword(ctx, req)
}

func validationError(_ context.Context, err error, w http.ResponseWriter, _ *http.Request, opts nethttpmiddleware.ErrorHandlerOpts) {
	switch opts.StatusCode {
	case http.StatusNotFound:
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "not found")
	case http.StatusMethodNotAllowed:
		httpx.WriteError(w, http.StatusMethodNotAllowed, httpx.CodeMethodNotAllowed, "method not allowed")
	default:
		var reqErr *openapi3filter.RequestError
		msg := err.Error()
		if errors.As(err, &reqErr) {
			msg = reqErr.Error()
		}
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, msg)
	}
}

func notFound(path string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == path {
				httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "not found")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func limitBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, httpx.CodeBodyTooLarge,
					fmt.Sprintf("request body must not exceed %d bytes", limit))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			next.ServeHTTP(ww, r)
			log.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}
