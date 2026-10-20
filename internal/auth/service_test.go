package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

// fakeStore keeps accounts and sessions in memory and enforces the same rules the
// database does: unique nickname and email, and no live session once it expires or is revoked.
type fakeStore struct {
	users    []StoredUser
	sessions []fakeSession
	released map[string]time.Time
	now      func() time.Time
}

type fakeSession struct {
	id        uuid.UUID
	userID    uuid.UUID
	hash      string
	expiresAt time.Time
	revoked   bool
	touched   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{now: time.Now, released: map[string]time.Time{}}
}

func (s *fakeStore) CreateUser(_ context.Context, u NewUser) (User, error) {
	for _, existing := range s.users {
		if strings.EqualFold(existing.Nickname, u.Nickname) ||
			(u.Email != "" && strings.EqualFold(existing.Email, u.Email)) {
			return User{}, ErrTaken
		}
	}
	user := User{
		ID:        uuid.New(),
		Nickname:  u.Nickname,
		Email:     u.Email,
		IsGuest:   u.IsGuest,
		CreatedAt: s.now(),
	}
	s.users = append(s.users, StoredUser{User: user, PasswordHash: u.PasswordHash})
	return user, nil
}

func (s *fakeStore) UserByLogin(_ context.Context, login string) (StoredUser, error) {
	for _, u := range s.users {
		if strings.EqualFold(u.Nickname, login) || (u.Email != "" && strings.EqualFold(u.Email, login)) {
			return u, nil
		}
	}
	return StoredUser{}, ErrNoUser
}

func (s *fakeStore) UserByID(_ context.Context, id uuid.UUID) (StoredUser, error) {
	for _, u := range s.users {
		if u.ID == id {
			return u, nil
		}
	}
	return StoredUser{}, ErrNoUser
}

func (s *fakeStore) CreateSession(_ context.Context, userID uuid.UUID, tokenHash []byte, expiresAt time.Time) (uuid.UUID, error) {
	id := uuid.New()
	s.sessions = append(s.sessions, fakeSession{id: id, userID: userID, hash: string(tokenHash), expiresAt: expiresAt})
	return id, nil
}

func (s *fakeStore) LiveSession(_ context.Context, tokenHash []byte) (Session, error) {
	for i := range s.sessions {
		session := &s.sessions[i]
		if session.hash != string(tokenHash) || session.revoked || !session.expiresAt.After(s.now()) {
			continue
		}
		for _, u := range s.users {
			if u.ID == session.userID {
				return Session{ID: session.id, User: u.User, ExpiresAt: session.expiresAt}, nil
			}
		}
	}
	return Session{}, ErrNoSession
}

func (s *fakeStore) TouchSession(_ context.Context, id uuid.UUID) error {
	for i := range s.sessions {
		if s.sessions[i].id == id {
			s.sessions[i].touched++
		}
	}
	return nil
}

func (s *fakeStore) RevokeSession(_ context.Context, id uuid.UUID) error {
	for i := range s.sessions {
		if s.sessions[i].id == id {
			s.sessions[i].revoked = true
		}
	}
	return nil
}

func (s *fakeStore) RevokeUserSessions(_ context.Context, userID uuid.UUID) error {
	for i := range s.sessions {
		if s.sessions[i].userID == userID {
			s.sessions[i].revoked = true
		}
	}
	return nil
}

func (s *fakeStore) PromoteGuest(_ context.Context, id uuid.UUID, nickname, email, passwordHash string) (User, error) {
	for i := range s.users {
		if s.users[i].ID != id {
			continue
		}
		if !s.users[i].IsGuest {
			return User{}, ErrNotGuest
		}
		for _, other := range s.users {
			if other.ID == id {
				continue
			}
			if strings.EqualFold(other.Nickname, nickname) || (email != "" && strings.EqualFold(other.Email, email)) {
				return User{}, ErrTaken
			}
		}
		s.users[i].Nickname = nickname
		s.users[i].Email = email
		s.users[i].PasswordHash = passwordHash
		s.users[i].IsGuest = false
		return s.users[i].User, nil
	}
	return User{}, ErrNoUser
}

func (s *fakeStore) SetPassword(_ context.Context, userID uuid.UUID, passwordHash string) error {
	for i := range s.users {
		if s.users[i].ID == userID {
			s.users[i].PasswordHash = passwordHash
			return nil
		}
	}
	return ErrNoUser
}

func (s *fakeStore) NicknameIsHeld(_ context.Context, nickname string, since time.Time) (bool, error) {
	releasedAt, ok := s.released[strings.ToLower(nickname)]
	return ok && releasedAt.After(since), nil
}

func (s *fakeStore) ChangeNickname(_ context.Context, id uuid.UUID, nickname string) (User, error) {
	for i := range s.users {
		if s.users[i].ID != id {
			continue
		}
		for _, other := range s.users {
			if other.ID != id && strings.EqualFold(other.Nickname, nickname) {
				return User{}, ErrTaken
			}
		}
		previous := s.users[i].Nickname
		if !strings.EqualFold(previous, nickname) {
			s.released[strings.ToLower(previous)] = s.now()
		}
		s.users[i].Nickname = nickname
		return s.users[i].User, nil
	}
	return User{}, ErrNoUser
}

func (s *fakeStore) ChangeEmail(_ context.Context, id uuid.UUID, email string) (User, error) {
	for i := range s.users {
		if s.users[i].ID != id {
			continue
		}
		for _, other := range s.users {
			if other.ID != id && email != "" && strings.EqualFold(other.Email, email) {
				return User{}, ErrTaken
			}
		}
		s.users[i].Email = email
		return s.users[i].User, nil
	}
	return User{}, ErrNoUser
}

const testPassword = "correct horse battery"

func newTestService(t *testing.T) (*Service, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	svc := NewService(store, time.Hour)
	// The store's clock is the service's: a test moves one to move both.
	svc.now = func() time.Time { return store.now() }
	return svc, store
}

func registerTestUser(t *testing.T, svc *Service) (User, Credentials) {
	t.Helper()
	user, creds, err := svc.Register(context.Background(), "Anna", "anna@example.com", testPassword)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return user, creds
}

func TestRegisterStartsSessionAndHashesPassword(t *testing.T) {
	svc, store := newTestService(t)

	user, creds, err := svc.Register(context.Background(), "Anna", "anna@example.com", testPassword)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if user.Nickname != "Anna" || user.Email != "anna@example.com" {
		t.Errorf("user = %+v, want the nickname and email as given", user)
	}
	if creds.Token == "" {
		t.Error("no session token")
	}
	stored := store.users[0].PasswordHash
	if stored == testPassword || !strings.HasPrefix(stored, "$argon2id$") {
		t.Errorf("stored password = %q, want an argon2id hash", stored)
	}
	// The token is never stored, only its hash.
	if store.sessions[0].hash == creds.Token {
		t.Error("the session token itself is stored")
	}

	session, err := svc.Authenticate(context.Background(), creds.Token)
	if err != nil {
		t.Fatalf("Authenticate after Register: %v", err)
	}
	if session.User.ID != user.ID {
		t.Errorf("session user = %v, want %v", session.User.ID, user.ID)
	}
}

func TestRegisterRejectsTakenNicknameInAnyCase(t *testing.T) {
	svc, _ := newTestService(t)
	registerTestUser(t, svc)

	_, _, err := svc.Register(context.Background(), "anna", "other@example.com", testPassword)
	if !errors.Is(err, ErrTaken) {
		t.Errorf("err = %v, want ErrTaken", err)
	}
}

func TestLoginByNicknameAndEmail(t *testing.T) {
	svc, _ := newTestService(t)
	user, _ := registerTestUser(t, svc)

	for _, login := range []string{"Anna", "anna", "ANNA@example.com"} {
		t.Run(login, func(t *testing.T) {
			got, creds, err := svc.Login(context.Background(), login, testPassword)
			if err != nil {
				t.Fatalf("Login: %v", err)
			}
			if got.ID != user.ID {
				t.Errorf("user = %v, want %v", got.ID, user.ID)
			}
			if creds.Token == "" {
				t.Error("no session token")
			}
		})
	}
}

func TestLoginRejectsUnknownAndWrongPasswordAlike(t *testing.T) {
	svc, _ := newTestService(t)
	registerTestUser(t, svc)

	for _, tc := range []struct{ name, login, password string }{
		{"unknown login", "bob", testPassword},
		{"wrong password", "Anna", "another password"},
		{"empty password", "Anna", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := svc.Login(context.Background(), tc.login, tc.password); !errors.Is(err, ErrCredentials) {
				t.Errorf("err = %v, want ErrCredentials", err)
			}
		})
	}
}

// A guest has no password, so nobody can sign in as one (task 2.7 gives it its own path).
func TestLoginRejectsGuest(t *testing.T) {
	svc, store := newTestService(t)
	if _, err := store.CreateUser(context.Background(), NewUser{Nickname: "Guest", IsGuest: true}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if _, _, err := svc.Login(context.Background(), "Guest", ""); !errors.Is(err, ErrCredentials) {
		t.Errorf("err = %v, want ErrCredentials", err)
	}
}

func TestAuthenticateRejectsUnknownExpiredAndRevoked(t *testing.T) {
	t.Run("unknown token", func(t *testing.T) {
		svc, _ := newTestService(t)
		registerTestUser(t, svc)

		if _, err := svc.Authenticate(context.Background(), "not-a-token"); !errors.Is(err, ErrNoSession) {
			t.Errorf("err = %v, want ErrNoSession", err)
		}
	})

	t.Run("empty token", func(t *testing.T) {
		svc, _ := newTestService(t)
		if _, err := svc.Authenticate(context.Background(), ""); !errors.Is(err, ErrNoSession) {
			t.Errorf("err = %v, want ErrNoSession", err)
		}
	})

	t.Run("expired session", func(t *testing.T) {
		svc, store := newTestService(t)
		_, creds := registerTestUser(t, svc)

		// Time moves past the TTL.
		store.now = func() time.Time { return time.Now().Add(2 * time.Hour) }

		if _, err := svc.Authenticate(context.Background(), creds.Token); !errors.Is(err, ErrNoSession) {
			t.Errorf("err = %v, want ErrNoSession", err)
		}
	})

	t.Run("revoked by logout", func(t *testing.T) {
		svc, _ := newTestService(t)
		_, creds := registerTestUser(t, svc)
		session, err := svc.Authenticate(context.Background(), creds.Token)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}

		if err := svc.Logout(context.Background(), session.ID); err != nil {
			t.Fatalf("Logout: %v", err)
		}

		if _, err := svc.Authenticate(context.Background(), creds.Token); !errors.Is(err, ErrNoSession) {
			t.Errorf("err = %v, want ErrNoSession", err)
		}
	})
}

func TestLogoutLeavesOtherSessionsAlone(t *testing.T) {
	svc, _ := newTestService(t)
	_, first := registerTestUser(t, svc)
	_, second, err := svc.Login(context.Background(), "Anna", testPassword)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	session, err := svc.Authenticate(context.Background(), first.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if err := svc.Logout(context.Background(), session.ID); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	if _, err := svc.Authenticate(context.Background(), second.Token); err != nil {
		t.Errorf("the other device lost its session: %v", err)
	}
}

func TestChangePasswordRevokesEverySession(t *testing.T) {
	svc, _ := newTestService(t)
	user, first := registerTestUser(t, svc)
	_, second, err := svc.Login(context.Background(), "Anna", testPassword)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := svc.ChangePassword(context.Background(), user.ID, testPassword, "a brand new password"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	for name, token := range map[string]string{"this device": first.Token, "the other device": second.Token} {
		if _, err := svc.Authenticate(context.Background(), token); !errors.Is(err, ErrNoSession) {
			t.Errorf("%s: err = %v, want ErrNoSession", name, err)
		}
	}

	if _, _, err := svc.Login(context.Background(), "Anna", testPassword); !errors.Is(err, ErrCredentials) {
		t.Errorf("the old password still works: %v", err)
	}
	if _, _, err := svc.Login(context.Background(), "Anna", "a brand new password"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}

func TestChangePasswordRejectsWrongCurrentPassword(t *testing.T) {
	svc, _ := newTestService(t)
	user, creds := registerTestUser(t, svc)

	err := svc.ChangePassword(context.Background(), user.ID, "not my password", "a brand new password")
	if !errors.Is(err, ErrCredentials) {
		t.Fatalf("err = %v, want ErrCredentials", err)
	}

	if _, err := svc.Authenticate(context.Background(), creds.Token); err != nil {
		t.Errorf("a failed change revoked the session: %v", err)
	}
}

func TestSessionTokensDiffer(t *testing.T) {
	svc, _ := newTestService(t)
	_, first := registerTestUser(t, svc)
	_, second, err := svc.Login(context.Background(), "Anna", testPassword)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if first.Token == second.Token {
		t.Error("two sessions got the same token")
	}
	if len(first.Token) < 40 {
		t.Errorf("token = %q, want at least 256 bits of entropy", first.Token)
	}
}

// A guest that signs up keeps its account: same id, so its servers, messages
// and read cursors stay where they are (FR-03).
func TestClaimGuestKeepsTheSameAccount(t *testing.T) {
	svc, _ := newTestService(t)

	guest, creds, err := svc.RegisterGuest(t.Context(), "Аня")
	if err != nil {
		t.Fatalf("RegisterGuest: %v", err)
	}
	if !guest.IsGuest || creds.Token == "" {
		t.Fatalf("guest = %+v, creds = %+v; want a guest with a session", guest, creds)
	}

	claimed, err := svc.ClaimGuest(t.Context(), guest.ID, "Аня", "anya@example.com", testPassword)
	if err != nil {
		t.Fatalf("ClaimGuest: %v", err)
	}

	if claimed.ID != guest.ID {
		t.Errorf("id = %v, want the guest's own %v", claimed.ID, guest.ID)
	}
	if claimed.IsGuest {
		t.Error("the account is still marked as a guest")
	}

	// The session from the guest days keeps working: signing up is not signing out.
	if _, err := svc.Authenticate(t.Context(), creds.Token); err != nil {
		t.Errorf("the session died on sign-up: %v", err)
	}
	// And the new password works.
	if _, _, err := svc.Login(t.Context(), "anya@example.com", testPassword); err != nil {
		t.Errorf("Login after the claim: %v", err)
	}
}

func TestGuestCannotSignInAndClaimIsOnlyForGuests(t *testing.T) {
	svc, _ := newTestService(t)
	guest, _, err := svc.RegisterGuest(t.Context(), "Аня")
	if err != nil {
		t.Fatalf("RegisterGuest: %v", err)
	}

	// A guest has no password, so nobody can sign in as one.
	if _, _, err := svc.Login(t.Context(), "Аня", ""); !errors.Is(err, ErrCredentials) {
		t.Errorf("Login as a guest: err = %v, want ErrCredentials", err)
	}

	if _, err := svc.ClaimGuest(t.Context(), guest.ID, "Аня", "", testPassword); err != nil {
		t.Fatalf("ClaimGuest: %v", err)
	}
	// Claiming twice is refused: the account already has a password.
	if _, err := svc.ClaimGuest(t.Context(), guest.ID, "Аня", "", "another password"); !errors.Is(err, ErrNotGuest) {
		t.Errorf("err = %v, want ErrNotGuest", err)
	}
}

func TestRegisterGuestRefusesATakenNickname(t *testing.T) {
	svc, _ := newTestService(t)
	registerTestUser(t, svc)

	if _, _, err := svc.RegisterGuest(t.Context(), "anna"); !errors.Is(err, ErrTaken) {
		t.Errorf("err = %v, want ErrTaken", err)
	}
}

func TestChangeNicknameConfirmsThePassword(t *testing.T) {
	svc, store := newTestService(t)
	user, _ := registerTestUser(t, svc)

	if _, err := svc.ChangeNickname(t.Context(), user.ID, "wrong password", "Bob"); !errors.Is(err, ErrCredentials) {
		t.Fatalf("err = %v, want ErrCredentials", err)
	}
	stored, _ := store.UserByID(t.Context(), user.ID)
	if stored.Nickname != "Anna" {
		t.Errorf("nickname = %q, want it unchanged", stored.Nickname)
	}
	if _, released := store.released["anna"]; released {
		t.Error("a failed change released the old nickname")
	}

	changed, err := svc.ChangeNickname(t.Context(), user.ID, testPassword, "Bob")
	if err != nil {
		t.Fatalf("ChangeNickname: %v", err)
	}
	if changed.Nickname != "Bob" {
		t.Errorf("nickname = %q, want Bob", changed.Nickname)
	}
	if releasedAt, ok := store.released["anna"]; !ok || releasedAt.IsZero() {
		t.Error("the old nickname was not released")
	}
}

func TestChangeNicknameRejectsACaseInsensitiveDuplicate(t *testing.T) {
	svc, _ := newTestService(t)
	anna, _ := registerTestUser(t, svc)
	// A second account with a different nickname.
	if _, _, err := svc.Register(t.Context(), "Bob", "bob@example.com", testPassword); err != nil {
		t.Fatalf("Register Bob: %v", err)
	}

	if _, err := svc.ChangeNickname(t.Context(), anna.ID, testPassword, "bob"); !errors.Is(err, ErrTaken) {
		t.Fatalf("err = %v, want ErrTaken", err)
	}
}

// A released handle is out of circulation for a week, so nobody can take it
// over right after its owner drops it. The test moves the clock.
func TestReleasedNicknameIsHeldForSevenDays(t *testing.T) {
	svc, store := newTestService(t)
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }

	anna, _ := registerTestUser(t, svc)
	if _, err := svc.ChangeNickname(t.Context(), anna.ID, testPassword, "Bob"); err != nil {
		t.Fatalf("ChangeNickname: %v", err)
	}

	if _, _, err := svc.Register(t.Context(), "anna", "other@example.com", testPassword); !errors.Is(err, ErrNicknameReserved) {
		t.Fatalf("err = %v, want ErrNicknameReserved right after the release", err)
	}

	clock = clock.Add(6 * 24 * time.Hour)
	if _, _, err := svc.Register(t.Context(), "anna", "other@example.com", testPassword); !errors.Is(err, ErrNicknameReserved) {
		t.Fatalf("err = %v, want the hold to last a full week", err)
	}

	clock = clock.Add(2 * 24 * time.Hour)
	if _, _, err := svc.Register(t.Context(), "anna", "other@example.com", testPassword); err != nil {
		t.Fatalf("Register after the hold: %v", err)
	}
}

func TestChangeNicknameIsLimitedToTwoAnHour(t *testing.T) {
	svc, _ := newTestService(t)
	svc.SetNicknameLimiter(ratelimit.NewHourly(2))
	user, _ := registerTestUser(t, svc)

	for _, nickname := range []string{"Bob", "Vika"} {
		if _, err := svc.ChangeNickname(t.Context(), user.ID, testPassword, nickname); err != nil {
			t.Fatalf("ChangeNickname %s: %v", nickname, err)
		}
	}

	_, err := svc.ChangeNickname(t.Context(), user.ID, testPassword, "Dina")
	var limited *RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want *RateLimitedError", err)
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want it to wrap ErrRateLimited", err)
	}
	if limited.RetryAfter != 30*time.Minute {
		t.Errorf("RetryAfter = %v, want 30m", limited.RetryAfter)
	}
}

func TestGuestCannotChangeNicknameOrEmail(t *testing.T) {
	svc, _ := newTestService(t)
	guest, _, err := svc.RegisterGuest(t.Context(), "Guest")
	if err != nil {
		t.Fatalf("RegisterGuest: %v", err)
	}

	if _, err := svc.ChangeNickname(t.Context(), guest.ID, "", "Bob"); !errors.Is(err, ErrNoPassword) {
		t.Errorf("nickname err = %v, want ErrNoPassword", err)
	}
	if _, err := svc.ChangeEmail(t.Context(), guest.ID, "", "guest@example.com"); !errors.Is(err, ErrNoPassword) {
		t.Errorf("email err = %v, want ErrNoPassword", err)
	}
}

func TestChangeEmailSetsAndClearsTheAddress(t *testing.T) {
	svc, _ := newTestService(t)
	user, _ := registerTestUser(t, svc)

	changed, err := svc.ChangeEmail(t.Context(), user.ID, testPassword, "new@example.com")
	if err != nil {
		t.Fatalf("ChangeEmail: %v", err)
	}
	if changed.Email != "new@example.com" {
		t.Errorf("email = %q, want the new address", changed.Email)
	}

	cleared, err := svc.ChangeEmail(t.Context(), user.ID, testPassword, "  ")
	if err != nil {
		t.Fatalf("clear email: %v", err)
	}
	if cleared.Email != "" {
		t.Errorf("email = %q, want it cleared", cleared.Email)
	}
	// With no email the nickname alone still signs in.
	if _, _, err := svc.Login(t.Context(), "Anna", testPassword); err != nil {
		t.Errorf("Login by nickname after clearing the email: %v", err)
	}
}

func TestChangeEmailRejectsAUsedAddressAndAWrongPassword(t *testing.T) {
	svc, _ := newTestService(t)
	anna, _ := registerTestUser(t, svc)
	if _, _, err := svc.Register(t.Context(), "Bob", "bob@example.com", testPassword); err != nil {
		t.Fatalf("Register Bob: %v", err)
	}

	if _, err := svc.ChangeEmail(t.Context(), anna.ID, testPassword, "BOB@example.com"); !errors.Is(err, ErrTaken) {
		t.Errorf("err = %v, want ErrTaken", err)
	}
	if _, err := svc.ChangeEmail(t.Context(), anna.ID, "wrong password", "new@example.com"); !errors.Is(err, ErrCredentials) {
		t.Errorf("err = %v, want ErrCredentials", err)
	}
	if _, err := svc.ChangeEmail(t.Context(), anna.ID, testPassword, "not an address"); !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("err = %v, want ErrInvalidEmail", err)
	}
}
