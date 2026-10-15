// Package config loads server configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	Env             string // "development" or "production"
	HTTPAddr        string
	ShutdownTimeout time.Duration
	DatabaseURL     string
	CORSOrigins     []string
	LiveKit         LiveKit
}

type LiveKit struct {
	URL       string
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

	cfg := Config{
		Env:             getenv("ORBIT_ENV", "production"),
		HTTPAddr:        getenv("ORBIT_HTTP_ADDR", ":8080"),
		ShutdownTimeout: shutdownTimeout,
		DatabaseURL:     required("ORBIT_DATABASE_URL"),
		CORSOrigins:     splitList(os.Getenv("ORBIT_CORS_ORIGINS")),
		LiveKit: LiveKit{
			URL:       required("LIVEKIT_URL"),
			APIKey:    required("LIVEKIT_API_KEY"),
			APISecret: required("LIVEKIT_API_SECRET"),
		},
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
