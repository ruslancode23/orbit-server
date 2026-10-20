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
	"github.com/ruslancode23/orbit-server/internal/friends"
	"github.com/ruslancode23/orbit-server/internal/gateway"
	"github.com/ruslancode23/orbit-server/internal/health"
	"github.com/ruslancode23/orbit-server/internal/invites"
	"github.com/ruslancode23/orbit-server/internal/messages"
	"github.com/ruslancode23/orbit-server/internal/platform/httpserver"
	"github.com/ruslancode23/orbit-server/internal/platform/logger"
	"github.com/ruslancode23/orbit-server/internal/platform/objectstore"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/presence"
	"github.com/ruslancode23/orbit-server/internal/privacy"
	"github.com/ruslancode23/orbit-server/internal/profile"
	"github.com/ruslancode23/orbit-server/internal/ratelimit"
	"github.com/ruslancode23/orbit-server/internal/reads"
	"github.com/ruslancode23/orbit-server/internal/secrets"
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

	// Who may write to whom: the block list, the dm policy and the guest rule.
	privacyService := privacy.NewService(privacy.NewPostgresStore(queries))
	// Friendships live in their own module. Privacy asks them two questions —
	// whether two accounts are friends, and what to withdraw when one blocks the
	// other — so neither package imports the other's rules.
	friendService := friends.NewService(friends.NewPostgresStore(queries), privacyService, friends.Limits{
		MaxFriends: cfg.MaxFriends,
		MaxPending: cfg.FriendRequestPendingLimit,
	})
	friendService.SetLimiter(ratelimit.NewDaily(cfg.FriendRequestDailyLimit))
	friendService.SetPublisher(hub)
	privacyService.SetFriendship(friendService)
	privacyService.SetBlockCleanup(friendService)

	// New conversations and the first message of a dialogue share one bucket
	// per account; opening a dialogue and its opener are one action to guard.
	limiter := ratelimit.New(cfg.NewConversationLimit)

	conversationService := conversations.NewService(conversations.NewPostgresStore(pool), cfg.MaxGroupMembers, privacyService)
	conversationService.SetLimiter(limiter)
	// A new dialogue is announced on each participant's own stream.
	conversationService.SetPublisher(hub)
	// A conversation is a channel without a server, so the channels module asks
	// this one who may read it.
	channelService.SetParticipants(conversationService)

	// Display names, bios and custom statuses. The module owns the profile
	// columns; servers and conversations only tell it who already knows whom.
	profileService := profile.NewService(profile.NewPostgresStore(queries), hub, log)
	profileService.SetServers(workspaces)
	profileService.SetConversations(conversationService)
	// A friend sees the profile of a friend: the friendship is what makes the
	// two strangers readable to each other.
	profileService.SetFriends(friendService)
	// Avatars are re-encoded into WebP before they are stored, so their links can
	// be rendered inline and cached for long.
	profileService.SetAvatars(profile.Avatars{
		Storage:  storage,
		MaxBytes: cfg.Storage.MaxAvatarBytes,
		LinkTTL:  cfg.Storage.AvatarLinkTTL,
	})
	workspaces.SetProfiles(profileService)
	conversationService.SetProfiles(profileService)

	messageService := messages.NewService(messages.NewPostgresStore(queries), channelService, workspaces, nil, hub)
	messageService.SetProfiles(profileService)
	fileService := files.NewService(files.NewPostgresStore(queries), storage, channelService, messageService,
		workspaces, cfg.Storage.MaxUploadBytes, cfg.Storage.LinkTTL)
	// The two know each other: a message carries files, a file needs its channel.
	messageService.SetFiles(files.NewForMessages(fileService))
	// Messages in a conversation go to its participants, not to a server.
	messageService.SetConversations(conversationService)
	// History and live group events hide the authors the reader blocked.
	messageService.SetPrivacy(privacyService)
	messageService.SetLimiter(limiter)

	// Secret chats: public keys and ciphertext only. The server never has the
	// key to open an envelope.
	secretService := secrets.NewService(secrets.NewPostgresStore(pool), conversationService, hub)
	sessions := auth.NewService(auth.NewPostgresStore(queries), cfg.SessionTTL)

	// A nickname is an identity change: it confirms the password in internal/auth
	// and then asks the profile module to announce the account like any other
	// profile change. Two renames per hour is the account-wide budget.
	sessions.SetNicknameLimiter(ratelimit.NewHourly(cfg.NicknameChangeHourlyLimit))

	authHandler := auth.NewHandler(sessions, !cfg.IsDevelopment(), log)
	authHandler.SetProfiles(profileService)
	authHandler.SetProfileEvents(profileService)
	inviteService := invites.NewService(invites.NewPostgresStore(queries), workspaces, sessions)

	workspaceHandler := servers.NewHandler(workspaces, log)

	// Who is online in text. Like voice presence it lives in this process: a
	// restart costs one reconnect, and the clients announce themselves again.
	userPresence := presence.NewService(presence.NewMemoryStore(), workspaces, hub, log)
	// A friend sees an account's status in the personal stream, and the friend
	// list shows the same status next to the name.
	userPresence.SetFriends(friendService)
	friendService.SetPresence(userPresence)
	workspaceHandler.SetPresence(userPresence)
	gatewayHandler := gateway.NewHandler(hub, workspaces, messageService, cfg.CORSOrigins, log)
	gatewayHandler.SetPresence(userPresence)

	var voiceWebhook http.Handler
	var presence *voice.Presence
	roomClient, err := voice.NewRooms(cfg.LiveKit.URL, cfg.LiveKit.APIKey, cfg.LiveKit.APISecret)
	if err != nil {
		log.Warn("voice presence disabled: could not build LiveKit room client", slog.Any("error", err))
	} else {
		presence = voice.NewPresence(channelService, roomClient, hub, log)
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
		Secrets:        secrets.NewHandler(secretService, log),
		Privacy:        privacy.NewHandler(privacyService, log),
		Friends:        friends.NewHandler(friendService, log),
		Profiles:       profile.NewHandler(profileService, log),
		MaxUploadBytes: cfg.Storage.MaxUploadBytes,
		MaxIconBytes:   cfg.Storage.MaxIconBytes,
		MaxAvatarBytes: cfg.Storage.MaxAvatarBytes,
		Reads: reads.NewHandler(
			reads.NewService(reads.NewPostgresStore(queries), channelService, workspaces, hub), log),
		Sessions:   sessions,
		Workspaces: workspaceHandler,
		Gateway:    gatewayHandler,
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
	// Envelopes nobody picked up before their deadline are dropped.
	go secrets.NewSweeper(secretService, log).Run(ctx)
	if presence != nil {
		go presence.Run(ctx)
	}
	// Statuses are published from this loop: the debounce and the offline grace
	// need a clock that is not a request.
	go userPresence.Run(ctx)
	// Profile changes wait out their own debounce in this loop, for the same reason.
	go profileService.Run(ctx)

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
