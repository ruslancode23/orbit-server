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
		"ORBIT_ATTACHMENT_LINK_TTL", "ORBIT_ORPHAN_ATTACHMENT_AGE", "LIVEKIT_PUBLIC_URL",
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
