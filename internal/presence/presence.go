// Package presence keeps who is online in text: the live WebSocket connections
// of an account, the status its owner chose to show, and what other people are
// allowed to see. It is the text counterpart of voice presence, which LiveKit
// owns; nothing here touches the database, so losing the state on restart costs
// one reconnect.
package presence

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Status is what other people see next to a name.
type Status string

const (
	// StatusOnline is a connection with recent activity behind it.
	StatusOnline Status = "online"
	// StatusIdle is a connection whose tab is hidden or that has been silent for
	// idleAfter.
	StatusIdle Status = "idle"
	// StatusDnd is the status the owner picked: "do not disturb".
	StatusDnd Status = "dnd"
	// StatusOffline is no connection at all, or an account that chose to be
	// invisible. The two are deliberately indistinguishable from outside.
	StatusOffline Status = "offline"
)

// Availability is the status the account picked for itself.
type Availability string

const (
	// AvailabilityAuto follows the connections: online while a tab is in front,
	// idle when none is, offline when there are none.
	AvailabilityAuto Availability = "auto"
	// AvailabilityOnline keeps the account online whatever its tabs say.
	AvailabilityOnline Availability = "online"
	// AvailabilityIdle keeps the account idle.
	AvailabilityIdle Availability = "idle"
	// AvailabilityDnd tells the others not to disturb; messages still arrive.
	AvailabilityDnd Availability = "dnd"
	// AvailabilityInvisible is online for the account itself and offline for
	// everyone else.
	AvailabilityInvisible Availability = "invisible"
)

// Activity is what one connection says about its tab. The client sends it when
// the tab is hidden or shown again; silence for idleAfter counts as idle too.
type Activity string

const (
	ActivityActive Activity = "active"
	ActivityIdle   Activity = "idle"
)

var (
	// ErrInvalidAvailability is a chosen status this package does not know.
	ErrInvalidAvailability = errors.New("presence: unknown availability")
	// ErrInvalidActivity is a tab state this package does not know.
	ErrInvalidActivity = errors.New("presence: unknown activity")
)

// idleAfter is how long a connection may stay silent before it counts as idle.
// The client normally sends an activity frame when the tab is hidden, so this
// only covers a tab left open and untouched.
const idleAfter = 5 * time.Minute

// State is the payload of the presence.updated event: where one account is,
// and when that became true. The account itself receives its own updates, since
// it is a member of its own servers; a client ignores the ones whose id is its
// own.
type State struct {
	UserID    uuid.UUID `json:"userId"`
	Status    Status    `json:"status"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// PresenceStore is where connections and the chosen status live. The shipped
// implementation keeps them in the memory of this process, like voice
// presence; a store shared by several instances can replace it without
// touching the rules in Service.
type PresenceStore interface {
	// Connect registers one live connection of an account.
	Connect(userID, connectionID uuid.UUID)
	// Disconnect forgets a connection. The account is offline once the last one
	// is gone; the moment it went is what the offline grace counts from.
	Disconnect(userID, connectionID uuid.UUID)
	// SetActivity records whether the tab of one connection is in front. A
	// connection the store does not know is ignored: a frame can arrive after a
	// reconnect.
	SetActivity(userID, connectionID uuid.UUID, activity Activity) error
	// SetAvailability records the status the account picked.
	SetAvailability(userID uuid.UUID, availability Availability) error
	// Availability is the status the account picked; auto by default.
	Availability(userID uuid.UUID) Availability
	// Status is what other people see, the chosen status already applied.
	Status(userID uuid.UUID) Status
	// Connected reports whether the account has any live connection.
	Connected(userID uuid.UUID) bool
	// DisconnectedAt is when the last connection went. It is zero while at
	// least one is open, and for an account that never connected.
	DisconnectedAt(userID uuid.UUID) time.Time
}

// MemoryStore keeps presence in the maps of this process. It is safe for
// concurrent use and one instance is shared by the whole server.
//
// An entry is a few dozen bytes per account that ever connected and lives until
// the process stops; a restart is also what clears it.
type MemoryStore struct {
	mu      sync.Mutex
	now     func() time.Time
	idleFor time.Duration
	users   map[uuid.UUID]*account
}

type account struct {
	connections  map[uuid.UUID]*connection
	availability Availability
	// closedAt is when the last connection went; zero while one is open.
	closedAt time.Time
}

type connection struct {
	activity Activity
	// seenAt is when this connection last said it was active.
	seenAt time.Time
}

// NewMemoryStore starts an empty store that reads the clock and treats a
// connection silent for idleAfter as idle.
func NewMemoryStore() *MemoryStore {
	return newMemoryStore(time.Now, idleAfter)
}

func newMemoryStore(now func() time.Time, idleFor time.Duration) *MemoryStore {
	return &MemoryStore{
		now:     now,
		idleFor: idleFor,
		users:   map[uuid.UUID]*account{},
	}
}

func (m *MemoryStore) Connect(userID, connectionID uuid.UUID) {
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()
	acc := m.accountLocked(userID)
	acc.connections[connectionID] = &connection{activity: ActivityActive, seenAt: now}
	acc.closedAt = time.Time{}
}

func (m *MemoryStore) Disconnect(userID, connectionID uuid.UUID) {
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()
	acc := m.users[userID]
	if acc == nil {
		return
	}
	delete(acc.connections, connectionID)
	if len(acc.connections) == 0 {
		acc.closedAt = now
	}
}

func (m *MemoryStore) SetActivity(userID, connectionID uuid.UUID, activity Activity) error {
	if !activity.valid() {
		return fmt.Errorf("%w: %q", ErrInvalidActivity, activity)
	}

	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	acc := m.users[userID]
	if acc == nil {
		return nil
	}
	conn := acc.connections[connectionID]
	if conn == nil {
		return nil
	}
	conn.activity = activity
	conn.seenAt = now
	return nil
}

func (m *MemoryStore) SetAvailability(userID uuid.UUID, availability Availability) error {
	if !availability.valid() {
		return fmt.Errorf("%w: %q", ErrInvalidAvailability, availability)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.accountLocked(userID).availability = availability
	return nil
}

func (m *MemoryStore) Availability(userID uuid.UUID) Availability {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc := m.users[userID]
	if acc == nil {
		return AvailabilityAuto
	}
	return acc.availability
}

func (m *MemoryStore) Status(userID uuid.UUID) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked(m.users[userID], m.now())
}

func (m *MemoryStore) Connected(userID uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc := m.users[userID]
	return acc != nil && len(acc.connections) > 0
}

func (m *MemoryStore) DisconnectedAt(userID uuid.UUID) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc := m.users[userID]
	if acc == nil {
		return time.Time{}
	}
	return acc.closedAt
}

func (m *MemoryStore) accountLocked(userID uuid.UUID) *account {
	acc := m.users[userID]
	if acc == nil {
		acc = &account{connections: map[uuid.UUID]*connection{}, availability: AvailabilityAuto}
		m.users[userID] = acc
	}
	return acc
}

func (m *MemoryStore) statusLocked(acc *account, now time.Time) Status {
	// Invisible comes first: an account that hid itself is offline to everyone,
	// whether or not it has a tab open.
	if acc != nil && acc.availability == AvailabilityInvisible {
		return StatusOffline
	}
	if acc == nil || len(acc.connections) == 0 {
		return StatusOffline
	}

	switch acc.availability {
	case AvailabilityDnd:
		return StatusDnd
	case AvailabilityOnline:
		return StatusOnline
	case AvailabilityIdle:
		return StatusIdle
	case AvailabilityAuto, AvailabilityInvisible:
	}

	for _, conn := range acc.connections {
		if conn.active(now, m.idleFor) {
			return StatusOnline
		}
	}
	return StatusIdle
}

func (c *connection) active(now time.Time, idleFor time.Duration) bool {
	return c.activity == ActivityActive && now.Sub(c.seenAt) < idleFor
}

func (a Availability) valid() bool {
	switch a {
	case AvailabilityAuto, AvailabilityOnline, AvailabilityIdle, AvailabilityDnd, AvailabilityInvisible:
		return true
	}
	return false
}

func (a Activity) valid() bool {
	return a == ActivityActive || a == ActivityIdle
}
