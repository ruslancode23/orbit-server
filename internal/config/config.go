// Package config loads server configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env             string // "development" or "production"
	HTTPAddr        string
	ShutdownTimeout time.Duration
	DatabaseURL     string
	CORSOrigins     []string
	SessionTTL      time.Duration
	// MaxGroupMembers caps a group chat, counting its creator.
	MaxGroupMembers int
	// NewConversationLimit caps how many new conversations one account may open
	// per minute. Zero turns the guard off.
	NewConversationLimit int
	// MaxFriends caps accepted friendships per account. A zero value would make
	// the list unlimited, but Load rejects that: production always has a cap.
	MaxFriends int
	// FriendRequestDailyLimit caps friendship requests per account per day;
	// zero turns the guard off.
	FriendRequestDailyLimit int
	// FriendRequestPendingLimit caps outgoing requests that are still waiting
	// for an answer; zero turns the guard off.
	FriendRequestPendingLimit int
	LiveKit                   LiveKit
	Storage                   Storage
}

// Storage is the object store for attachments (SeaweedFS locally, S3 elsewhere).
type Storage struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	// MaxUploadBytes caps one attachment.
	MaxUploadBytes int64
	// LinkTTL is how long a download link stays valid.
	LinkTTL time.Duration
	// OrphanAge is how long a file without a message is kept before the sweep.
	OrphanAge time.Duration
	// MaxIconBytes caps one server icon.
	MaxIconBytes int64
	// IconLinkTTL is how long the signed link to a server icon stays valid.
	IconLinkTTL time.Duration
}

type LiveKit struct {
	URL       string // used by the server to reach LiveKit
	PublicURL string // returned to clients; differs from URL behind Docker or a proxy
	APIKey    string
	APISecret string
}

func (c Config) IsDevelopment() bool { return c.Env == "development" }

// Load reads configuration from the environment. Every missing required
// variable is reported at once so that a misconfigured instance fails fast.
func Load() (Config, error) {
	var errs []error
	required := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
		return v
	}

	shutdownTimeout, err := time.ParseDuration(getenv("ORBIT_SHUTDOWN_TIMEOUT", "15s"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ORBIT_SHUTDOWN_TIMEOUT: %w", err))
	}

	sessionTTL, err := time.ParseDuration(getenv("ORBIT_SESSION_TTL", "720h"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ORBIT_SESSION_TTL: %w", err))
	}

	maxGroup, err := strconv.Atoi(getenv("ORBIT_MAX_GROUP_MEMBERS", "10"))
	if err != nil || maxGroup < 2 {
		errs = append(errs, fmt.Errorf("ORBIT_MAX_GROUP_MEMBERS must be at least 2, got %q",
			os.Getenv("ORBIT_MAX_GROUP_MEMBERS")))
	}

	newConversations, err := strconv.Atoi(getenv("ORBIT_NEW_CONVERSATION_LIMIT", "10"))
	if err != nil || newConversations < 0 {
		errs = append(errs, fmt.Errorf("ORBIT_NEW_CONVERSATION_LIMIT must be zero or a positive number, got %q",
			os.Getenv("ORBIT_NEW_CONVERSATION_LIMIT")))
	}

	maxFriends, err := strconv.Atoi(getenv("ORBIT_MAX_FRIENDS", "1000"))
	if err != nil || maxFriends <= 0 {
		errs = append(errs, fmt.Errorf("ORBIT_MAX_FRIENDS must be a positive number, got %q",
			os.Getenv("ORBIT_MAX_FRIENDS")))
	}

	friendRequestDaily, err := strconv.Atoi(getenv("ORBIT_FRIEND_REQUEST_DAILY_LIMIT", "20"))
	if err != nil || friendRequestDaily < 0 {
		errs = append(errs, fmt.Errorf("ORBIT_FRIEND_REQUEST_DAILY_LIMIT must be zero or a positive number, got %q",
			os.Getenv("ORBIT_FRIEND_REQUEST_DAILY_LIMIT")))
	}

	friendRequestPending, err := strconv.Atoi(getenv("ORBIT_FRIEND_REQUEST_PENDING_LIMIT", "100"))
	if err != nil || friendRequestPending < 0 {
		errs = append(errs, fmt.Errorf("ORBIT_FRIEND_REQUEST_PENDING_LIMIT must be zero or a positive number, got %q",
			os.Getenv("ORBIT_FRIEND_REQUEST_PENDING_LIMIT")))
	}

	maxUpload, err := strconv.ParseInt(getenv("ORBIT_MAX_UPLOAD_BYTES", "26214400"), 10, 64)
	if err != nil || maxUpload <= 0 {
		errs = append(errs, fmt.Errorf("ORBIT_MAX_UPLOAD_BYTES must be a positive number of bytes, got %q",
			os.Getenv("ORBIT_MAX_UPLOAD_BYTES")))
	}

	linkTTL, err := time.ParseDuration(getenv("ORBIT_ATTACHMENT_LINK_TTL", "10m"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ORBIT_ATTACHMENT_LINK_TTL: %w", err))
	}

	orphanAge, err := time.ParseDuration(getenv("ORBIT_ORPHAN_ATTACHMENT_AGE", "24h"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ORBIT_ORPHAN_ATTACHMENT_AGE: %w", err))
	}

	maxIcon, err := strconv.ParseInt(getenv("ORBIT_MAX_ICON_BYTES", "2097152"), 10, 64)
	if err != nil || maxIcon <= 0 {
		errs = append(errs, fmt.Errorf("ORBIT_MAX_ICON_BYTES must be a positive number of bytes, got %q",
			os.Getenv("ORBIT_MAX_ICON_BYTES")))
	}

	iconLinkTTL, err := time.ParseDuration(getenv("ORBIT_ICON_LINK_TTL", "1h"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ORBIT_ICON_LINK_TTL: %w", err))
	}

	cfg := Config{
		Env:                       getenv("ORBIT_ENV", "production"),
		HTTPAddr:                  getenv("ORBIT_HTTP_ADDR", ":8080"),
		ShutdownTimeout:           shutdownTimeout,
		DatabaseURL:               required("ORBIT_DATABASE_URL"),
		CORSOrigins:               splitList(os.Getenv("ORBIT_CORS_ORIGINS")),
		SessionTTL:                sessionTTL,
		MaxGroupMembers:           maxGroup,
		NewConversationLimit:      newConversations,
		MaxFriends:                maxFriends,
		FriendRequestDailyLimit:   friendRequestDaily,
		FriendRequestPendingLimit: friendRequestPending,
		Storage: Storage{
			Endpoint:       required("ORBIT_S3_ENDPOINT"),
			Bucket:         required("ORBIT_S3_BUCKET"),
			AccessKey:      required("ORBIT_S3_ACCESS_KEY"),
			SecretKey:      required("ORBIT_S3_SECRET_KEY"),
			MaxUploadBytes: maxUpload,
			LinkTTL:        linkTTL,
			OrphanAge:      orphanAge,
			MaxIconBytes:   maxIcon,
			IconLinkTTL:    iconLinkTTL,
		},
		LiveKit: LiveKit{
			URL:       required("LIVEKIT_URL"),
			APIKey:    required("LIVEKIT_API_KEY"),
			APISecret: required("LIVEKIT_API_SECRET"),
		},
	}

	cfg.LiveKit.PublicURL = getenv("LIVEKIT_PUBLIC_URL", cfg.LiveKit.URL)

	if cfg.Env != "development" && cfg.Env != "production" {
		errs = append(errs, fmt.Errorf("ORBIT_ENV must be development or production, got %q", cfg.Env))
	}

	return cfg, errors.Join(errs...)
}

func getenv(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
