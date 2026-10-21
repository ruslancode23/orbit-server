// Package pgtest gives a test its own migrated PostgreSQL database.
//
// The tests that use it check SQL that fakes cannot: ordering, cursors,
// constraints and counts over real rows. They run only when
// ORBIT_TEST_DATABASE_URL points at a server where the account may create
// databases (`make test-db` passes the compose one); otherwise they are skipped,
// so `go test ./...` never depends on a developer's database.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvURL names the server the integration tests create their databases on.
const EnvURL = "ORBIT_TEST_DATABASE_URL"

// New creates an empty database, applies every migration to it and drops it
// when the test ends. Each call is isolated: tests may run in parallel.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()

	base := os.Getenv(EnvURL)
	if base == "" {
		t.Skipf("%s is not set: skipping the PostgreSQL test", EnvURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to %s: %v", EnvURL, err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	name := "orbit_test_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Errorf("connect to drop %s: %v", name, err)
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})

	dbURL, err := withDatabase(base, name)
	if err != nil {
		t.Fatalf("test database url: %v", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(pool.Close)

	migrate(ctx, t, pool)
	return pool
}

// migrate runs the Up part of every goose migration in order. A file without
// arguments goes through the simple protocol, which accepts several statements
// and dollar-quoted bodies at once, so the goose annotations — plain comments —
// need no parsing beyond cutting the Down part off.
func migrate(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(migrationsDir(t), "*.sql"))
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	sort.Strings(files)
	for _, file := range files {
		raw, err := os.ReadFile(file) //nolint:gosec // the repository's own migrations
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		up, _, _ := strings.Cut(string(raw), "-- +goose Down")
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(file), err)
		}
	}
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate pgtest source")
	}
	// internal/platform/postgres/pgtest → repository root.
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "..", "migrations")
}

func withDatabase(raw, name string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	parsed.Path = "/" + name
	return parsed.String(), nil
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(buf)
}
