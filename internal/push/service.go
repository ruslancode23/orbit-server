// Package push owns Web Push: the subscriptions of browsers that agreed to
// notifications and the sending to them. A notification goes out only when the
// account has no Orbit tab open — an open tab shows its own — and never while
// the account asked not to be disturbed. It names who wrote and where, never
// what: the text stays in the app.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrDisabled means this instance has no VAPID keys.
	ErrDisabled = errors.New("push: not configured on this instance")
	// ErrInvalidEndpoint means the endpoint is not an https URL of a known push service.
	ErrInvalidEndpoint = errors.New("push: the endpoint is not a known push service")
	// ErrNotFound means there is no such subscription for this account.
	ErrNotFound = errors.New("push: subscription not found")
)

// pushHosts are the push services browsers use. The server sends requests to
// the endpoint a client registers, so an arbitrary address would let anyone
// make it call an internal one; only these hosts and their subdomains pass.
var pushHosts = []string{
	"fcm.googleapis.com",        // Chrome, Edge on Android, Opera
	"push.services.mozilla.com", // Firefox
	"push.apple.com",            // Safari and iOS PWA (web.push.apple.com)
	"notify.windows.com",        // Edge on Windows (wns2-*.notify.windows.com)
	"android.googleapis.com",    // older Chrome endpoints
}

const (
	// ttl is how long a push service keeps a notification for an offline
	// browser. After a day the news is old; the app shows it as unread anyway.
	ttl = 24 * 60 * 60
	// sendTimeout bounds one round of sending, so a slow push service cannot pile up goroutines.
	sendTimeout = 15 * time.Second
	// parallel is how many requests to push services run at once.
	parallel = 8
)

// Subscription is one browser of one account.
type Subscription struct {
	ID       uuid.UUID
	UserID   uuid.UUID
	Endpoint string
	P256dh   string
	Auth     string
}

// Notification is what the service worker shows. URL is a path inside the app.
type Notification struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url"`
	// Tag makes a newer notification about the same chat replace the older one.
	Tag string `json:"tag"`
}

type Store interface {
	Upsert(ctx context.Context, s Subscription) (Subscription, error)
	Delete(ctx context.Context, id, userID uuid.UUID) (bool, error)
	DeleteEndpoint(ctx context.Context, endpoint string) error
	ForUsers(ctx context.Context, userIDs []uuid.UUID) ([]Subscription, error)
	Touch(ctx context.Context, endpoint string) error
}

// Presence tells whether an account has a tab open and whether it asked not to
// be disturbed (internal/presence).
type Presence interface {
	Connected(userID uuid.UUID) bool
	DoNotDisturb(userID uuid.UUID) bool
}

// Sender delivers one encrypted notification to one browser and returns the
// push service's HTTP status.
type Sender interface {
	Send(ctx context.Context, s Subscription, payload []byte) (int, error)
}

type Service struct {
	store     Store
	presence  Presence
	sender    Sender
	publicKey string
	log       *slog.Logger

	// wg tracks rounds of sending, so a test or a shutdown can wait for them.
	wg sync.WaitGroup
}

// NewService returns a service; with an empty publicKey push is off and
// Notify does nothing.
func NewService(store Store, presence Presence, sender Sender, publicKey string, log *slog.Logger) *Service {
	return &Service{store: store, presence: presence, sender: sender, publicKey: publicKey, log: log}
}

func (s *Service) Enabled() bool { return s.publicKey != "" && s.sender != nil }

// PublicKey is the VAPID key a browser subscribes with; empty when push is off.
func (s *Service) PublicKey() string {
	if !s.Enabled() {
		return ""
	}
	return s.publicKey
}

func (s *Service) Subscribe(ctx context.Context, userID uuid.UUID, endpoint, p256dh, auth string) (Subscription, error) {
	if !s.Enabled() {
		return Subscription{}, ErrDisabled
	}
	if !knownPushService(endpoint) {
		return Subscription{}, ErrInvalidEndpoint
	}
	if strings.TrimSpace(p256dh) == "" || strings.TrimSpace(auth) == "" {
		return Subscription{}, ErrInvalidEndpoint
	}
	saved, err := s.store.Upsert(ctx, Subscription{
		ID: uuid.New(), UserID: userID, Endpoint: endpoint, P256dh: p256dh, Auth: auth,
	})
	if err != nil {
		return Subscription{}, fmt.Errorf("subscribe: %w", err)
	}
	return saved, nil
}

func (s *Service) Unsubscribe(ctx context.Context, userID, id uuid.UUID) error {
	deleted, err := s.store.Delete(ctx, id, userID)
	if err != nil {
		return fmt.Errorf("unsubscribe: %w", err)
	}
	if !deleted {
		return ErrNotFound
	}
	return nil
}

// Notify sends n to every browser of the recipients who have no tab open and
// did not ask not to be disturbed. It returns at once: the message that caused
// it is already stored, and a slow push service must not hold up its sender.
func (s *Service) Notify(ctx context.Context, recipients []uuid.UUID, n Notification) {
	if !s.Enabled() || len(recipients) == 0 {
		return
	}
	away := make([]uuid.UUID, 0, len(recipients))
	for _, id := range recipients {
		if s.presence != nil && (s.presence.Connected(id) || s.presence.DoNotDisturb(id)) {
			continue
		}
		away = append(away, id)
	}
	if len(away) == 0 {
		return
	}
	payload, err := json.Marshal(n)
	if err != nil {
		s.log.ErrorContext(ctx, "encode push notification", slog.Any("error", err))
		return
	}
	// The request is over by the time this runs; the round keeps its values but not its deadline.
	round, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		s.send(round, away, payload)
	}()
}

// Wait blocks until every round of sending started so far is over.
func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) send(ctx context.Context, userIDs []uuid.UUID, payload []byte) {
	subs, err := s.store.ForUsers(ctx, userIDs)
	if err != nil {
		s.log.ErrorContext(ctx, "load push subscriptions", slog.Any("error", err))
		return
	}
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for _, sub := range subs {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			s.deliver(ctx, sub, payload)
		}()
	}
	wg.Wait()
}

func (s *Service) deliver(ctx context.Context, sub Subscription, payload []byte) {
	status, err := s.sender.Send(ctx, sub, payload)
	if err != nil {
		s.log.WarnContext(ctx, "push send failed", slog.String("subscription_id", sub.ID.String()), slog.Any("error", err))
		return
	}
	switch {
	case status == 404 || status == 410:
		// The browser unsubscribed or the subscription expired: it will never come back.
		if err := s.store.DeleteEndpoint(ctx, sub.Endpoint); err != nil {
			s.log.WarnContext(ctx, "drop dead push subscription", slog.Any("error", err))
		}
	case status >= 200 && status < 300:
		if err := s.store.Touch(ctx, sub.Endpoint); err != nil {
			s.log.WarnContext(ctx, "touch push subscription", slog.Any("error", err))
		}
	default:
		// 429 and 5xx are the push service's trouble; the next message tries again.
		s.log.WarnContext(ctx, "push service refused",
			slog.String("subscription_id", sub.ID.String()), slog.Int("status", status))
	}
}

func knownPushService(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, known := range pushHosts {
		if host == known || strings.HasSuffix(host, "."+known) {
			return true
		}
	}
	return false
}
