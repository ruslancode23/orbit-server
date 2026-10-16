// Package auth owns accounts and sessions: registration, sign-in, sign-out and
// password changes (FR-01). Passwords are stored as argon2id hashes, sessions as
// opaque tokens whose hash alone lives in the database.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrCredentials covers both an unknown login and a wrong password: the caller
	// must not tell them apart, or it leaks which accounts exist.
	ErrCredentials = errors.New("auth: wrong login or password")
	// ErrTaken means the nickname or email already belongs to someone.
	ErrTaken = errors.New("auth: nickname or email is taken")
	// ErrNoSession means the token is unknown, expired or revoked.
	ErrNoSession = errors.New("auth: no live session")
	// ErrNoUser means the account is gone.
	ErrNoUser = errors.New("auth: no such user")
)

// User is an account as the rest of the server sees it, without the password hash.
type User struct {
	ID        uuid.UUID
	Nickname  string
	Email     string // empty when the account has none
	IsGuest   bool
	CreatedAt time.Time
}

// StoredUser adds what only this package may see.
type StoredUser struct {
	User
	PasswordHash string // empty for a guest
}

// NewUser is what Store.CreateUser writes.
type NewUser struct {
	Nickname     string
	Email        string // empty means no email
	PasswordHash string
	IsGuest      bool
}

// Session is a live session resolved from its token.
type Session struct {
	ID        uuid.UUID
	User      User
	ExpiresAt time.Time
}

// Store is the persistence this package needs. The interface lives with its
// consumer; internal/auth/store.go implements it over PostgreSQL.
type Store interface {
	// CreateUser returns ErrTaken when the nickname or email is in use.
	CreateUser(ctx context.Context, u NewUser) (User, error)
	// UserByLogin accepts a nickname or an email, either case; ErrNoUser when there is none.
	UserByLogin(ctx context.Context, login string) (StoredUser, error)
	UserByID(ctx context.Context, id uuid.UUID) (StoredUser, error)
	CreateSession(ctx context.Context, userID uuid.UUID, tokenHash []byte, expiresAt time.Time) (uuid.UUID, error)
	// LiveSession returns ErrNoSession unless the session exists, is unrevoked and unexpired.
	LiveSession(ctx context.Context, tokenHash []byte) (Session, error)
	TouchSession(ctx context.Context, id uuid.UUID) error
	RevokeSession(ctx context.Context, id uuid.UUID) error
	RevokeUserSessions(ctx context.Context, userID uuid.UUID) error
	SetPassword(ctx context.Context, userID uuid.UUID, passwordHash string) error
}

// Service holds the rules; it knows nothing about HTTP.
type Service struct {
	store      Store
	sessionTTL time.Duration
	now        func() time.Time
}

func NewService(store Store, sessionTTL time.Duration) *Service {
	return &Service{store: store, sessionTTL: sessionTTL, now: time.Now}
}

// Credentials is a session handed to the client: the token goes into the cookie.
type Credentials struct {
	Token     string
	ExpiresAt time.Time
}

// Register creates an account and its first session. email may be empty.
func (s *Service) Register(ctx context.Context, nickname, email, password string) (User, Credentials, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, Credentials{}, fmt.Errorf("hash password: %w", err)
	}

	user, err := s.store.CreateUser(ctx, NewUser{
		Nickname:     strings.TrimSpace(nickname),
		Email:        strings.TrimSpace(email),
		PasswordHash: hash,
	})
	if err != nil {
		return User{}, Credentials{}, err
	}

	creds, err := s.startSession(ctx, user.ID)
	if err != nil {
		return User{}, Credentials{}, err
	}
	return user, creds, nil
}

// Login verifies the password and starts a session. A wrong login and a wrong
// password are the same error, and a guest (no password) cannot sign in.
func (s *Service) Login(ctx context.Context, login, password string) (User, Credentials, error) {
	stored, err := s.store.UserByLogin(ctx, strings.TrimSpace(login))
	switch {
	case errors.Is(err, ErrNoUser):
		return User{}, Credentials{}, ErrCredentials
	case err != nil:
		return User{}, Credentials{}, err
	case stored.PasswordHash == "":
		return User{}, Credentials{}, ErrCredentials
	}

	if err := VerifyPassword(stored.PasswordHash, password); err != nil {
		if errors.Is(err, ErrPasswordMismatch) {
			return User{}, Credentials{}, ErrCredentials
		}
		return User{}, Credentials{}, fmt.Errorf("verify password: %w", err)
	}

	creds, err := s.startSession(ctx, stored.ID)
	if err != nil {
		return User{}, Credentials{}, err
	}
	return stored.User, creds, nil
}

// Authenticate resolves a session token. The store checks expiry and revocation,
// so an expired or revoked token is ErrNoSession here as well.
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrNoSession
	}
	session, err := s.store.LiveSession(ctx, hashToken(token))
	if err != nil {
		return Session{}, err
	}
	// Best effort: a failed touch must not cost the user their request.
	_ = s.store.TouchSession(ctx, session.ID)
	return session, nil
}

// Logout ends one session and leaves the other devices alone.
func (s *Service) Logout(ctx context.Context, sessionID uuid.UUID) error {
	return s.store.RevokeSession(ctx, sessionID)
}

// ChangePassword sets a new password and revokes every session of the account,
// this one included: a stolen session dies with the password change.
func (s *Service) ChangePassword(ctx context.Context, userID uuid.UUID, current, next string) error {
	stored, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if stored.PasswordHash == "" {
		return ErrCredentials
	}
	if err := VerifyPassword(stored.PasswordHash, current); err != nil {
		if errors.Is(err, ErrPasswordMismatch) {
			return ErrCredentials
		}
		return fmt.Errorf("verify password: %w", err)
	}

	hash, err := HashPassword(next)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if err := s.store.SetPassword(ctx, userID, hash); err != nil {
		return err
	}
	return s.store.RevokeUserSessions(ctx, userID)
}

func (s *Service) startSession(ctx context.Context, userID uuid.UUID) (Credentials, error) {
	token, hash, err := newSessionToken()
	if err != nil {
		return Credentials{}, err
	}
	expiresAt := s.now().Add(s.sessionTTL)
	if _, err := s.store.CreateSession(ctx, userID, hash, expiresAt); err != nil {
		return Credentials{}, err
	}
	return Credentials{Token: token, ExpiresAt: expiresAt}, nil
}
