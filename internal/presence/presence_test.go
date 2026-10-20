package presence

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// clock is a hand-wound time source: the store reads it and the test moves it,
// so the idle timeout and the offline grace are never waited out for real.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestStore() (*MemoryStore, *clock) {
	c := newClock()
	return newMemoryStore(c.Now, idleAfter), c
}

func TestStatusFollowsConnections(t *testing.T) {
	store, _ := newTestStore()
	user, laptop, phone := uuid.New(), uuid.New(), uuid.New()

	if got := store.Status(user); got != StatusOffline {
		t.Fatalf("before any connection: %q, want offline", got)
	}

	store.Connect(user, laptop)
	if got := store.Status(user); got != StatusOnline {
		t.Fatalf("after connect: %q, want online", got)
	}

	store.Connect(user, phone)
	store.Disconnect(user, laptop)
	if got := store.Status(user); got != StatusOnline {
		t.Fatalf("one of two tabs closed: %q, want online", got)
	}

	store.Disconnect(user, phone)
	if got := store.Status(user); got != StatusOffline {
		t.Fatalf("last tab closed: %q, want offline", got)
	}
}

func TestIdleComesFromTheTabOrFromSilence(t *testing.T) {
	store, c := newTestStore()
	user, tab := uuid.New(), uuid.New()
	store.Connect(user, tab)

	if got := store.Status(user); got != StatusOnline {
		t.Fatalf("a fresh connection: %q, want online", got)
	}

	if err := store.SetActivity(user, tab, ActivityIdle); err != nil {
		t.Fatalf("set activity: %v", err)
	}
	if got := store.Status(user); got != StatusIdle {
		t.Fatalf("hidden tab: %q, want idle", got)
	}

	if err := store.SetActivity(user, tab, ActivityActive); err != nil {
		t.Fatalf("set activity: %v", err)
	}
	if got := store.Status(user); got != StatusOnline {
		t.Fatalf("tab in front again: %q, want online", got)
	}

	// A tab left open and untouched says nothing; silence is idle after a while.
	c.advance(idleAfter)
	if got := store.Status(user); got != StatusIdle {
		t.Fatalf("silent connection: %q, want idle", got)
	}

	// Any frame keeps it awake.
	if err := store.SetActivity(user, tab, ActivityActive); err != nil {
		t.Fatalf("set activity: %v", err)
	}
	if got := store.Status(user); got != StatusOnline {
		t.Fatalf("after a frame: %q, want online", got)
	}
}

func TestChosenStatusOverridesTheConnections(t *testing.T) {
	tests := []struct {
		name         string
		availability Availability
		connected    bool
		want         Status
	}{
		{name: "auto, no tab", availability: AvailabilityAuto, want: StatusOffline},
		{name: "auto, tab open", availability: AvailabilityAuto, connected: true, want: StatusOnline},
		{name: "online, no tab", availability: AvailabilityOnline, want: StatusOffline},
		{name: "online, tab open", availability: AvailabilityOnline, connected: true, want: StatusOnline},
		{name: "idle, no tab", availability: AvailabilityIdle, want: StatusOffline},
		{name: "idle, tab open", availability: AvailabilityIdle, connected: true, want: StatusIdle},
		{name: "do not disturb, no tab", availability: AvailabilityDnd, want: StatusOffline},
		{name: "do not disturb, tab open", availability: AvailabilityDnd, connected: true, want: StatusDnd},
		{name: "invisible, no tab", availability: AvailabilityInvisible, want: StatusOffline},
		{name: "invisible, tab open", availability: AvailabilityInvisible, connected: true, want: StatusOffline},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := newTestStore()
			user := uuid.New()
			if tc.connected {
				store.Connect(user, uuid.New())
			}
			if err := store.SetAvailability(user, tc.availability); err != nil {
				t.Fatalf("set availability: %v", err)
			}
			if got := store.Status(user); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
			if got := store.Availability(user); got != tc.availability {
				t.Fatalf("availability = %q, want %q", got, tc.availability)
			}
		})
	}
}

func TestInvisibleKeepsTheConnection(t *testing.T) {
	store, _ := newTestStore()
	user := uuid.New()
	store.Connect(user, uuid.New())

	if err := store.SetAvailability(user, AvailabilityInvisible); err != nil {
		t.Fatalf("set availability: %v", err)
	}
	if !store.Connected(user) {
		t.Fatal("invisible must not drop the connection")
	}
	if got := store.Status(user); got != StatusOffline {
		t.Fatalf("status = %q, want offline", got)
	}
}

func TestUnknownValuesAreRejected(t *testing.T) {
	store, _ := newTestStore()
	user := uuid.New()

	if err := store.SetAvailability(user, "away"); !errors.Is(err, ErrInvalidAvailability) {
		t.Fatalf("SetAvailability(away) = %v, want ErrInvalidAvailability", err)
	}
	if err := store.SetAvailability(user, ""); !errors.Is(err, ErrInvalidAvailability) {
		t.Fatalf("SetAvailability(empty) = %v, want ErrInvalidAvailability", err)
	}

	store.Connect(user, uuid.New())
	tab := uuid.New()
	if err := store.SetActivity(user, tab, "typing"); !errors.Is(err, ErrInvalidActivity) {
		t.Fatalf("SetActivity(typing) = %v, want ErrInvalidActivity", err)
	}
}

// A frame can arrive for a connection the store never saw: the client is a
// step behind a reconnect. That is not an error and must not create state.
func TestFrameForAnUnknownConnectionIsIgnored(t *testing.T) {
	store, _ := newTestStore()
	user := uuid.New()

	if err := store.SetActivity(user, uuid.New(), ActivityIdle); err != nil {
		t.Fatalf("unknown connection: %v", err)
	}
	if got := store.Status(user); got != StatusOffline {
		t.Fatalf("status = %q, want offline", got)
	}
}

func TestDisconnectedAt(t *testing.T) {
	store, c := newTestStore()
	user, tab := uuid.New(), uuid.New()

	if got := store.DisconnectedAt(user); !got.IsZero() {
		t.Fatalf("never connected: %v, want zero", got)
	}

	store.Connect(user, tab)
	if got := store.DisconnectedAt(user); !got.IsZero() {
		t.Fatalf("connected: %v, want zero", got)
	}

	closedAt := c.Now()
	store.Disconnect(user, tab)
	if got := store.DisconnectedAt(user); !got.Equal(closedAt) {
		t.Fatalf("DisconnectedAt = %v, want %v", got, closedAt)
	}

	// Reconnecting clears it, so a second close starts the grace again.
	store.Connect(user, tab)
	if got := store.DisconnectedAt(user); !got.IsZero() {
		t.Fatalf("reconnected: %v, want zero", got)
	}
}
