// Command orbit runs the Orbit API server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/config"
	"github.com/ruslancode23/orbit-server/internal/health"
	"github.com/ruslancode23/orbit-server/internal/platform/httpserver"
	"github.com/ruslancode23/orbit-server/internal/platform/logger"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/servers"
	"github.com/ruslancode23/orbit-server/internal/voice"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "orbit:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	log := logger.New(cfg.IsDevelopment())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	queries := db.New(pool)
	sessions := auth.NewService(auth.NewPostgresStore(queries), cfg.SessionTTL)

	deps := httpserver.Deps{
		Logger:      log,
		CORSOrigins: cfg.CORSOrigins,
		Health:      health.NewHandler(pool),
		// Cookies go over TLS everywhere but local development.
		Auth:       auth.NewHandler(sessions, !cfg.IsDevelopment(), log),
		Sessions:   sessions,
		Workspaces: servers.NewHandler(servers.NewService(servers.NewPostgresStore(pool)), log),
	}
	if cfg.IsDevelopment() {
		issuer := voice.NewTokenIssuer(cfg.LiveKit.APIKey, cfg.LiveKit.APISecret, cfg.LiveKit.PublicURL)
		deps.Voice = voice.NewHandler(issuer, log)
		log.Warn("dev voice token endpoint enabled: POST /api/v1/voice/token has no authorization")
	}

	router, err := httpserver.NewRouter(deps)
	if err != nil {
		return fmt.Errorf("build router: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("http server started", slog.String("addr", cfg.HTTPAddr), slog.String("env", cfg.Env))
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
