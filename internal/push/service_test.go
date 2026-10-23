package push

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"
)

type fakeStore struct {
	mu      sync.Mutex
	subs    map[string]Subscription // by endpoint
	touched []string
}

func newFakeStore() *fakeStore { return &fakeStore{subs: map[string]Subscription{}} }

func (f *fakeStore) Upsert(_ context.Context, s Subscription) (Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.subs[s.Endpoint]; ok {
		s.ID = old.ID
	}
	f.subs[s.Endpoint] = s
	return s, nil
}

func (f *fakeStore) Delete(_ context.Context, id, userID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for endpoint, s := range f.subs {
		if s.ID == id && s.UserID == userID {
			delete(f.subs, endpoint)
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) DeleteEndpoint(_ context.Context, endpoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.subs, endpoint)
	return nil
}

func (f *fakeStore) ForUsers(_ context.Context, userIDs []uuid.UUID) ([]Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[uuid.UUID]bool{}
	for _, id := range userIDs {
		want[id] = true
	}
	var out []Subscription
	for _, s := range f.subs {
		if want[s.UserID] {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeStore) Touch(_ context.Context, endpoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, endpoint)
	return nil
}

func (f *fakeStore) has(endpoint string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.subs[endpoint]
	return ok
}

type fakePresence struct {
	connected map[uuid.UUID]bool
	dnd       map[uuid.UUID]bool
}

func (p fakePresence) Connected(id uuid.UUID) bool    { return p.connected[id] }
func (p fakePresence) DoNotDisturb(id uuid.UUID) bool { return p.dnd[id] }

type fakeSender struct {
	mu     sync.Mutex
	sent   map[string][]byte // payload by endpoint
	status map[string]int    // answer by endpoint, 201 by default
}

func newFakeSender() *fakeSender {
	return &fakeSender{sent: map[string][]byte{}, status: map[string]int{}}
}

func (f *fakeSender) Send(_ context.Context, s Subscription, payload []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[s.Endpoint] = payload
	if status, ok := f.status[s.Endpoint]; ok {
		return status, nil
	}
	return 201, nil
}

func (f *fakeSender) endpoints() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for endpoint := range f.sent {
		out[endpoint] = true
	}
	return out
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const fcm = "https://fcm.googleapis.com/fcm/send/"

func TestSubscribeAcceptsOnlyKnownPushServices(t *testing.T) {
	tests := []struct {
		endpoint string
		ok       bool
	}{
		{fcm + "abc", true},
		{"https://updates.push.services.mozilla.com/wpush/v2/abc", true},
		{"https://web.push.apple.com/QH8abc", true},
		{"https://wns2-par02p.notify.windows.com/w/?token=abc", true},
		{"http://fcm.googleapis.com/fcm/send/abc", false},            // not https
		{"https://fcm.googleapis.com:8443/fcm/send/abc", false},      // other port
		{"https://user@fcm.googleapis.com/fcm/send/abc", false},      // credentials
		{"https://evilfcm.googleapis.com.attacker.example/x", false}, // suffix trick
		{"https://169.254.169.254/latest/meta-data", false},          // internal address
		{"https://localhost/push", false},
		{"not a url", false},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			svc := NewService(newFakeStore(), nil, newFakeSender(), "public-key", discard())
			_, err := svc.Subscribe(context.Background(), uuid.New(), tt.endpoint, "p256dh", "auth")
			if tt.ok && err != nil {
				t.Fatalf("Subscribe() error = %v, want nil", err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidEndpoint) {
				t.Fatalf("Subscribe() error = %v, want ErrInvalidEndpoint", err)
			}
		})
	}
}

func TestSubscribeWithoutKeysIsDisabled(t *testing.T) {
	svc := NewService(newFakeStore(), nil, nil, "", discard())
	if svc.Enabled() || svc.PublicKey() != "" {
		t.Fatal("push without VAPID keys should be off")
	}
	if _, err := svc.Subscribe(context.Background(), uuid.New(), fcm+"a", "k", "a"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Subscribe() error = %v, want ErrDisabled", err)
	}
}

func TestUnsubscribeOnlyOwnSubscription(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, nil, newFakeSender(), "public-key", discard())
	owner, stranger := uuid.New(), uuid.New()
	sub, err := svc.Subscribe(context.Background(), owner, fcm+"a", "k", "a")
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Unsubscribe(context.Background(), stranger, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger Unsubscribe() error = %v, want ErrNotFound", err)
	}
	if !store.has(fcm + "a") {
		t.Fatal("a stranger removed someone else's subscription")
	}
	if err := svc.Unsubscribe(context.Background(), owner, sub.ID); err != nil {
		t.Fatalf("owner Unsubscribe() error = %v", err)
	}
	if store.has(fcm + "a") {
		t.Fatal("the subscription is still there")
	}
}

func TestNotifyReachesOnlyPeopleAwayAndNotDisturbed(t *testing.T) {
	store := newFakeStore()
	sender := newFakeSender()
	away, online, quiet := uuid.New(), uuid.New(), uuid.New()
	presence := fakePresence{
		connected: map[uuid.UUID]bool{online: true},
		dnd:       map[uuid.UUID]bool{quiet: true},
	}
	svc := NewService(store, presence, sender, "public-key", discard())
	ctx := context.Background()
	for i, id := range []uuid.UUID{away, online, quiet} {
		if _, err := svc.Subscribe(ctx, id, fcm+string(rune('a'+i)), "k", "a"); err != nil {
			t.Fatal(err)
		}
	}

	n := Notification{Title: "Анна", Body: "Новое сообщение", URL: "/me/1", Tag: "1"}
	svc.Notify(ctx, []uuid.UUID{away, online, quiet}, n)
	svc.Wait()

	got := sender.endpoints()
	if !got[fcm+"a"] || got[fcm+"b"] || got[fcm+"c"] {
		t.Fatalf("sent to %v, want only the account with no tab open and no «do not disturb»", got)
	}
	var payload Notification
	if err := json.Unmarshal(sender.sent[fcm+"a"], &payload); err != nil {
		t.Fatal(err)
	}
	if payload != n {
		t.Fatalf("payload = %+v, want %+v", payload, n)
	}
	if len(store.touched) != 1 {
		t.Fatalf("touched %v, want the delivered subscription", store.touched)
	}
}

func TestNotifyDropsSubscriptionsThePushServiceForgot(t *testing.T) {
	store := newFakeStore()
	sender := newFakeSender()
	svc := NewService(store, fakePresence{}, sender, "public-key", discard())
	ctx := context.Background()
	user := uuid.New()
	for _, endpoint := range []string{fcm + "gone", fcm + "expired", fcm + "busy", fcm + "fine"} {
		if _, err := svc.Subscribe(ctx, user, endpoint, "k", "a"); err != nil {
			t.Fatal(err)
		}
	}
	sender.status[fcm+"gone"] = 404
	sender.status[fcm+"expired"] = 410
	sender.status[fcm+"busy"] = 429

	svc.Notify(ctx, []uuid.UUID{user}, Notification{Title: "x", URL: "/", Tag: "t"})
	svc.Wait()

	for endpoint, want := range map[string]bool{fcm + "gone": false, fcm + "expired": false, fcm + "busy": true, fcm + "fine": true} {
		if store.has(endpoint) != want {
			t.Errorf("subscription %s kept = %v, want %v", endpoint, !want, want)
		}
	}
}

func TestNotifyWhenDisabledSendsNothing(t *testing.T) {
	sender := newFakeSender()
	svc := NewService(newFakeStore(), fakePresence{}, sender, "", discard())
	svc.Notify(context.Background(), []uuid.UUID{uuid.New()}, Notification{Title: "x"})
	svc.Wait()
	if len(sender.endpoints()) != 0 {
		t.Fatal("a disabled service sent a push")
	}
}
