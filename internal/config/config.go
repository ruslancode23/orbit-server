// Package config loads server configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ruslancode23/orbit-server/internal/platform/clientip"
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
	// NicknameChangeHourlyLimit caps nickname changes per account per hour;
	// zero turns the guard off.
	NicknameChangeHourlyLimit int
	// BugReportsPerHour caps bug reports per account per hour; zero turns the
	// guard off.
	BugReportsPerHour int
	// TrustedProxies are the addresses whose X-Forwarded-For is believed; empty
	// means the connection's peer is the client.
	TrustedProxies []netip.Prefix
	// LoginPerMinutePerIP caps sign-in attempts from one address per minute.
	LoginPerMinutePerIP int
	// LoginFailuresPerHour caps wrong passwords for one login name per hour;
	// a successful sign-in costs nothing.
	LoginFailuresPerHour int
	// SignupsPerHourPerIP caps new accounts, registered or guest, from one
	// address per hour.
	SignupsPerHourPerIP int
	// LinkPreviewAllowPrivate lets the preview fetcher reach loopback and private
	// addresses. Off by default: it exists for local end-to-end tests only.
	LinkPreviewAllowPrivate bool
	LiveKit                 LiveKit
	// Push holds the VAPID keys for Web Push; all empty turns push off.
	Push    Push
	Storage Storage
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
	// MaxAvatarBytes caps one avatar upload.
	MaxAvatarBytes int64
	// AvatarLinkTTL is how long the signed link to an avatar stays valid. S3
	// refuses a presigned link longer than seven days.
	AvatarLinkTTL time.Duration
	// MaxScreenshotBytes caps the screenshot attached to one bug report.
	MaxScreenshotBytes int64
}

type LiveKit struct {
	URL       string // used by the server to reach LiveKit
	PublicURL string // returned to clients; differs from URL behind Docker or a proxy
	APIKey    string
	APISecret string
}

// Push is the VAPID key pair (base64url, as webpush-go generates it) and the
// subject push services may contact about this sender: mailto: or https:.
type Push struct {
	PublicKey  string
	PrivateKey string
	Subject    string
}

func (p Push) Enabled() bool { return p.PublicKey != "" }

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

	nicknameChanges, err := strconv.Atoi(getenv("ORBIT_NICKNAME_CHANGE_HOURLY_LIMIT", "2"))
	if err != nil || nicknameChanges < 0 {
		errs = append(errs, fmt.Errorf("ORBIT_NICKNAME_CHANGE_HOURLY_LIMIT must be zero or a positive number, got %q",
			os.Getenv("ORBIT_NICKNAME_CHANGE_HOURLY_LIMIT")))
	}

	bugReports, err := strconv.Atoi(getenv("ORBIT_BUG_REPORTS_PER_HOUR", "5"))
	if err != nil || bugReports < 0 {
		errs = append(errs, fmt.Errorf("ORBIT_BUG_REPORTS_PER_HOUR must be zero or a positive number, got %q",
			os.Getenv("ORBIT_BUG_REPORTS_PER_HOUR")))
	}

	trustedProxies, err := clientip.ParseTrusted(splitList(os.Getenv("ORBIT_TRUSTED_PROXIES")))
	if err != nil {
		errs = append(errs, fmt.Errorf("ORBIT_TRUSTED_PROXIES: %w", err))
	}

	loginPerIP, err := strconv.Atoi(getenv("ORBIT_LOGIN_PER_MINUTE_PER_IP", "20"))
	if err != nil || loginPerIP < 0 {
		errs = append(errs, fmt.Errorf("ORBIT_LOGIN_PER_MINUTE_PER_IP must be zero or a positive number, got %q",
			os.Getenv("ORBIT_LOGIN_PER_MINUTE_PER_IP")))
	}

	loginFailures, err := strconv.Atoi(getenv("ORBIT_LOGIN_FAILURES_PER_HOUR", "10"))
	if err != nil || loginFailures < 0 {
		errs = append(errs, fmt.Errorf("ORBIT_LOGIN_FAILURES_PER_HOUR must be zero or a positive number, got %q",
			os.Getenv("ORBIT_LOGIN_FAILURES_PER_HOUR")))
	}

	signupsPerIP, err := strconv.Atoi(getenv("ORBIT_SIGNUPS_PER_HOUR_PER_IP", "30"))
	if err != nil || signupsPerIP < 0 {
		errs = append(errs, fmt.Errorf("ORBIT_SIGNUPS_PER_HOUR_PER_IP must be zero or a positive number, got %q",
			os.Getenv("ORBIT_SIGNUPS_PER_HOUR_PER_IP")))
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

	maxAvatar, err := strconv.ParseInt(getenv("ORBIT_MAX_AVATAR_BYTES", "8388608"), 10, 64)
	if err != nil || maxAvatar <= 0 {
		errs = append(errs, fmt.Errorf("ORBIT_MAX_AVATAR_BYTES must be a positive number of bytes, got %q",
			os.Getenv("ORBIT_MAX_AVATAR_BYTES")))
	}

	maxScreenshot, err := strconv.ParseInt(getenv("ORBIT_MAX_SCREENSHOT_BYTES", "5242880"), 10, 64)
	if err != nil || maxScreenshot <= 0 {
		errs = append(errs, fmt.Errorf("ORBIT_MAX_SCREENSHOT_BYTES must be a positive number of bytes, got %q",
			os.Getenv("ORBIT_MAX_SCREENSHOT_BYTES")))
	}

	avatarLinkTTL, err := time.ParseDuration(getenv("ORBIT_AVATAR_LINK_TTL", "168h"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ORBIT_AVATAR_LINK_TTL: %w", err))
	}
	// S3's SigV4 presigner refuses anything longer, so fail at startup instead
	// of returning an avatar URL that cannot be built.
	if avatarLinkTTL > 7*24*time.Hour {
		errs = append(errs, fmt.Errorf("ORBIT_AVATAR_LINK_TTL must not exceed 168h, got %q", os.Getenv("ORBIT_AVATAR_LINK_TTL")))
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
		NicknameChangeHourlyLimit: nicknameChanges,
		BugReportsPerHour:         bugReports,
		TrustedProxies:            trustedProxies,
		LoginPerMinutePerIP:       loginPerIP,
		LoginFailuresPerHour:      loginFailures,
		SignupsPerHourPerIP:       signupsPerIP,
		LinkPreviewAllowPrivate:   envBool("ORBIT_LINK_PREVIEW_ALLOW_PRIVATE"),
		Storage: Storage{
			Endpoint:           required("ORBIT_S3_ENDPOINT"),
			Bucket:             required("ORBIT_S3_BUCKET"),
			AccessKey:          required("ORBIT_S3_ACCESS_KEY"),
			SecretKey:          required("ORBIT_S3_SECRET_KEY"),
			MaxUploadBytes:     maxUpload,
			LinkTTL:            linkTTL,
			OrphanAge:          orphanAge,
			MaxIconBytes:       maxIcon,
			IconLinkTTL:        iconLinkTTL,
			MaxAvatarBytes:     maxAvatar,
			AvatarLinkTTL:      avatarLinkTTL,
			MaxScreenshotBytes: maxScreenshot,
		},
		LiveKit: LiveKit{
			URL:       required("LIVEKIT_URL"),
			APIKey:    required("LIVEKIT_API_KEY"),
			APISecret: required("LIVEKIT_API_SECRET"),
		},
	}

	cfg.LiveKit.PublicURL = getenv("LIVEKIT_PUBLIC_URL", cfg.LiveKit.URL)

	cfg.Push = Push{
		PublicKey:  os.Getenv("ORBIT_VAPID_PUBLIC_KEY"),
		PrivateKey: os.Getenv("ORBIT_VAPID_PRIVATE_KEY"),
		Subject:    os.Getenv("ORBIT_VAPID_SUBJECT"),
	}
	set := 0
	for _, v := range []string{cfg.Push.PublicKey, cfg.Push.PrivateKey, cfg.Push.Subject} {
		if v != "" {
			set++
		}
	}
	if set != 0 && set != 3 {
		errs = append(errs, errors.New("ORBIT_VAPID_PUBLIC_KEY, ORBIT_VAPID_PRIVATE_KEY and ORBIT_VAPID_SUBJECT go together: set all three or none"))
	}
	if cfg.Push.Subject != "" && !strings.HasPrefix(cfg.Push.Subject, "mailto:") && !strings.HasPrefix(cfg.Push.Subject, "https://") {
		errs = append(errs, fmt.Errorf("ORBIT_VAPID_SUBJECT must be a mailto: or https: URL, got %q", cfg.Push.Subject))
	}

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

// envBool reads a yes/no switch; anything but 1/true/yes is false.
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes":
		return true
	}
	return false
}
