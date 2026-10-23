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
			if !sub.Revoked() {
				t.Errorf("%s is not marked as revoked", name)
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

// A reader that stops reading is dropped once its queue overflows; the others
// keep their stream, and the one that was dropped catches up by cursor later.
func TestSlowSubscriberIsDropped(t *testing.T) {
	hub := NewHub()
	workspace := uuid.New()
	slow := hub.Subscribe(uuid.New(), workspace)

	// One more than the queue holds: the last publish has nowhere to put it.
	for seq := range int64(queueSize + 1) {
		hub.Publish(Event{Kind: KindMessageCreated, WorkspaceID: workspace, Seq: seq + 1})
	}

	select {
	case <-slow.Done():
		if !slow.Dropped() {
			t.Error("the subscription ended but is not marked as dropped")
		}
	case <-time.After(time.Second):
		t.Fatal("the subscription survived overflowing its queue")
	}
	if hub.Subscribers(workspace) != 0 {
		t.Errorf("subscribers = %d, want 0", hub.Subscribers(workspace))
	}
}

// A reader that keeps up gets every event, in order, however many come — and
// one stalled neighbour does not cost it anything.
func TestReaderThatKeepsUpLosesNothing(t *testing.T) {
	hub := NewHub()
	workspace := uuid.New()
	stalled := hub.Subscribe(uuid.New(), workspace)
	defer stalled.Close()
	fast := hub.Subscribe(uuid.New(), workspace)
	defer fast.Close()

	for seq := range int64(queueSize * 3) {
		hub.Publish(Event{Kind: KindMessageCreated, WorkspaceID: workspace, Seq: seq + 1})

		event, ok := receive(t, fast)
		if !ok {
			t.Fatalf("the reader that keeps up was dropped at seq %d", seq+1)
		}
		if event.Seq != seq+1 {
			t.Fatalf("seq = %d, want %d: the stream has a gap", event.Seq, seq+1)
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

// The account stream follows a person, not a server: conversations and secret
// envelopes use it because they have no server to announce to.
func TestPersonalStreamReachesEveryConnectionOfOneAccount(t *testing.T) {
	hub := NewHub()
	annaID, bobID := uuid.New(), uuid.New()
	anna := hub.SubscribeUser(annaID)
	annaPhone := hub.SubscribeUser(annaID)
	bob := hub.SubscribeUser(bobID)
	defer anna.Close()
	defer annaPhone.Close()
	defer bob.Close()

	hub.PublishToUser(annaID, Event{Kind: KindConversationCreated, ChannelID: uuid.New()})

	for name, sub := range map[string]*Subscription{"laptop": anna, "phone": annaPhone} {
		event, ok := receive(t, sub)
		if !ok || event.Kind != KindConversationCreated || !event.Personal {
			t.Errorf("%s got %+v (%v), want a personal conversation event", name, event, ok)
		}
	}
	select {
	case event := <-bob.Events():
		t.Errorf("a personal event reached another account: %+v", event)
	default:
	}

	// A server event never leaks into the account stream of its members.
	hub.Publish(Event{Kind: KindMessageCreated, WorkspaceID: uuid.New(), Seq: 1})
	select {
	case event := <-anna.Events():
		t.Errorf("a server event reached the personal stream: %+v", event)
	default:
	}
}

// A subscription its own connection closed is neither slow nor revoked: the
// gateway must not tell the client it lost access.
func TestCloseIsNotARevoke(t *testing.T) {
	hub := NewHub()
	workspace, userID := uuid.New(), uuid.New()
	sub := hub.Subscribe(userID, workspace)

	sub.Close()
	hub.Revoke(workspace, userID)

	<-sub.Done()
	if sub.Dropped() || sub.Revoked() {
		t.Errorf("dropped = %v, revoked = %v, want neither", sub.Dropped(), sub.Revoked())
	}
}
