package events

import (
	"sync"

	"github.com/google/uuid"
)

// queueSize is how many events one connection may fall behind before the hub
// gives up on it. A client that cannot keep up is closed and catches up by
// cursor when it reconnects — one slow reader must not hold up the others.
const queueSize = 64

// Hub fans events out to the connections watching a server, in this process.
// One instance is shared by the whole server; it is safe for concurrent use.
type Hub struct {
	mu sync.RWMutex
	// subscriptions are grouped by server, then by subscription, so that
	// publishing to a server and revoking one person are both cheap.
	byWorkspace map[uuid.UUID]map[*Subscription]struct{}
}

func NewHub() *Hub {
	return &Hub{byWorkspace: map[uuid.UUID]map[*Subscription]struct{}{}}
}

// Subscription is one connection watching one server.
type Subscription struct {
	hub         *Hub
	userID      uuid.UUID
	workspaceID uuid.UUID

	events chan Event
	// done is closed when the subscription ends, whatever ends it.
	done chan struct{}
	once sync.Once
	// dropped says the subscription was closed because it fell behind.
	dropped bool
	mu      sync.Mutex
}

// Subscribe starts watching one server. The caller must have checked that this
// account may read it; the hub only delivers.
func (h *Hub) Subscribe(userID, workspaceID uuid.UUID) *Subscription {
	sub := &Subscription{
		hub:         h,
		userID:      userID,
		workspaceID: workspaceID,
		events:      make(chan Event, queueSize),
		done:        make(chan struct{}),
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byWorkspace[workspaceID] == nil {
		h.byWorkspace[workspaceID] = map[*Subscription]struct{}{}
	}
	h.byWorkspace[workspaceID][sub] = struct{}{}
	return sub
}

// Publish delivers an event to everyone watching that server. It never blocks:
// a subscription whose queue is full is closed instead.
func (h *Hub) Publish(event Event) {
	h.mu.RLock()
	subs := make([]*Subscription, 0, len(h.byWorkspace[event.WorkspaceID]))
	for sub := range h.byWorkspace[event.WorkspaceID] {
		subs = append(subs, sub)
	}
	h.mu.RUnlock()

	for _, sub := range subs {
		select {
		case sub.events <- event:
		default:
			sub.closeSlow()
		}
	}
}

// Revoke ends every subscription of one account on one server. A ban or a lost
// role takes effect at once, without waiting for the client to reconnect.
func (h *Hub) Revoke(workspaceID, userID uuid.UUID) {
	h.mu.RLock()
	var subs []*Subscription
	for sub := range h.byWorkspace[workspaceID] {
		if sub.userID == userID {
			subs = append(subs, sub)
		}
	}
	h.mu.RUnlock()

	for _, sub := range subs {
		sub.Close()
	}
}

// Subscribers reports how many connections watch a server. For tests and metrics.
func (h *Hub) Subscribers(workspaceID uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byWorkspace[workspaceID])
}

// Events is the stream to read. It is never closed: a reader selects on it
// together with Done, so a publish racing with Close cannot panic on a closed
// channel.
func (s *Subscription) Events() <-chan Event { return s.events }

// Done is closed when the subscription ends, by Close, by a revoke or because
// the reader fell behind.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Dropped reports whether the subscription ended because it fell behind. The
// gateway tells the client, which reconnects and catches up by cursor.
func (s *Subscription) Dropped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Close ends the subscription. Calling it twice is fine.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.hub.remove(s)
		close(s.done)
	})
}

func (s *Subscription) closeSlow() {
	s.mu.Lock()
	s.dropped = true
	s.mu.Unlock()
	s.Close()
}

func (h *Hub) remove(sub *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	subs := h.byWorkspace[sub.workspaceID]
	delete(subs, sub)
	if len(subs) == 0 {
		delete(h.byWorkspace, sub.workspaceID)
	}
}
