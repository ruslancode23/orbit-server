package presence

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/events"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeWorkspaces struct {
	servers map[uuid.UUID][]uuid.UUID
	err     error
}

func (f *fakeWorkspaces) Servers(_ context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.servers[userID], nil
}

type fakeFriends struct {
	friends map[uuid.UUID][]uuid.UUID
}

func (f *fakeFriends) FriendsOf(_ context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	return f.friends[userID], nil
}

type personalEvent struct {
	userID uuid.UUID
	event  events.Event
}

type recorder struct {
	mu       sync.Mutex
	servers  []events.Event
	personal []personalEvent
}

func (r *recorder) Publish(event events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.servers = append(r.servers, event)
}

func (r *recorder) PublishToUser(userID uuid.UUID, event events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.personal = append(r.personal, personalEvent{userID: userID, event: event})
}

func (r *recorder) serverEvents() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.servers...)
}

func (r *recorder) personalEvents() []personalEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]personalEvent(nil), r.personal...)
}

type harness struct {
	svc        *Service
	store      *MemoryStore
	clock      *clock
	rec        *recorder
	workspaces *fakeWorkspaces
	friends    *fakeFriends
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, c := newTestStore()
	rec := &recorder{}
	workspaces := &fakeWorkspaces{servers: map[uuid.UUID][]uuid.UUID{}}
	friends := &fakeFriends{friends: map[uuid.UUID][]uuid.UUID{}}

	svc := NewService(store, workspaces, rec, testLogger())
	svc.now = c.Now
	svc.SetFriends(friends)
	return &harness{svc: svc, store: store, clock: c, rec: rec, workspaces: workspaces, friends: friends}
}

func (h *harness) statuses(ev events.Event) Status {
	state, ok := ev.Payload.(State)
	if !ok {
		return ""
	}
	return state.Status
}

func TestAnnounceIsDebounced(t *testing.T) {
	h := newHarness(t)
	user, workspace := uuid.New(), uuid.New()
	h.workspaces.servers[user] = []uuid.UUID{workspace}

	// A thousand tabs of one account: one event, not a thousand.
	for range 1000 {
		h.svc.Connect(user, uuid.New())
	}
	h.svc.flush(t.Context())

	got := h.rec.serverEvents()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if status := h.statuses(got[0]); status != StatusOnline {
		t.Fatalf("status = %q, want online", status)
	}
	if got[0].Kind != events.KindPresenceUpdated || got[0].WorkspaceID != workspace {
		t.Fatalf("event = %+v, want presence.updated in the account's server", got[0])
	}

	// Anything that happens inside the window waits for it.
	h.clock.advance(time.Second)
	for range 1000 {
		h.svc.Connect(user, uuid.New())
	}
	h.svc.flush(t.Context())
	if got := h.rec.serverEvents(); len(got) != 1 {
		t.Fatalf("events inside the window = %d, want 1", len(got))
	}

	// Nothing changed, so the window passing changes nothing either.
	h.clock.advance(debounce)
	h.svc.flush(t.Context())
	if got := h.rec.serverEvents(); len(got) != 1 {
		t.Fatalf("events after the window = %d, want 1", len(got))
	}
}

func TestFlappingTabPublishesTheLastStateOnce(t *testing.T) {
	h := newHarness(t)
	user, tab := uuid.New(), uuid.New()
	h.workspaces.servers[user] = []uuid.UUID{uuid.New()}

	h.svc.Connect(user, tab)
	h.clock.advance(debounce)
	h.svc.flush(t.Context())

	// Three changes in a row: one event at the end of the window, carrying the
	// state it settled on.
	for _, availability := range []Availability{AvailabilityDnd, AvailabilityIdle, AvailabilityDnd} {
		if err := h.svc.SetAvailability(user, availability); err != nil {
			t.Fatalf("set availability: %v", err)
		}
		h.clock.advance(time.Second)
	}
	h.svc.flush(t.Context())
	h.clock.advance(debounce)
	h.svc.flush(t.Context())

	got := h.rec.serverEvents()
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2", len(got))
	}
	if status := h.statuses(got[1]); status != StatusDnd {
		t.Fatalf("last status = %q, want dnd", status)
	}
}

func TestOfflineWaitsForTheGrace(t *testing.T) {
	h := newHarness(t)
	user, tab := uuid.New(), uuid.New()
	h.workspaces.servers[user] = []uuid.UUID{uuid.New()}

	h.svc.Connect(user, tab)
	h.svc.flush(t.Context())
	if got := h.rec.serverEvents(); len(got) != 1 {
		t.Fatalf("events after connect = %d, want 1", len(got))
	}

	h.clock.advance(debounce)
	h.svc.Disconnect(user, tab)

	// A page reload looks exactly like this: the socket is gone for a moment.
	h.svc.flush(t.Context())
	if got := h.rec.serverEvents(); len(got) != 1 {
		t.Fatalf("events right after the last tab closed = %d, want 1", len(got))
	}

	h.clock.advance(offlineGrace - time.Second)
	h.svc.flush(t.Context())
	if got := h.rec.serverEvents(); len(got) != 1 {
		t.Fatalf("events before the grace is over = %d, want 1", len(got))
	}

	h.clock.advance(2 * time.Second)
	h.svc.flush(t.Context())
	got := h.rec.serverEvents()
	if len(got) != 2 {
		t.Fatalf("events after the grace = %d, want 2", len(got))
	}
	if status := h.statuses(got[1]); status != StatusOffline {
		t.Fatalf("status = %q, want offline", status)
	}
}

func TestReloadDoesNotBlinkOffline(t *testing.T) {
	h := newHarness(t)
	user, laptop, phone := uuid.New(), uuid.New(), uuid.New()
	h.workspaces.servers[user] = []uuid.UUID{uuid.New()}

	h.svc.Connect(user, laptop)
	h.svc.flush(t.Context())

	h.clock.advance(10 * time.Second)
	h.svc.Disconnect(user, laptop)
	h.clock.advance(5 * time.Second)
	h.svc.Connect(user, phone)

	// Well past the grace: the audience never heard about the gap.
	h.clock.advance(time.Minute)
	h.svc.flush(t.Context())

	got := h.rec.serverEvents()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1: a reload must not blink", len(got))
	}
	if status := h.statuses(got[0]); status != StatusOnline {
		t.Fatalf("status = %q, want online", status)
	}
}

func TestInvisibleIsOfflineToEveryone(t *testing.T) {
	h := newHarness(t)
	user, tab := uuid.New(), uuid.New()
	h.workspaces.servers[user] = []uuid.UUID{uuid.New()}

	h.svc.Connect(user, tab)
	if err := h.svc.SetAvailability(user, AvailabilityInvisible); err != nil {
		t.Fatalf("set availability: %v", err)
	}
	h.svc.flush(t.Context())

	got := h.rec.serverEvents()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if status := h.statuses(got[0]); status != StatusOffline {
		t.Fatalf("status = %q, want offline", status)
	}
}

// Nobody who shares no server with the account learns anything about it.
func TestStrangerSeesNothing(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()

	h.svc.Connect(user, uuid.New())
	h.svc.flush(t.Context())

	if got := h.rec.serverEvents(); len(got) != 0 {
		t.Fatalf("events for an account with no servers = %d, want 0", len(got))
	}
	if got := h.rec.personalEvents(); len(got) != 0 {
		t.Fatalf("personal events for an account with no friends = %d, want 0", len(got))
	}
}

func TestFriendsGetThePersonalStream(t *testing.T) {
	h := newHarness(t)
	user, anna, bob := uuid.New(), uuid.New(), uuid.New()
	h.friends.friends[user] = []uuid.UUID{anna, bob}

	h.svc.Connect(user, uuid.New())
	h.svc.flush(t.Context())

	got := h.rec.personalEvents()
	if len(got) != 2 {
		t.Fatalf("personal events = %d, want 2", len(got))
	}
	seen := map[uuid.UUID]bool{}
	for _, event := range got {
		seen[event.userID] = true
		if event.event.Kind != events.KindPresenceUpdated {
			t.Fatalf("kind = %q, want presence.updated", event.event.Kind)
		}
	}
	if !seen[anna] || !seen[bob] {
		t.Fatalf("friends reached = %v, want anna and bob", seen)
	}
}

func TestStatusesForTheMemberList(t *testing.T) {
	h := newHarness(t)
	online, dnd, stranger := uuid.New(), uuid.New(), uuid.New()

	h.svc.Connect(online, uuid.New())
	h.svc.Connect(dnd, uuid.New())
	if err := h.svc.SetAvailability(dnd, AvailabilityDnd); err != nil {
		t.Fatalf("set availability: %v", err)
	}

	got := h.svc.Statuses([]uuid.UUID{online, dnd, stranger})
	want := map[uuid.UUID]api.PresenceStatus{
		online:   api.Online,
		dnd:      api.Dnd,
		stranger: api.Offline,
	}
	for userID, status := range want {
		if got[userID] != status {
			t.Fatalf("status of %s = %q, want %q", userID, got[userID], status)
		}
	}
}

// Push asks two things: is a tab open, and did the account ask for quiet.
func TestConnectedAndDoNotDisturbForPush(t *testing.T) {
	h := newHarness(t)
	anna, conn := uuid.New(), uuid.New()

	if h.svc.Connected(anna) || h.svc.DoNotDisturb(anna) {
		t.Fatal("an account that never connected is neither connected nor in «do not disturb»")
	}
	h.svc.Connect(anna, conn)
	if !h.svc.Connected(anna) {
		t.Error("Connected = false with a tab open")
	}
	if err := h.svc.SetAvailability(anna, AvailabilityDnd); err != nil {
		t.Fatal(err)
	}
	h.svc.Disconnect(anna, conn)
	if h.svc.Connected(anna) {
		t.Error("Connected = true after the last tab closed")
	}
	if !h.svc.DoNotDisturb(anna) {
		t.Error("DoNotDisturb = false after picking it, even with no tab open")
	}
}
