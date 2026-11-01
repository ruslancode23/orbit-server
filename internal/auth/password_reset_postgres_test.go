package auth

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
)

// The reset flow against real PostgreSQL: only the hash is stored, the previous
// link stops working, a token is one-time, and an expired one is refused. The
// single statement that consumes it also changes the password and revokes the
// sessions, which a fake cannot prove.
func TestPostgresPasswordReset(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := t.Context()
	svc := NewService(NewPostgresStore(db.New(pool)), time.Hour)
	svc.SetPasswordResetTTL(time.Hour)

	user, first, err := svc.Register(ctx, "Anna", "anna@example.com", testPassword)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, second, err := svc.Login(ctx, "Anna", testPassword)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	address, token, err := svc.RequestPasswordReset(ctx, "anna@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	if address != "anna@example.com" || token == "" {
		t.Fatalf("RequestPasswordReset = %q, %q; want the address and a token", address, token)
	}

	// Only the SHA-256 is in the row.
	var storedHash []byte
	if err := pool.QueryRow(ctx,
		`SELECT token_hash FROM password_reset_tokens WHERE user_id = $1`, user.ID).Scan(&storedHash); err != nil {
		t.Fatalf("read token hash: %v", err)
	}
	if !bytes.Equal(storedHash, hashToken(token)) {
		t.Error("the stored value is not the SHA-256 of the token")
	}
	if bytes.Contains(storedHash, []byte(token)) {
		t.Error("the raw token is stored")
	}

	// A second request replaces the unused first token: only the newest link works.
	_, newest, err := svc.RequestPasswordReset(ctx, "Anna")
	if err != nil {
		t.Fatalf("RequestPasswordReset again: %v", err)
	}
	var unused int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM password_reset_tokens WHERE user_id = $1 AND used_at IS NULL`, user.ID).Scan(&unused); err != nil {
		t.Fatalf("count unused tokens: %v", err)
	}
	if unused != 1 {
		t.Errorf("unused tokens = %d, want the newest alone", unused)
	}
	if err := svc.ResetPassword(ctx, token, "a brand new password"); !errors.Is(err, ErrResetToken) {
		t.Errorf("the replaced token still works: %v", err)
	}

	if err := svc.ResetPassword(ctx, newest, "a brand new password"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	for name, credentials := range map[string]Credentials{"this device": first, "the other device": second} {
		if _, err := svc.Authenticate(ctx, credentials.Token); !errors.Is(err, ErrNoSession) {
			t.Errorf("%s: err = %v, want ErrNoSession", name, err)
		}
	}
	if _, _, err := svc.Login(ctx, "Anna", testPassword); !errors.Is(err, ErrCredentials) {
		t.Errorf("the old password still works: %v", err)
	}
	if _, _, err := svc.Login(ctx, "Anna", "a brand new password"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
	if err := svc.ResetPassword(ctx, newest, "yet another password"); !errors.Is(err, ErrResetToken) {
		t.Errorf("the token was usable twice: %v", err)
	}

	// An expired token is the same generic refusal.
	_, expired, err := svc.RequestPasswordReset(ctx, "Anna")
	if err != nil {
		t.Fatalf("RequestPasswordReset for expiry: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE password_reset_tokens SET expires_at = now() - interval '1 minute' WHERE token_hash = $1`,
		hashToken(expired)); err != nil {
		t.Fatalf("expire token: %v", err)
	}
	if err := svc.ResetPassword(ctx, expired, "another new password"); !errors.Is(err, ErrResetToken) {
		t.Errorf("an expired token was accepted: %v", err)
	}
}

// An unknown account and an account without an address get no token and no
// error: the handler answers the same 204 for all of them.
func TestPostgresPasswordResetHasNothingToSendForSomeAccounts(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := t.Context()
	svc := NewService(NewPostgresStore(db.New(pool)), time.Hour)

	if _, _, err := svc.Register(ctx, "NoMail", "", testPassword); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, _, err := svc.RegisterGuest(ctx, "Guest"); err != nil {
		t.Fatalf("RegisterGuest: %v", err)
	}

	for _, login := range []string{"nobody", "NoMail", "Guest"} {
		address, token, err := svc.RequestPasswordReset(ctx, login)
		if err != nil {
			t.Errorf("%s: err = %v, want no error", login, err)
		}
		if address != "" || token != "" {
			t.Errorf("%s: got %q / %q, want nothing", login, address, token)
		}
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM password_reset_tokens`).Scan(&rows); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if rows != 0 {
		t.Errorf("tokens = %d, want none", rows)
	}
}
