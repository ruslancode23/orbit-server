package profile

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/events"
)

// A profile change reaches every server and every conversation of the account
// once, and the debounce folds a burst of edits into one announcement.
func TestUserUpdatedIsDebouncedAndFannedOut(t *testing.T) {
	h := newHarness(t)
	anna, bob, dina := uuid.New(), uuid.New(), uuid.New()
	h.seed(anna, "anna")
	workspace := uuid.New()
	h.servers.servers[anna] = []uuid.UUID{workspace}
	h.conversations.peers[anna] = []uuid.UUID{bob}
	h.friends.friends[anna] = []uuid.UUID{dina}

	bio := "first"
	if _, err := h.svc.Update(t.Context(), anna, Update{Bio: &bio}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	h.svc.flush(t.Context())

	servers := h.hub.serverEvents()
	if len(servers) != 1 {
		t.Fatalf("server events = %d, want 1", len(servers))
	}
	if servers[0].Kind != events.KindUserUpdated || servers[0].WorkspaceID != workspace {
		t.Fatalf("event = %+v, want user.updated on the account's server", servers[0])
	}
	payload, ok := servers[0].Payload.(api.UserProfile)
	if !ok {
		t.Fatalf("payload = %T, want api.UserProfile", servers[0].Payload)
	}
	if payload.Id != anna || payload.Bio != "first" || payload.DisplayName != "anna" {
		t.Fatalf("payload = %+v, want the current profile", payload)
	}

	// The account's other tabs, the conversation peer and the friend; nobody else.
	audience := map[uuid.UUID]bool{}
	for _, event := range h.hub.personalEvents() {
		audience[event.userID] = true
		if event.event.Kind != events.KindUserUpdated {
			t.Errorf("personal kind = %q, want user.updated", event.event.Kind)
		}
	}
	for _, id := range []uuid.UUID{anna, bob, dina} {
		if !audience[id] {
			t.Errorf("audience misses %s", id)
		}
	}
	if len(audience) != 3 {
		t.Errorf("audience = %v, want exactly the account, its peer and its friend", audience)
	}

	// Inside the window nothing goes out, even though the profile did change.
	h.clock.advance(debounce / 2)
	bio = "second"
	if _, err := h.svc.Update(t.Context(), anna, Update{Bio: &bio}); err != nil {
		t.Fatalf("Update inside the window: %v", err)
	}
	h.svc.flush(t.Context())
	if got := len(h.hub.serverEvents()); got != 1 {
		t.Fatalf("server events inside the window = %d, want 1", got)
	}
	if got := len(h.hub.personalEvents()); got != 3 {
		t.Fatalf("personal events inside the window = %d, want 3", got)
	}

	// When the window passes, the announcement carries the latest state.
	h.clock.advance(debounce - debounce/2)
	h.svc.flush(t.Context())
	servers = h.hub.serverEvents()
	if len(servers) != 2 {
		t.Fatalf("server events after the window = %d, want 2", len(servers))
	}
	payload, ok = servers[1].Payload.(api.UserProfile)
	if !ok || payload.Bio != "second" {
		t.Fatalf("second payload = %+v, want the latest bio", servers[1].Payload)
	}
	if got := len(h.hub.personalEvents()); got != 6 {
		t.Fatalf("personal events after the window = %d, want 6", got)
	}
}

// An account nobody shares a server or a conversation with still gets its own
// tabs told; a stranger never hears anything.
func TestUserUpdatedReachesNobodyElse(t *testing.T) {
	h := newHarness(t)
	anna := uuid.New()
	h.seed(anna, "anna")

	text := "hello"
	if _, err := h.svc.Update(t.Context(), anna, Update{Bio: &text}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	h.svc.flush(t.Context())

	if got := h.hub.serverEvents(); len(got) != 0 {
		t.Fatalf("server events = %d, want none", len(got))
	}
	events := h.hub.personalEvents()
	if len(events) != 1 || events[0].userID != anna {
		t.Fatalf("personal events = %+v, want exactly the account itself", events)
	}
}

func TestNoAnnouncementIsScheduledByARejectedUpdate(t *testing.T) {
	h := newHarness(t)
	anna := uuid.New()
	h.seed(anna, "anna")

	tooLong := make([]rune, MaxDisplayNameLength+1)
	for i := range tooLong {
		tooLong[i] = 'я'
	}
	name := string(tooLong)
	if _, err := h.svc.Update(t.Context(), anna, Update{DisplayName: &name}); err == nil {
		t.Fatal("the over-long name was accepted")
	}
	h.clock.advance(debounce)
	h.svc.flush(t.Context())

	if got := h.hub.personalEvents(); len(got) != 0 {
		t.Fatalf("personal events = %d, want none", len(got))
	}
}

func TestDebounceDoesNotMoveAPendingAnnouncementLater(t *testing.T) {
	h := newHarness(t)
	anna := uuid.New()
	h.seed(anna, "anna")
	h.servers.servers[anna] = []uuid.UUID{uuid.New()}

	text := "one"
	if _, err := h.svc.Update(t.Context(), anna, Update{Bio: &text}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	h.svc.flush(t.Context())

	// A second edit right after the first schedules at last+debounce. A third
	// one inside that window must not push it further out.
	h.clock.advance(time.Second)
	text = "two"
	if _, err := h.svc.Update(t.Context(), anna, Update{Bio: &text}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	h.clock.advance(debounce / 4)
	text = "three"
	if _, err := h.svc.Update(t.Context(), anna, Update{Bio: &text}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	h.svc.flush(t.Context())
	if got := len(h.hub.serverEvents()); got != 1 {
		t.Fatalf("server events before the window = %d, want 1", got)
	}
	h.clock.advance(debounce - time.Second - debounce/4)
	h.svc.flush(t.Context())
	if got := len(h.hub.serverEvents()); got != 2 {
		t.Fatalf("server events at the end of the window = %d, want 2", got)
	}
	if payload, ok := h.hub.serverEvents()[1].Payload.(api.UserProfile); !ok || payload.Bio != "three" {
		t.Fatalf("payload = %+v, want the latest edit", h.hub.serverEvents()[1].Payload)
	}
}
