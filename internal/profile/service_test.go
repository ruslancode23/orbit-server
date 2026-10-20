package profile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/events"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// clock is a fixed clock the tests move by hand: the debounce and status
// deadlines are time-based, and a real clock makes them flaky.
type clock struct{ now time.Time }

func newClock() *clock {
	return &clock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }
func ptr[T any](value T) *T              { return &value }
func pairKey(a, b uuid.UUID) [2]uuid.UUID {
	if a.String() < b.String() {
		return [2]uuid.UUID{a, b}
	}
	return [2]uuid.UUID{b, a}
}

// fakeStore keeps profiles in memory with the same field semantics the SQL has:
// an empty display name falls back to the nickname, and a nil update field is
// not touched.
type fakeStore struct {
	mu       sync.Mutex
	profiles map[uuid.UUID]Stored
	batches  int
}

func newFakeStore() *fakeStore {
	return &fakeStore{profiles: map[uuid.UUID]Stored{}}
}

func (s *fakeStore) Profile(_ context.Context, userID uuid.UUID) (Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	profile, ok := s.profiles[userID]
	if !ok {
		return Stored{}, ErrNotFound
	}
	return profile, nil
}

func (s *fakeStore) Profiles(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches++
	out := make(map[uuid.UUID]Stored, len(ids))
	for _, id := range ids {
		if profile, ok := s.profiles[id]; ok {
			out[id] = profile
		}
	}
	return out, nil
}

func (s *fakeStore) UpdateProfile(_ context.Context, userID uuid.UUID, u Update) (Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	profile, ok := s.profiles[userID]
	if !ok {
		return Stored{}, ErrNotFound
	}
	if u.DisplayName != nil {
		if *u.DisplayName == "" {
			profile.DisplayName = profile.Nickname
		} else {
			profile.DisplayName = *u.DisplayName
		}
	}
	if u.Bio != nil {
		profile.Bio = *u.Bio
	}
	if u.AccentColor != nil {
		profile.AccentColor = *u.AccentColor
	}
	s.profiles[userID] = profile
	return profile, nil
}

func (s *fakeStore) SetStatus(_ context.Context, userID uuid.UUID, emoji, text string, expiresAt *time.Time) (Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	profile, ok := s.profiles[userID]
	if !ok {
		return Stored{}, ErrNotFound
	}
	profile.CustomStatus = &CustomStatus{Emoji: emoji, Text: text, ExpiresAt: expiresAt}
	s.profiles[userID] = profile
	return profile, nil
}

func (s *fakeStore) ClearStatus(_ context.Context, userID uuid.UUID) (Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	profile, ok := s.profiles[userID]
	if !ok {
		return Stored{}, ErrNotFound
	}
	profile.CustomStatus = nil
	s.profiles[userID] = profile
	return profile, nil
}

type personal struct {
	userID uuid.UUID
	event  events.Event
}

type fakeHub struct {
	mu       sync.Mutex
	servers  []events.Event
	personal []personal
}

func (h *fakeHub) Publish(event events.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.servers = append(h.servers, event)
}

func (h *fakeHub) PublishToUser(userID uuid.UUID, event events.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.personal = append(h.personal, personal{userID: userID, event: event})
}

func (h *fakeHub) serverEvents() []events.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]events.Event(nil), h.servers...)
}

func (h *fakeHub) personalEvents() []personal {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]personal(nil), h.personal...)
}

type fakeServers struct {
	servers map[uuid.UUID][]uuid.UUID
}

func (f *fakeServers) Servers(_ context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	return f.servers[userID], nil
}

type fakeConversations struct {
	shared map[[2]uuid.UUID]bool
	peers  map[uuid.UUID][]uuid.UUID
}

func (f *fakeConversations) SharesConversation(_ context.Context, a, b uuid.UUID) (bool, error) {
	return f.shared[pairKey(a, b)], nil
}

func (f *fakeConversations) Peers(_ context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	return f.peers[userID], nil
}

type fakeFriends struct {
	pairs   map[[2]uuid.UUID]bool
	friends map[uuid.UUID][]uuid.UUID
}

func (f *fakeFriends) AreFriends(_ context.Context, a, b uuid.UUID) (bool, error) {
	return f.pairs[pairKey(a, b)], nil
}

func (f *fakeFriends) FriendsOf(_ context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	return f.friends[userID], nil
}

type harness struct {
	svc           *Service
	store         *fakeStore
	clock         *clock
	hub           *fakeHub
	servers       *fakeServers
	conversations *fakeConversations
	friends       *fakeFriends
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := newFakeStore()
	hub := &fakeHub{}
	c := newClock()
	servers := &fakeServers{servers: map[uuid.UUID][]uuid.UUID{}}
	conversations := &fakeConversations{
		shared: map[[2]uuid.UUID]bool{},
		peers:  map[uuid.UUID][]uuid.UUID{},
	}
	friends := &fakeFriends{
		pairs:   map[[2]uuid.UUID]bool{},
		friends: map[uuid.UUID][]uuid.UUID{},
	}

	svc := NewService(store, hub, testLogger())
	svc.now = c.Now
	svc.SetServers(servers)
	svc.SetConversations(conversations)
	svc.SetFriends(friends)
	return &harness{
		svc: svc, store: store, clock: c, hub: hub,
		servers: servers, conversations: conversations, friends: friends,
	}
}

// seed adds an account with the default profile.
func (h *harness) seed(id uuid.UUID, nickname string) Stored {
	profile := Stored{Public: Public{
		ID:          id,
		Nickname:    nickname,
		DisplayName: nickname,
		AccentColor: DefaultAccent,
		CreatedAt:   h.clock.Now(),
	}}
	h.store.profiles[id] = profile
	return profile
}

func TestCleanDisplayName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		err  error
	}{
		{name: "plain", in: "Anna", want: "Anna"},
		{name: "trimmed", in: "  Anna  ", want: "Anna"},
		{name: "at the limit", in: strings.Repeat("я", MaxDisplayNameLength), want: strings.Repeat("я", MaxDisplayNameLength)},
		{name: "too long", in: strings.Repeat("я", MaxDisplayNameLength+1), err: ErrInvalidDisplayName},
		{name: "empty clears", in: "   ", want: ""},
		{name: "emoji", in: "🎧 Anna", want: "🎧 Anna"},
		{name: "newline", in: "An\nna", err: ErrInvalidDisplayNameChars},
		{name: "zero-width space", in: "An\u200Bna", err: ErrInvalidDisplayNameChars},
		{name: "bidi override", in: "An\u202Ena", err: ErrInvalidDisplayNameChars},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cleanDisplayName(tt.in)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if got != tt.want {
				t.Errorf("name = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCleanBio(t *testing.T) {
	tests := []struct {
		name string
		in   string
		err  error
	}{
		{name: "empty", in: ""},
		{name: "at the limit", in: strings.Repeat("я", MaxBioLength)},
		{name: "too long", in: strings.Repeat("я", MaxBioLength+1), err: ErrInvalidBio},
		{name: "newline", in: "one\ntwo", err: ErrInvalidBioChars},
		{name: "bidi", in: "one\u202Etwo", err: ErrInvalidBioChars},
		// A composed emoji needs the zero-width joiner, which stays allowed here.
		{name: "family emoji", in: "👨\u200D👩\u200D👧"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := cleanBio(tt.in); !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
		})
	}
}

func TestStatusValidation(t *testing.T) {
	tests := []struct {
		name  string
		emoji string
		text  string
		exp   Expiry
		err   error
	}{
		{name: "emoji only", emoji: "🎧", exp: Expiry30Minutes},
		{name: "text only", text: "на связи", exp: Expiry4Hours},
		{name: "both", emoji: "🎧", text: "в наушниках", exp: ExpiryEndOfDay},
		{name: "no expiry", emoji: "🎧", exp: ExpiryNever},
		{name: "empty", exp: ExpiryNever, err: ErrEmptyStatus},
		{name: "text too long", text: strings.Repeat("я", MaxStatusTextLength+1), err: ErrInvalidStatusText},
		{name: "emoji too long", emoji: strings.Repeat("я", MaxStatusEmojiLength+1), err: ErrInvalidStatusEmoji},
		{name: "control in text", text: "a\nb", err: ErrInvalidStatusChars},
		{name: "bidi in emoji", emoji: "\u202E🎧", err: ErrInvalidStatusEmoji},
		{name: "unknown expiry", emoji: "🎧", exp: Expiry("1h"), err: ErrInvalidExpiry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := StatusUpdate{Emoji: tt.emoji, Text: tt.text, ExpiresIn: tt.exp}.cleaned()
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
		})
	}
}

func TestStatusDeadlines(t *testing.T) {
	now := time.Date(2026, 10, 20, 23, 30, 0, 0, time.UTC)
	if got := Expiry30Minutes.deadline(now); got == nil || !got.Equal(now.Add(30*time.Minute)) {
		t.Errorf("30m deadline = %v, want %v", got, now.Add(30*time.Minute))
	}
	if got := Expiry4Hours.deadline(now); got == nil || !got.Equal(now.Add(4*time.Hour)) {
		t.Errorf("4h deadline = %v, want %v", got, now.Add(4*time.Hour))
	}
	want := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	if got := ExpiryEndOfDay.deadline(now); got == nil || !got.Equal(want) {
		t.Errorf("end-of-day deadline = %v, want %v (next 00:00 UTC)", got, want)
	}
	if got := ExpiryNever.deadline(now); got != nil {
		t.Errorf("never deadline = %v, want nil", got)
	}
}

func TestExpiredStatusIsNotReturned(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	seed := h.seed(user, "anna")
	past := h.clock.Now().Add(-time.Second)
	seed.CustomStatus = &CustomStatus{Emoji: "🎧", Text: "was here", ExpiresAt: &past}
	h.store.profiles[user] = seed

	profile, err := h.svc.Profile(t.Context(), user)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if profile.CustomStatus != nil {
		t.Fatalf("custom status = %+v, want it hidden after the deadline", profile.CustomStatus)
	}

	// A future status is visible.
	future := h.clock.Now().Add(time.Minute)
	seed.CustomStatus = &CustomStatus{Text: "still here", ExpiresAt: &future}
	h.store.profiles[user] = seed
	profile, err = h.svc.Profile(t.Context(), user)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if profile.CustomStatus == nil || profile.CustomStatus.Text != "still here" {
		t.Fatalf("custom status = %+v, want the live one", profile.CustomStatus)
	}
}

func TestProfilesIsOneBatch(t *testing.T) {
	h := newHarness(t)
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i, id := range ids {
		h.seed(id, "user"+string(rune('a'+i)))
	}

	profiles, err := h.svc.Profiles(t.Context(), ids)
	if err != nil {
		t.Fatalf("Profiles: %v", err)
	}
	if len(profiles) != len(ids) {
		t.Fatalf("profiles = %d, want %d", len(profiles), len(ids))
	}
	if h.store.batches != 1 {
		t.Errorf("store batches = %d, want 1", h.store.batches)
	}
}

func TestProfileIsVisibleToAcquaintancesOnly(t *testing.T) {
	h := newHarness(t)
	anna, bob, vika, dina, stranger := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{anna, bob, vika, dina, stranger} {
		h.seed(id, "user")
	}
	workspace := uuid.New()
	h.servers.servers[anna] = []uuid.UUID{workspace}
	h.servers.servers[bob] = []uuid.UUID{workspace}
	h.conversations.shared[pairKey(anna, vika)] = true
	h.friends.pairs[pairKey(anna, dina)] = true

	tests := []struct {
		name   string
		target uuid.UUID
		err    error
	}{
		{name: "self", target: anna},
		{name: "shared server", target: bob},
		{name: "shared conversation", target: vika},
		{name: "friend", target: dina},
		{name: "stranger", target: stranger, err: ErrNotFound},
		{name: "unknown account", target: uuid.New(), err: ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := h.svc.PublicProfile(t.Context(), anna, tt.target); !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
		})
	}
}

func TestPartialUpdateLeavesOtherFields(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	seed := h.seed(user, "anna")
	seed.DisplayName = "Anna"
	seed.Bio = "old bio"
	seed.AccentColor = AccentRed
	h.store.profiles[user] = seed

	bio := "new bio"
	profile, err := h.svc.Update(t.Context(), user, Update{Bio: &bio})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if profile.Bio != bio {
		t.Errorf("bio = %q, want %q", profile.Bio, bio)
	}
	if profile.DisplayName != "Anna" || profile.AccentColor != AccentRed {
		t.Errorf("profile = %+v, want the display name and accent untouched", profile)
	}

	clear := ""
	profile, err = h.svc.Update(t.Context(), user, Update{DisplayName: &clear})
	if err != nil {
		t.Fatalf("clear display name: %v", err)
	}
	if profile.DisplayName != "anna" {
		t.Errorf("display name = %q, want the nickname again", profile.DisplayName)
	}
	if profile.Bio != bio || profile.AccentColor != AccentRed {
		t.Errorf("profile = %+v, want the other fields untouched", profile)
	}

	accent := AccentSky
	profile, err = h.svc.Update(t.Context(), user, Update{AccentColor: &accent})
	if err != nil {
		t.Fatalf("set accent: %v", err)
	}
	if profile.AccentColor != AccentSky || profile.Bio != bio {
		t.Errorf("profile = %+v, want only the accent changed", profile)
	}
}

func TestUpdateRejectsEmptyUpdateAndUnknownAccent(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	h.seed(user, "anna")

	if _, err := h.svc.Update(t.Context(), user, Update{}); !errors.Is(err, ErrNothingToUpdate) {
		t.Errorf("empty update: err = %v, want ErrNothingToUpdate", err)
	}
	unknown := AccentColor("#ff0000")
	if _, err := h.svc.Update(t.Context(), user, Update{AccentColor: &unknown}); !errors.Is(err, ErrInvalidAccent) {
		t.Errorf("hex accent: err = %v, want ErrInvalidAccent", err)
	}
}

func TestStatusIsStoredWithItsDeadline(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	h.seed(user, "anna")

	profile, err := h.svc.SetStatus(t.Context(), user, StatusUpdate{
		Emoji: "🎧", Text: "на связи", ExpiresIn: Expiry30Minutes,
	})
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if profile.CustomStatus == nil || profile.CustomStatus.Emoji != "🎧" || profile.CustomStatus.Text != "на связи" {
		t.Fatalf("status = %+v, want the emoji and text", profile.CustomStatus)
	}
	want := h.clock.Now().Add(30 * time.Minute)
	if profile.CustomStatus.ExpiresAt == nil || !profile.CustomStatus.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %v, want %v", profile.CustomStatus.ExpiresAt, want)
	}

	profile, err = h.svc.ClearStatus(t.Context(), user)
	if err != nil {
		t.Fatalf("ClearStatus: %v", err)
	}
	if profile.CustomStatus != nil {
		t.Errorf("status = %+v, want it removed", profile.CustomStatus)
	}
}

func TestUserProfilesAreInTheAPIshape(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	seed := h.seed(user, "anna")
	seed.Bio = "hello"
	seed.CustomStatus = &CustomStatus{Emoji: "🎧"}
	h.store.profiles[user] = seed

	profiles, err := h.svc.UserProfiles(t.Context(), []uuid.UUID{user})
	if err != nil {
		t.Fatalf("UserProfiles: %v", err)
	}
	card, ok := profiles[user]
	if !ok {
		t.Fatal("the account is missing from the result")
	}
	if card.DisplayName != "anna" || card.Bio != "hello" || card.AccentColor != api.Slate {
		t.Errorf("card = %+v, want the public fields", card)
	}
	if card.CustomStatus == nil || card.CustomStatus.Emoji == nil || *card.CustomStatus.Emoji != "🎧" {
		t.Errorf("custom status = %+v, want the emoji", card.CustomStatus)
	}
}
