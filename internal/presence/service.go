package presence

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/events"
)

const (
	// debounce is the least time between two announcements about one account.
	// A flapping tab must not turn into a storm of events for its servers.
	debounce = 5 * time.Second
	// offlineGrace delays the offline that follows the last tab closing, so a
	// page reload does not blink the name grey and back.
	offlineGrace = 30 * time.Second
	// tick is how often the loop in Run sends what has come due.
	tick = 250 * time.Millisecond
)

// Workspaces tells presence where an account may be seen: the servers it is on.
// Everyone there may know its status.
type Workspaces interface {
	Servers(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

// Friends is who else may see an account's status. Friends are a module of
// their own (SCRUM-98); until it exists this is nil and presence reaches the
// servers only.
type Friends interface {
	FriendsOf(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

// Publisher is the live stream presence announces to.
type Publisher interface {
	Publish(event events.Event)
	PublishToUser(userID uuid.UUID, event events.Event)
}

// Service turns connections into announcements. Changes are not published one
// by one: each account has an announcement due at some moment, and Run sends
// what is due, so a burst of connections still produces one event.
type Service struct {
	store      PresenceStore
	workspaces Workspaces
	friends    Friends
	hub        Publisher
	log        *slog.Logger

	now      func() time.Time
	debounce time.Duration
	grace    time.Duration
	tick     time.Duration

	mu sync.Mutex
	// due is when to look at an account again; the entry disappears once it has
	// been looked at.
	due map[uuid.UUID]time.Time
	// sent is the last announcement and when it went, per account.
	sent map[uuid.UUID]announcement
}

type announcement struct {
	status Status
	at     time.Time
}

func NewService(store PresenceStore, workspaces Workspaces, hub Publisher, log *slog.Logger) *Service {
	return &Service{
		store:      store,
		workspaces: workspaces,
		hub:        hub,
		log:        log,
		now:        time.Now,
		debounce:   debounce,
		grace:      offlineGrace,
		tick:       tick,
		due:        map[uuid.UUID]time.Time{},
		sent:       map[uuid.UUID]announcement{},
	}
}

// SetFriends wires in who may see an account besides its servers. Optional.
func (s *Service) SetFriends(friends Friends) {
	s.friends = friends
}

// Connect registers a new live connection of an account. The gateway calls it
// once per WebSocket.
func (s *Service) Connect(userID, connectionID uuid.UUID) {
	s.store.Connect(userID, connectionID)
	s.schedule(userID)
}

// Disconnect forgets a connection. The account goes offline only after the
// offline grace, and only if no other tab is open by then.
func (s *Service) Disconnect(userID, connectionID uuid.UUID) {
	s.store.Disconnect(userID, connectionID)
	s.schedule(userID)
}

// SetActivity records whether one tab is in front. This is not a chosen status:
// it only feeds the automatic one.
func (s *Service) SetActivity(userID, connectionID uuid.UUID, activity Activity) error {
	if err := s.store.SetActivity(userID, connectionID, activity); err != nil {
		return err
	}
	s.schedule(userID)
	return nil
}

// SetAvailability records the status the account picked for itself.
func (s *Service) SetAvailability(userID uuid.UUID, availability Availability) error {
	if err := s.store.SetAvailability(userID, availability); err != nil {
		return err
	}
	s.schedule(userID)
	return nil
}

// Statuses is what a member list shows: the status of every account asked
// about, as other people see it. Unknown accounts are offline.
func (s *Service) Statuses(userIDs []uuid.UUID) map[uuid.UUID]api.PresenceStatus {
	out := make(map[uuid.UUID]api.PresenceStatus, len(userIDs))
	for _, userID := range userIDs {
		out[userID] = api.PresenceStatus(s.store.Status(userID))
	}
	return out
}

// Run sends what is due until the server stops. Presence has no other clock:
// the debounce and the offline grace are measured here.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.flush(ctx)
		}
	}
}

// schedule asks for another look at one account. Nothing is sent now: the
// announcement waits out the debounce, and an account without a connection
// waits out the offline grace. Asking twice is cheap and never moves a pending
// announcement later.
func (s *Service) schedule(userID uuid.UUID) {
	now := s.now()
	at := now

	s.mu.Lock()
	defer s.mu.Unlock()

	if previous, ok := s.sent[userID]; ok && previous.at.Add(s.debounce).After(at) {
		at = previous.at.Add(s.debounce)
	}
	// The last tab closing is the only reason to wait: a connection that is
	// still open is here and now.
	if !s.store.Connected(userID) {
		if closedAt := s.store.DisconnectedAt(userID); !closedAt.IsZero() && closedAt.Add(s.grace).After(at) {
			at = closedAt.Add(s.grace)
		}
	}
	if pending, ok := s.due[userID]; !ok || at.Before(pending) {
		s.due[userID] = at
	}
}

// flush sends every announcement that has come due. It is the only place that
// publishes, so two changes close in time cannot arrive out of order.
func (s *Service) flush(ctx context.Context) {
	now := s.now()

	s.mu.Lock()
	ready := make([]uuid.UUID, 0, len(s.due))
	for userID, at := range s.due {
		if !at.After(now) {
			ready = append(ready, userID)
			delete(s.due, userID)
		}
	}
	s.mu.Unlock()

	for _, userID := range ready {
		s.announce(ctx, userID)
	}
}

// announce publishes the current status of one account if it differs from what
// its audience was last told.
func (s *Service) announce(ctx context.Context, userID uuid.UUID) {
	status := s.store.Status(userID)

	s.mu.Lock()
	previous, known := s.sent[userID]
	if known && previous.status == status {
		s.mu.Unlock()
		return
	}
	now := s.now()
	s.sent[userID] = announcement{status: status, at: now}
	s.mu.Unlock()

	s.publish(ctx, userID, status, now)
}

// publish tells everyone who may know: the servers the account is on and, once
// friends exist, the friends. Invisible has already become offline in the
// status, so the payload never says more than the audience may know.
func (s *Service) publish(ctx context.Context, userID uuid.UUID, status Status, at time.Time) {
	state := State{UserID: userID, Status: status, UpdatedAt: at}

	servers, err := s.workspaces.Servers(ctx, userID)
	if err != nil {
		s.log.WarnContext(ctx, "list servers for presence", slog.Any("error", err))
	}
	for _, workspaceID := range servers {
		s.hub.Publish(events.Event{
			Kind:        events.KindPresenceUpdated,
			WorkspaceID: workspaceID,
			Payload:     state,
		})
	}

	if s.friends == nil {
		return
	}
	friends, err := s.friends.FriendsOf(ctx, userID)
	if err != nil {
		s.log.WarnContext(ctx, "list friends for presence", slog.Any("error", err))
		return
	}
	for _, friendID := range friends {
		s.hub.PublishToUser(friendID, events.Event{
			Kind:    events.KindPresenceUpdated,
			Payload: state,
		})
	}
}
