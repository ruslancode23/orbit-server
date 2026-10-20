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
	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/config"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/files"
	"github.com/ruslancode23/orbit-server/internal/gateway"
	"github.com/ruslancode23/orbit-server/internal/health"
	"github.com/ruslancode23/orbit-server/internal/invites"
	"github.com/ruslancode23/orbit-server/internal/messages"
	"github.com/ruslancode23/orbit-server/internal/platform/httpserver"
	"github.com/ruslancode23/orbit-server/internal/platform/logger"
	"github.com/ruslancode23/orbit-server/internal/platform/objectstore"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/reads"
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
	hub := events.NewHub()
	storage, err := objectstore.New(ctx, cfg.Storage.Endpoint, cfg.Storage.Bucket,
		cfg.Storage.AccessKey, cfg.Storage.SecretKey)
	if err != nil {
		return fmt.Errorf("connect object store: %w", err)
	}
	workspaces := servers.NewService(servers.NewPostgresStore(pool), hub, servers.Icons{
		Storage: storage, MaxBytes: cfg.Storage.MaxIconBytes, LinkTTL: cfg.Storage.IconLinkTTL,
	})
	channelService := channels.NewService(channels.NewPostgresStore(pool), workspaces)

	conversationService := conversations.NewService(conversations.NewPostgresStore(pool), cfg.MaxGroupMembers)
	// A conversation is a channel without a server, so the channels module asks
	// this one who may read it.
	channelService.SetParticipants(conversationService)

	messageService := messages.NewService(messages.NewPostgresStore(queries), channelService, workspaces, nil, hub)
	fileService := files.NewService(files.NewPostgresStore(queries), storage, channelService, messageService,
		workspaces, cfg.Storage.MaxUploadBytes, cfg.Storage.LinkTTL)
	// The two know each other: a message carries files, a file needs its channel.
	messageService.SetFiles(files.NewForMessages(fileService))
	sessions := auth.NewService(auth.NewPostgresStore(queries), cfg.SessionTTL)

	authHandler := auth.NewHandler(sessions, !cfg.IsDevelopment(), log)
	inviteService := invites.NewService(invites.NewPostgresStore(queries), workspaces, sessions)

	workspaceHandler := servers.NewHandler(workspaces, log)
	var voiceWebhook http.Handler
	roomClient, err := voice.NewRooms(cfg.LiveKit.URL, cfg.LiveKit.APIKey, cfg.LiveKit.APISecret)
	if err != nil {
		log.Warn("voice presence disabled: could not build LiveKit room client", slog.Any("error", err))
	} else {
		presence := voice.NewPresence(channelService, roomClient, hub, log)
		workspaceHandler.SetVoicePresence(presence)
		voiceWebhook = voice.NewWebhook(cfg.LiveKit.APIKey, cfg.LiveKit.APISecret, presence, log)
	}

	deps := httpserver.Deps{
		Logger:      log,
		CORSOrigins: cfg.CORSOrigins,
		Health:      health.NewHandler(pool),
		// Cookies go over TLS everywhere but local development.
		Auth:           authHandler,
		Invites:        invites.NewHandler(inviteService, invites.NewCookies(authHandler), log),
		Files:          files.NewHandler(fileService, log),
		Conversations:  conversations.NewHandler(conversationService, log),
		MaxUploadBytes: cfg.Storage.MaxUploadBytes,
		MaxIconBytes:   cfg.Storage.MaxIconBytes,
		Reads: reads.NewHandler(
			reads.NewService(reads.NewPostgresStore(queries), channelService, workspaces, hub), log),
		Sessions:   sessions,
		Workspaces: workspaceHandler,
		Gateway:    gateway.NewHandler(hub, workspaces, messageService, cfg.CORSOrigins, log),
		Channels:   channels.NewHandler(channelService, log),
		Messages: messages.NewHandler(
			messageService, log),
		Voice: voice.NewHandler(
			voice.NewTokenIssuer(cfg.LiveKit.APIKey, cfg.LiveKit.APISecret, cfg.LiveKit.PublicURL),
			channelService, log),
		VoiceWebhook: voiceWebhook,
	}

	// Files whose message never arrived are swept in the background.
	go files.NewSweeper(fileService, cfg.Storage.OrphanAge, log).Run(ctx)

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
