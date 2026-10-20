// Package auth owns accounts and sessions: registration, sign-in, sign-out and
// password changes (FR-01). Passwords are stored as argon2id hashes, sessions as
// opaque tokens whose hash alone lives in the database.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/ratelimit"
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
	// ErrNotGuest means the account already has a password, so there is nothing
	// to claim.
	ErrNotGuest = errors.New("auth: not a guest account")
	// ErrNoPassword means a guest has no password to confirm a change with.
	ErrNoPassword = errors.New("auth: a guest account has no password")
	// ErrInvalidNickname means the handle does not follow the nickname rule.
	ErrInvalidNickname = errors.New("auth: a nickname must be 1-32 letters, digits, spaces, '.', '_' or '-'")
	// ErrInvalidEmail means the address could not be parsed as one address.
	ErrInvalidEmail = errors.New("auth: the email address is not valid")
	// ErrNicknameReserved means someone released this handle less than seven
	// days ago and it must not be taken over yet.
	ErrNicknameReserved = errors.New("auth: the nickname is reserved for 7 days after release")
	// ErrRateLimited means this account changed its nickname too often lately.
	ErrRateLimited = errors.New("auth: too many nickname changes")
)

// RateLimitedError carries how long the caller should wait; the handler puts it
// in Retry-After. It unwraps to ErrRateLimited, so errors.Is stays simple.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }

// nicknameHold is how long a released handle stays out of circulation. It is
// what stops someone from taking a name the moment its owner drops it and
// using it to impersonate them.
const nicknameHold = 7 * 24 * time.Hour

// nicknamePattern is the rule api/openapi.yaml states for registration, kept
// here as well so a direct service caller cannot store a handle the API would
// refuse.
var nicknamePattern = regexp.MustCompile(`^[A-Za-z0-9А-Яа-яЁё][A-Za-z0-9А-Яа-яЁё _.-]{0,31}$`)

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
	// PromoteGuest gives a guest a nickname, an email and a password, keeping its
	// id. Returns ErrNotGuest when the account is not a guest any more.
	PromoteGuest(ctx context.Context, id uuid.UUID, nickname, email, passwordHash string) (User, error)
	// NicknameIsHeld reports whether the handle was released after since.
	NicknameIsHeld(ctx context.Context, nickname string, since time.Time) (bool, error)
	// ChangeNickname releases the old handle and takes the new one in one
	// statement; ErrTaken or ErrNicknameReserved says why it did not.
	ChangeNickname(ctx context.Context, id uuid.UUID, nickname string) (User, error)
	// ChangeEmail replaces the address and marks it unverified; an empty address
	// clears it.
	ChangeEmail(ctx context.Context, id uuid.UUID, email string) (User, error)
}

// Service holds the rules; it knows nothing about HTTP.
type Service struct {
	store      Store
	sessionTTL time.Duration
	now        func() time.Time
	// nicknameLimiter is the per-account hourly budget for renames. A nil
	// limiter allows everything, which is how an operator turns the guard off.
	nicknameLimiter *ratelimit.Limiter
}

func NewService(store Store, sessionTTL time.Duration) *Service {
	return &Service{store: store, sessionTTL: sessionTTL, now: time.Now}
}

// SetNicknameLimiter wires in the rename budget after both exist.
func (s *Service) SetNicknameLimiter(limiter *ratelimit.Limiter) { s.nicknameLimiter = limiter }

// Credentials is a session handed to the client: the token goes into the cookie.
type Credentials struct {
	Token     string
	ExpiresAt time.Time
}

// Register creates an account and its first session. email may be empty.
func (s *Service) Register(ctx context.Context, nickname, email, password string) (User, Credentials, error) {
	nickname, err := s.availableNickname(ctx, nickname)
	if err != nil {
		return User{}, Credentials{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, Credentials{}, fmt.Errorf("hash password: %w", err)
	}

	user, err := s.store.CreateUser(ctx, NewUser{
		Nickname:     nickname,
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

// RegisterGuest creates an account without a password and starts its session.
// A guest keeps its id when it signs up, so its messages and memberships stay.
func (s *Service) RegisterGuest(ctx context.Context, nickname string) (User, Credentials, error) {
	nickname, err := s.availableNickname(ctx, nickname)
	if err != nil {
		return User{}, Credentials{}, err
	}
	user, err := s.store.CreateUser(ctx, NewUser{
		Nickname: nickname,
		IsGuest:  true,
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

// ClaimGuest turns a guest into a full account in place: same id, same history.
// The session stays, so the person is not thrown out at the moment they sign up.
func (s *Service) ClaimGuest(ctx context.Context, userID uuid.UUID, nickname, email, password string) (User, error) {
	stored, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if !stored.IsGuest {
		return User{}, ErrNotGuest
	}
	nickname, err = s.availableNickname(ctx, nickname)
	if err != nil {
		return User{}, err
	}

	hash, err := HashPassword(password)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}
	return s.store.PromoteGuest(ctx, userID, nickname, strings.TrimSpace(email), hash)
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

// ChangeNickname replaces the unique handle after confirming the password. The
// old handle is released for a week and the new one is taken in the same
// statement; the store's unique index and trigger settle races. At most two
// changes per hour are allowed, counted per account after the password check.
func (s *Service) ChangeNickname(ctx context.Context, userID uuid.UUID, currentPassword, nickname string) (User, error) {
	user, err := s.changeableAccount(ctx, userID, currentPassword)
	if err != nil {
		return User{}, err
	}
	if allowed, retryAfter := s.nicknameLimiter.Allow(userID); !allowed {
		return User{}, &RateLimitedError{RetryAfter: retryAfter}
	}

	nickname, err = s.availableNickname(ctx, nickname)
	if err != nil {
		return User{}, err
	}
	changed, err := s.store.ChangeNickname(ctx, user.ID, nickname)
	if err != nil {
		return User{}, err
	}
	return changed, nil
}

// ChangeEmail replaces the address after confirming the password. There is no
// confirmation mail yet, so the address is simply marked unverified. An empty
// address clears it: an email is optional, and unlinking one is a fair request.
func (s *Service) ChangeEmail(ctx context.Context, userID uuid.UUID, currentPassword, address string) (User, error) {
	user, err := s.changeableAccount(ctx, userID, currentPassword)
	if err != nil {
		return User{}, err
	}
	address, err = cleanEmail(address)
	if err != nil {
		return User{}, err
	}
	return s.store.ChangeEmail(ctx, user.ID, address)
}

// changeableAccount loads the account and verifies its password. A guest has no
// password to prove control with, so it is refused instead of being allowed a
// free identity change; signing up (ClaimGuest) is the path that gives it one.
func (s *Service) changeableAccount(ctx context.Context, userID uuid.UUID, password string) (User, error) {
	stored, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if stored.PasswordHash == "" {
		return User{}, ErrNoPassword
	}
	if err := VerifyPassword(stored.PasswordHash, password); err != nil {
		if errors.Is(err, ErrPasswordMismatch) {
			return User{}, ErrCredentials
		}
		return User{}, fmt.Errorf("verify password: %w", err)
	}
	return stored.User, nil
}

// availableNickname validates a handle and checks that nobody released it in
// the last week. The check is for a clear answer; the database trigger is what
// makes it hold under a race.
func (s *Service) availableNickname(ctx context.Context, value string) (string, error) {
	nickname := strings.TrimSpace(value)
	if !nicknamePattern.MatchString(nickname) {
		return "", ErrInvalidNickname
	}
	held, err := s.store.NicknameIsHeld(ctx, nickname, s.now().Add(-nicknameHold))
	if err != nil {
		return "", fmt.Errorf("check nickname hold: %w", err)
	}
	if held {
		return "", ErrNicknameReserved
	}
	return nickname, nil
}

// cleanEmail accepts one bare address and preserves its case; an empty value
// clears the address. ParseAddress alone would also accept a display name, so
// the result must be exactly what the caller sent.
func cleanEmail(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > 254 {
		return "", ErrInvalidEmail
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Name != "" || address.Address != value {
		return "", ErrInvalidEmail
	}
	return value, nil
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
