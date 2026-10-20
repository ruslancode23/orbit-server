package config

import (
	"strings"
	"testing"
	"time"
)

// setRequired sets the required variables and clears the optional ones,
// so tests do not depend on the developer's environment or .env.
func setRequired(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"ORBIT_ENV", "ORBIT_HTTP_ADDR", "ORBIT_SHUTDOWN_TIMEOUT", "ORBIT_CORS_ORIGINS", "ORBIT_SESSION_TTL", "ORBIT_MAX_UPLOAD_BYTES",
		"ORBIT_ATTACHMENT_LINK_TTL", "ORBIT_ORPHAN_ATTACHMENT_AGE", "ORBIT_NEW_CONVERSATION_LIMIT",
		"ORBIT_MAX_FRIENDS", "ORBIT_FRIEND_REQUEST_DAILY_LIMIT", "ORBIT_FRIEND_REQUEST_PENDING_LIMIT", "LIVEKIT_PUBLIC_URL",
		"ORBIT_MAX_ICON_BYTES", "ORBIT_ICON_LINK_TTL", "ORBIT_MAX_AVATAR_BYTES", "ORBIT_AVATAR_LINK_TTL",
		"ORBIT_NICKNAME_CHANGE_HOURLY_LIMIT",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("ORBIT_DATABASE_URL", "postgres://orbit:orbit@localhost:5432/orbit")
	t.Setenv("LIVEKIT_URL", "ws://localhost:7880")
	t.Setenv("LIVEKIT_API_KEY", "devkey")
	t.Setenv("LIVEKIT_API_SECRET", "secret")
	t.Setenv("ORBIT_S3_ENDPOINT", "http://localhost:8333")
	t.Setenv("ORBIT_S3_BUCKET", "orbit")
	t.Setenv("ORBIT_S3_ACCESS_KEY", "orbit")
	t.Setenv("ORBIT_S3_SECRET_KEY", "secret")
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", cfg.ShutdownTimeout)
	}
	if cfg.IsDevelopment() {
		t.Error("default env must be production")
	}
	if cfg.LiveKit.PublicURL != cfg.LiveKit.URL {
		t.Errorf("PublicURL = %q, want it to default to URL %q", cfg.LiveKit.PublicURL, cfg.LiveKit.URL)
	}
}

func TestLoadReportsAllMissingVariables(t *testing.T) {
	for _, name := range []string{
		"ORBIT_DATABASE_URL", "LIVEKIT_URL", "LIVEKIT_API_KEY", "LIVEKIT_API_SECRET",
		"ORBIT_S3_ENDPOINT", "ORBIT_S3_BUCKET", "ORBIT_S3_ACCESS_KEY", "ORBIT_S3_SECRET_KEY",
	} {
		t.Setenv(name, "")
	}

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want missing variables")
	}
	for _, name := range []string{
		"ORBIT_DATABASE_URL", "LIVEKIT_URL", "LIVEKIT_API_KEY", "LIVEKIT_API_SECRET",
		"ORBIT_S3_ENDPOINT", "ORBIT_S3_BUCKET", "ORBIT_S3_ACCESS_KEY", "ORBIT_S3_SECRET_KEY",
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %s", err, name)
		}
	}
}

func TestLoadParsesCORSOrigins(t *testing.T) {
	setRequired(t)
	t.Setenv("ORBIT_CORS_ORIGINS", " http://localhost:5173, ,https://orbit.example ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"http://localhost:5173", "https://orbit.example"}
	if strings.Join(cfg.CORSOrigins, "|") != strings.Join(want, "|") {
		t.Errorf("CORSOrigins = %v, want %v", cfg.CORSOrigins, want)
	}
}

func TestLoadSessionTTL(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		setRequired(t)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.SessionTTL != 720*time.Hour {
			t.Errorf("SessionTTL = %v, want 720h", cfg.SessionTTL)
		}
	})

	t.Run("from the environment", func(t *testing.T) {
		setRequired(t)
		t.Setenv("ORBIT_SESSION_TTL", "48h")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.SessionTTL != 48*time.Hour {
			t.Errorf("SessionTTL = %v, want 48h", cfg.SessionTTL)
		}
	})

	t.Run("unparsable", func(t *testing.T) {
		setRequired(t)
		t.Setenv("ORBIT_SESSION_TTL", "a month")

		if _, err := Load(); err == nil {
			t.Error("Load() error = nil, want a complaint about ORBIT_SESSION_TTL")
		}
	})
}

func TestLoadNewConversationLimit(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		setRequired(t)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.NewConversationLimit != 10 {
			t.Errorf("NewConversationLimit = %d, want 10", cfg.NewConversationLimit)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		setRequired(t)
		t.Setenv("ORBIT_NEW_CONVERSATION_LIMIT", "0")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.NewConversationLimit != 0 {
			t.Errorf("NewConversationLimit = %d, want 0", cfg.NewConversationLimit)
		}
	})

	t.Run("negative", func(t *testing.T) {
		setRequired(t)
		t.Setenv("ORBIT_NEW_CONVERSATION_LIMIT", "-1")

		if _, err := Load(); err == nil {
			t.Error("Load() error = nil, want a complaint about ORBIT_NEW_CONVERSATION_LIMIT")
		}
	})
}

func TestLoadFriendLimits(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		setRequired(t)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.MaxFriends != 1000 {
			t.Errorf("MaxFriends = %d, want 1000", cfg.MaxFriends)
		}
		if cfg.FriendRequestDailyLimit != 20 {
			t.Errorf("FriendRequestDailyLimit = %d, want 20", cfg.FriendRequestDailyLimit)
		}
		if cfg.FriendRequestPendingLimit != 100 {
			t.Errorf("FriendRequestPendingLimit = %d, want 100", cfg.FriendRequestPendingLimit)
		}
	})

	t.Run("disabled guards", func(t *testing.T) {
		setRequired(t)
		t.Setenv("ORBIT_FRIEND_REQUEST_DAILY_LIMIT", "0")
		t.Setenv("ORBIT_FRIEND_REQUEST_PENDING_LIMIT", "0")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.FriendRequestDailyLimit != 0 || cfg.FriendRequestPendingLimit != 0 {
			t.Errorf("limits = %d/%d, want both off", cfg.FriendRequestDailyLimit, cfg.FriendRequestPendingLimit)
		}
	})

	t.Run("a friend limit is always positive", func(t *testing.T) {
		setRequired(t)
		t.Setenv("ORBIT_MAX_FRIENDS", "0")

		if _, err := Load(); err == nil {
			t.Error("Load() error = nil, want a complaint about ORBIT_MAX_FRIENDS")
		}
	})

	t.Run("negative request limits", func(t *testing.T) {
		setRequired(t)
		t.Setenv("ORBIT_FRIEND_REQUEST_DAILY_LIMIT", "-1")
		t.Setenv("ORBIT_FRIEND_REQUEST_PENDING_LIMIT", "-1")

		if _, err := Load(); err == nil {
			t.Error("Load() error = nil, want complaints about the request limits")
		}
	})
}

func TestLoadAvatarAndNicknameSettings(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		setRequired(t)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Storage.MaxAvatarBytes != 8<<20 {
			t.Errorf("MaxAvatarBytes = %d, want 8 MiB", cfg.Storage.MaxAvatarBytes)
		}
		if cfg.Storage.AvatarLinkTTL != 168*time.Hour {
			t.Errorf("AvatarLinkTTL = %v, want 168h", cfg.Storage.AvatarLinkTTL)
		}
		if cfg.NicknameChangeHourlyLimit != 2 {
			t.Errorf("NicknameChangeHourlyLimit = %d, want 2", cfg.NicknameChangeHourlyLimit)
		}
	})

	t.Run("nickname limit off", func(t *testing.T) {
		setRequired(t)
		t.Setenv("ORBIT_NICKNAME_CHANGE_HOURLY_LIMIT", "0")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.NicknameChangeHourlyLimit != 0 {
			t.Errorf("NicknameChangeHourlyLimit = %d, want 0", cfg.NicknameChangeHourlyLimit)
		}
	})

	t.Run("negative nickname limit", func(t *testing.T) {
		setRequired(t)
		t.Setenv("ORBIT_NICKNAME_CHANGE_HOURLY_LIMIT", "-1")

		if _, err := Load(); err == nil {
			t.Error("Load() error = nil, want a complaint about ORBIT_NICKNAME_CHANGE_HOURLY_LIMIT")
		}
	})

	t.Run("avatar link longer than S3 allows", func(t *testing.T) {
		setRequired(t)
		t.Setenv("ORBIT_AVATAR_LINK_TTL", "200h")

		if _, err := Load(); err == nil {
			t.Error("Load() error = nil, want a complaint about ORBIT_AVATAR_LINK_TTL")
		}
	})

	t.Run("empty avatar limit", func(t *testing.T) {
		setRequired(t)
		t.Setenv("ORBIT_MAX_AVATAR_BYTES", "0")

		if _, err := Load(); err == nil {
			t.Error("Load() error = nil, want a complaint about ORBIT_MAX_AVATAR_BYTES")
		}
	})
}
