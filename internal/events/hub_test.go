package events

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// receive waits briefly for one event; tests must not hang on a missing one.
func receive(t *testing.T, sub *Subscription) (Event, bool) {
	t.Helper()
	select {
	case event := <-sub.Events():
		return event, true
	case <-sub.Done():
		return Event{}, false
	case <-time.After(time.Second):
		t.Fatal("no event within a second")
		return Event{}, false
	}
}

func TestPublishReachesEverySubscriberOfThatServer(t *testing.T) {
	hub := NewHub()
	workspace, other := uuid.New(), uuid.New()
	anna := hub.Subscribe(uuid.New(), workspace)
	bob := hub.Subscribe(uuid.New(), workspace)
	elsewhere := hub.Subscribe(uuid.New(), other)
	defer anna.Close()
	defer bob.Close()
	defer elsewhere.Close()

	hub.Publish(Event{Kind: KindMessageCreated, WorkspaceID: workspace, Seq: 7})

	for name, sub := range map[string]*Subscription{"anna": anna, "bob": bob} {
		event, ok := receive(t, sub)
		if !ok || event.Seq != 7 {
			t.Errorf("%s got %+v (%v), want the event", name, event, ok)
		}
	}

	// A server someone else watches hears nothing.
	select {
	case event := <-elsewhere.Events():
		t.Errorf("an event leaked to another server: %+v", event)
	default:
	}
}

func TestRevokeEndsOnlyThatAccountsSubscriptions(t *testing.T) {
	hub := NewHub()
	workspace := uuid.New()
	annaID := uuid.New()
	anna := hub.Subscribe(annaID, workspace)
	annaPhone := hub.Subscribe(annaID, workspace)
	bob := hub.Subscribe(uuid.New(), workspace)
	defer bob.Close()

	hub.Revoke(workspace, annaID)

	for name, sub := range map[string]*Subscription{"anna": anna, "anna's phone": annaPhone} {
		select {
		case <-sub.Done():
			if sub.Dropped() {
				t.Errorf("%s is marked as slow, but it was revoked", name)
			}
		case <-time.After(time.Second):
			t.Errorf("%s is still subscribed after the revoke", name)
		}
	}

	select {
	case <-bob.Done():
		t.Error("bob lost his subscription when anna was revoked")
	default:
	}
	if hub.Subscribers(workspace) != 1 {
		t.Errorf("subscribers = %d, want 1", hub.Subscribers(workspace))
	}
}

// One reader that stops reading must not hold up the others: it is dropped and
// catches up by cursor when it reconnects.
func TestSlowSubscriberIsDroppedAndTheRestKeepGoing(t *testing.T) {
	hub := NewHub()
	workspace := uuid.New()
	slow := hub.Subscribe(uuid.New(), workspace)
	fast := hub.Subscribe(uuid.New(), workspace)
	defer fast.Close()

	received := make(chan int64, queueSize*4)
	go func() {
		for {
			select {
			case event := <-fast.Events():
				received <- event.Seq
			case <-fast.Done():
				return
			}
		}
	}()

	for seq := range int64(queueSize * 3) {
		hub.Publish(Event{Kind: KindMessageCreated, WorkspaceID: workspace, Seq: seq + 1})
	}

	select {
	case <-slow.Done():
		if !slow.Dropped() {
			t.Error("the slow subscription ended but is not marked as dropped")
		}
	case <-time.After(time.Second):
		t.Fatal("the slow subscription is still alive after overflowing its queue")
	}

	// The fast reader saw everything.
	deadline := time.After(2 * time.Second)
	var last int64
	for last < int64(queueSize*3) {
		select {
		case seq := <-received:
			if seq != last+1 {
				t.Fatalf("got seq %d after %d: the stream has a gap", seq, last)
			}
			last = seq
		case <-deadline:
			t.Fatalf("the fast reader stopped at %d of %d", last, queueSize*3)
		}
	}
}

func TestCloseIsIdempotentAndUnsubscribes(t *testing.T) {
	hub := NewHub()
	workspace := uuid.New()
	sub := hub.Subscribe(uuid.New(), workspace)

	sub.Close()
	sub.Close()

	if hub.Subscribers(workspace) != 0 {
		t.Errorf("subscribers = %d, want 0", hub.Subscribers(workspace))
	}
	// Publishing to nobody is fine.
	hub.Publish(Event{Kind: KindMessageCreated, WorkspaceID: workspace})
}

// Publishing while a subscription is closing must not panic.
func TestPublishRacingWithClose(t *testing.T) {
	hub := NewHub()
	workspace := uuid.New()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			hub.Publish(Event{Kind: KindMessageCreated, WorkspaceID: workspace, Seq: 1})
		}
	}()

	for range 50 {
		sub := hub.Subscribe(uuid.New(), workspace)
		sub.Close()
	}
	<-done
}
