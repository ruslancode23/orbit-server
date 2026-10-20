// Package friends owns friendships (SCRUM-98): the one row a pair of accounts
// can have, which is either a pending request or an accepted friendship. It
// publishes the personal events around that row and answers presence and
// privacy through the narrow interfaces they declare, so the pair rule lives in
// one place.
package friends

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/events"
)

var (
	// ErrNotFound covers a request that is not there, one the caller may not
	// act on, and an account that does not exist. One answer for all of them,
	// so the endpoint does not tell which one it was.
	ErrNotFound = errors.New("friends: not found")
	// ErrFriendLimit means accepting would take this instance's friend limit
	// over the top. The request stays pending, so nothing is lost.
	ErrFriendLimit = errors.New("friends: the friend list is full")
	// ErrRateLimited means the account sent too many requests recently or has
	// too many pending at once.
	ErrRateLimited = errors.New("friends: too many friendship requests")
)

// RateLimitedError carries how long the caller should wait; the handler puts it
// in Retry-After. It unwraps to ErrRateLimited, so errors.Is stays simple.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }

// Status is where a pair stands: a request waiting for an answer, or a
// friendship that was accepted.
type Status string

const (
	StatusPending  Status = "pending"
	StatusAccepted Status = "accepted"
)

// Direction says which side of a pending request an account is on.
type Direction string

const (
	DirectionIncoming Direction = "incoming"
	DirectionOutgoing Direction = "outgoing"
)

// User is the little a friendship shows about a person.
type User struct {
	ID       uuid.UUID
	Nickname string
	IsGuest  bool
}

// Friendship is the row of a pair.
type Friendship struct {
	Low, High    uuid.UUID
	Status       Status
	RequestedBy  uuid.UUID
	CreatedAt    time.Time
	AcceptedAt   time.Time
	LowNickname  string
	HighNickname string
	LowIsGuest   bool
	HighIsGuest  bool
}

// Other returns the account opposite userID and the row's cached nickname.
func (f Friendship) Other(userID uuid.UUID) (User, bool) {
	switch userID {
	case f.Low:
		return User{ID: f.High, Nickname: f.HighNickname, IsGuest: f.HighIsGuest}, true
	case f.High:
		return User{ID: f.Low, Nickname: f.LowNickname, IsGuest: f.LowIsGuest}, true
	default:
		return User{}, false
	}
}

// Request is one entry of an incoming or outgoing list.
type Request struct {
	User      User
	Direction Direction
	CreatedAt time.Time
}

// Friend is one entry of the friend list.
type Friend struct {
	ID       uuid.UUID
	Nickname string
	Since    time.Time
}

// Store is the persistence this package needs. It owns the friendships table.
type Store interface {
	UserByNickname(ctx context.Context, nickname string) (User, error)
	UserByID(ctx context.Context, id uuid.UUID) (User, error)
	// Pair returns ErrNotFound when the two accounts have no row.
	Pair(ctx context.Context, low, high uuid.UUID) (Friendship, error)
	// CreateRequest returns when the row was created and whether it was; a
	// concurrent request for the same pair leaves the flag false instead of
	// failing.
	CreateRequest(ctx context.Context, low, high, requestedBy uuid.UUID) (time.Time, bool, error)
	// Accept moves a pending row to accepted and returns the moment it did.
	Accept(ctx context.Context, low, high uuid.UUID) (time.Time, error)
	// DeletePending and DeleteAccepted report whether a row was removed.
	DeletePending(ctx context.Context, low, high uuid.UUID) (bool, error)
	DeleteAccepted(ctx context.Context, low, high uuid.UUID) (bool, error)
	// Delete removes the pair whatever its status; a block uses it.
	Delete(ctx context.Context, low, high uuid.UUID) (bool, error)
	Incoming(ctx context.Context, userID uuid.UUID) ([]Request, error)
	Outgoing(ctx context.Context, userID uuid.UUID) ([]Request, error)
	Friends(ctx context.Context, userID uuid.UUID) ([]Friend, error)
	CountFriends(ctx context.Context, userID uuid.UUID) (int, error)
	CountPending(ctx context.Context, userID uuid.UUID) (int, error)
	FriendsOf(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
	AreFriends(ctx context.Context, a, b uuid.UUID) (bool, error)
}

// Privacy is the rules the friends module asks about accounts and blocks. The
// internal/privacy module implements it; friends keeps no copy of the answers.
type Privacy interface {
	IsGuest(ctx context.Context, userID uuid.UUID) (bool, error)
	CanRequestFriendship(ctx context.Context, fromID, toID uuid.UUID) (bool, error)
	BlockedEitherWay(ctx context.Context, a, b uuid.UUID) (bool, error)
}

// Presence is what the friend list shows next to a name. Optional: with nil,
// everyone is offline, and only the events lose that detail.
type Presence interface {
	Statuses(userIDs []uuid.UUID) map[uuid.UUID]api.PresenceStatus
}

// Publisher is the personal stream the events go to. Optional: with nil the
// rules work the same and nobody is told live.
type Publisher interface {
	PublishToUser(userID uuid.UUID, event events.Event)
}

// Limits are the instance's friend limits. A zero MaxFriends or MaxPending
// turns that particular guard off; the daily request bucket is wired in with
// SetLimiter.
type Limits struct {
	MaxFriends int
	MaxPending int
}

// pendingRetryAfter is what Retry-After says when the pending limit, not the
// daily bucket, is in the way. Pending requests leave only when either side
// answers, so there is no moment that is more correct than a hint.
const pendingRetryAfter = time.Hour

// Service holds the rules. It knows nothing about HTTP.
type Service struct {
	store     Store
	privacy   Privacy
	limits    Limits
	limiter   Limiter
	publisher Publisher
	presence  Presence
}

// Limiter is the per-account bucket of internal/ratelimit.
type Limiter interface {
	Allow(userID uuid.UUID) (bool, time.Duration)
}

// NewService builds the service. Privacy is required: without it every rule
// would have to guess whether an account is a guest or blocked.
func NewService(store Store, privacy Privacy, limits Limits) *Service {
	return &Service{store: store, privacy: privacy, limits: limits}
}

// SetLimiter wires in the daily per-account bucket. Optional; nil means the
// daily guard is off.
func (s *Service) SetLimiter(limiter Limiter) {
	s.limiter = limiter
}

// SetPublisher wires in the live stream after construction. Optional.
func (s *Service) SetPublisher(publisher Publisher) { s.publisher = publisher }

// SetPresence wires in the online status the friend list carries. Optional.
func (s *Service) SetPresence(presence Presence) { s.presence = presence }

// CanRequest reports whether fromID may send toID a friendship request. The
// answer is the privacy module's: a guest cannot send or receive, a block
// outranks everything, and the recipient's friend request policy decides.
func (s *Service) CanRequest(ctx context.Context, fromID, toID uuid.UUID) (bool, error) {
	if s.privacy == nil {
		return false, nil
	}
	return s.privacy.CanRequestFriendship(ctx, fromID, toID)
}

// Request sends a friendship request by exact nickname, compared without case.
// It returns nil for every outcome except a rate limit and a storage failure:
// a missing account, a refusal and a blocked pair all look like success, which
// is the point. When the other side already asked, the pair becomes friends at
// once.
func (s *Service) Request(ctx context.Context, fromID uuid.UUID, nickname string) error {
	if s.privacy == nil || fromID == uuid.Nil {
		return nil
	}
	guest, err := s.privacy.IsGuest(ctx, fromID)
	if err != nil {
		return err
	}
	// A guest has no standing to send a request, and must not learn that from a
	// different answer: the caller still gets its neutral 202.
	if guest {
		return nil
	}

	if s.limiter != nil {
		allowed, retryAfter := s.limiter.Allow(fromID)
		if !allowed {
			return &RateLimitedError{RetryAfter: retryAfter}
		}
	}
	if s.limits.MaxPending > 0 {
		pending, err := s.store.CountPending(ctx, fromID)
		if err != nil {
			return err
		}
		if pending >= s.limits.MaxPending {
			return &RateLimitedError{RetryAfter: pendingRetryAfter}
		}
	}

	target, err := s.store.UserByNickname(ctx, nickname)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if target.ID == fromID {
		return nil
	}
	allowed, err := s.CanRequest(ctx, fromID, target.ID)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}

	sender, err := s.store.UserByID(ctx, fromID)
	if err != nil {
		return err
	}

	low, high := pair(fromID, target.ID)
	existing, err := s.store.Pair(ctx, low, high)
	switch {
	case err == nil:
		return s.requestAgainstExisting(ctx, existing, fromID)
	case errors.Is(err, ErrNotFound):
		createdAt, created, err := s.store.CreateRequest(ctx, low, high, fromID)
		if err != nil {
			return err
		}
		if !created {
			// A concurrent request for the same pair won. Its own event was
			// published by that call; this one keeps the neutral answer.
			return nil
		}
		s.publishRequest(events.KindFriendRequestCreated, sender, target, DirectionOutgoing, createdAt)
		s.publishRequest(events.KindFriendRequestCreated, target, sender, DirectionIncoming, createdAt)
		return nil
	default:
		return err
	}
}

// requestAgainstExisting handles the row a pair already has: a friendship, the
// same outgoing request, or an incoming one that completes the pair.
func (s *Service) requestAgainstExisting(ctx context.Context, existing Friendship, fromID uuid.UUID) error {
	switch existing.Status {
	case StatusAccepted:
		return nil
	case StatusPending:
		if existing.RequestedBy == fromID {
			// Sending the same request twice changes nothing and tells nothing.
			return nil
		}
		// The other side asked first: this is the mutual request that becomes a
		// friendship at once. It is still a neutral success to the sender.
		_, err := s.accept(ctx, existing, fromID)
		if errors.Is(err, ErrFriendLimit) {
			// A full list is the recipient's problem to solve; the request stays
			// pending and the sender must not learn the difference.
			return nil
		}
		return err
	default:
		return nil
	}
}

// Accept turns an incoming request into a friendship. Only its addressee may
// accept, and a full list refuses rather than silently dropping a friend.
func (s *Service) Accept(ctx context.Context, callerID, targetID uuid.UUID) (Friend, error) {
	if s.privacy == nil || callerID == uuid.Nil || targetID == uuid.Nil {
		return Friend{}, ErrNotFound
	}
	guest, err := s.privacy.IsGuest(ctx, callerID)
	if err != nil {
		return Friend{}, err
	}
	if guest {
		return Friend{}, ErrNotFound
	}

	low, high := pair(callerID, targetID)
	existing, err := s.store.Pair(ctx, low, high)
	if err != nil {
		return Friend{}, err
	}
	if existing.Status != StatusPending || existing.RequestedBy != targetID {
		// Not a request addressed to the caller, or not a request at all.
		return Friend{}, ErrNotFound
	}

	return s.accept(ctx, existing, callerID)
}

// accept is the shared accept path. Its caller decides how a full friend list
// is reported: the explicit endpoint says so, the mutual request stays neutral.
func (s *Service) accept(ctx context.Context, existing Friendship, callerID uuid.UUID) (Friend, error) {
	other, ok := existing.Other(callerID)
	if !ok {
		return Friend{}, ErrNotFound
	}
	blocked, err := s.privacy.BlockedEitherWay(ctx, callerID, other.ID)
	if err != nil {
		return Friend{}, err
	}
	if blocked {
		return Friend{}, ErrNotFound
	}
	if err := s.checkFriendLimit(ctx, callerID, other.ID); err != nil {
		return Friend{}, err
	}

	acceptedAt, err := s.store.Accept(ctx, existing.Low, existing.High)
	if err != nil {
		return Friend{}, err
	}
	existing.Status = StatusAccepted
	existing.AcceptedAt = acceptedAt

	caller := User{ID: callerID, Nickname: nicknameOf(existing, callerID)}
	// The addressee saw an incoming request; the author saw an outgoing one.
	s.publishRequest(events.KindFriendRequestRemoved, caller, other, DirectionIncoming, existing.CreatedAt)
	s.publishRequest(events.KindFriendRequestRemoved, other, caller, DirectionOutgoing, existing.CreatedAt)
	s.publishFriend(events.KindFriendAdded, callerID, other, acceptedAt)
	s.publishFriend(events.KindFriendAdded, other.ID, caller, acceptedAt)

	return Friend{ID: other.ID, Nickname: other.Nickname, Since: acceptedAt}, nil
}

// checkFriendLimit refuses an accept that would take either side over the
// instance's limit.
func (s *Service) checkFriendLimit(ctx context.Context, userIDs ...uuid.UUID) error {
	if s.limits.MaxFriends <= 0 {
		return nil
	}
	for _, id := range userIDs {
		count, err := s.store.CountFriends(ctx, id)
		if err != nil {
			return err
		}
		if count >= s.limits.MaxFriends {
			return ErrFriendLimit
		}
	}
	return nil
}

// Delete is the answer to a pending request: the addressee declines it, the
// author cancels it. Either way the row goes and both sides hear about it.
func (s *Service) Delete(ctx context.Context, callerID, targetID uuid.UUID) error {
	if s.privacy == nil || callerID == uuid.Nil || targetID == uuid.Nil {
		return ErrNotFound
	}
	guest, err := s.privacy.IsGuest(ctx, callerID)
	if err != nil {
		return err
	}
	if guest {
		return ErrNotFound
	}

	low, high := pair(callerID, targetID)
	existing, err := s.store.Pair(ctx, low, high)
	if err != nil {
		return err
	}
	if existing.Status != StatusPending {
		return ErrNotFound
	}
	deleted, err := s.store.DeletePending(ctx, low, high)
	if err != nil {
		return err
	}
	if !deleted {
		return ErrNotFound
	}

	caller := User{ID: callerID, Nickname: nicknameOf(existing, callerID)}
	other, _ := existing.Other(callerID)
	callerDirection, otherDirection := DirectionIncoming, DirectionOutgoing
	if existing.RequestedBy == callerID {
		callerDirection, otherDirection = DirectionOutgoing, DirectionIncoming
	}
	s.publishRequest(events.KindFriendRequestRemoved, caller, other, callerDirection, existing.CreatedAt)
	s.publishRequest(events.KindFriendRequestRemoved, other, caller, otherDirection, existing.CreatedAt)
	return nil
}

// Remove ends an accepted friendship. Removing something that is not there is
// fine: the endpoint is idempotent, and the answer says nothing either way.
func (s *Service) Remove(ctx context.Context, callerID, targetID uuid.UUID) error {
	if s.privacy == nil || callerID == uuid.Nil || targetID == uuid.Nil {
		return nil
	}
	guest, err := s.privacy.IsGuest(ctx, callerID)
	if err != nil {
		return err
	}
	if guest {
		return nil
	}

	low, high := pair(callerID, targetID)
	existing, err := s.store.Pair(ctx, low, high)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.Status != StatusAccepted {
		return nil
	}
	deleted, err := s.store.DeleteAccepted(ctx, low, high)
	if err != nil {
		return err
	}
	if !deleted {
		return nil
	}

	other, _ := existing.Other(callerID)
	s.publishFriend(events.KindFriendRemoved, callerID, other, existing.AcceptedAt)
	s.publishFriend(events.KindFriendRemoved, other.ID, User{ID: callerID, Nickname: nicknameOf(existing, callerID)},
		existing.AcceptedAt)
	return nil
}

// CutOnBlock is the block hook privacy calls: whatever the pair had goes, and
// both sides learn about a request or a friendship that disappeared. The block
// itself is not announced by this module, so neither side learns why.
func (s *Service) CutOnBlock(ctx context.Context, a, b uuid.UUID) error {
	if a == uuid.Nil || b == uuid.Nil || a == b {
		return nil
	}
	low, high := pair(a, b)
	existing, err := s.store.Pair(ctx, low, high)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	deleted, err := s.store.Delete(ctx, low, high)
	if err != nil {
		return err
	}
	if !deleted {
		return nil
	}

	// Tell each side about the row that disappeared, in the direction it saw.
	for _, side := range []uuid.UUID{a, b} {
		other, ok := existing.Other(side)
		if !ok {
			continue
		}
		if existing.Status == StatusPending {
			direction := DirectionOutgoing
			if existing.RequestedBy != side {
				direction = DirectionIncoming
			}
			s.publishRequest(events.KindFriendRequestRemoved, User{ID: side}, other,
				direction, existing.CreatedAt)
			continue
		}
		s.publishFriend(events.KindFriendRemoved, side, other, existing.AcceptedAt)
	}
	return nil
}

// Incoming lists requests addressed to the caller, newest first. A guest has
// none, and gets the same empty list as an account nobody asked.
func (s *Service) Incoming(ctx context.Context, userID uuid.UUID) ([]Request, error) {
	if s.privacy == nil {
		return []Request{}, nil
	}
	guest, err := s.privacy.IsGuest(ctx, userID)
	if err != nil {
		return nil, err
	}
	if guest {
		return []Request{}, nil
	}
	return s.store.Incoming(ctx, userID)
}

// Outgoing lists requests the caller sent, newest first.
func (s *Service) Outgoing(ctx context.Context, userID uuid.UUID) ([]Request, error) {
	if s.privacy == nil {
		return []Request{}, nil
	}
	guest, err := s.privacy.IsGuest(ctx, userID)
	if err != nil {
		return nil, err
	}
	if guest {
		return []Request{}, nil
	}
	return s.store.Outgoing(ctx, userID)
}

// Friends lists the caller's accepted friendships, sorted by nickname.
func (s *Service) Friends(ctx context.Context, userID uuid.UUID) ([]Friend, error) {
	if s.privacy == nil {
		return []Friend{}, nil
	}
	guest, err := s.privacy.IsGuest(ctx, userID)
	if err != nil {
		return nil, err
	}
	if guest {
		return []Friend{}, nil
	}
	return s.store.Friends(ctx, userID)
}

// FriendsOf is who may see an account's online status. Presence asks it.
func (s *Service) FriendsOf(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	if userID == uuid.Nil {
		return nil, nil
	}
	return s.store.FriendsOf(ctx, userID)
}

// AreFriends reports an accepted friendship between the two. Privacy asks it
// before a dialogue under PolicyFriends opens.
func (s *Service) AreFriends(ctx context.Context, a, b uuid.UUID) (bool, error) {
	if a == b || a == uuid.Nil || b == uuid.Nil {
		return false, nil
	}
	return s.store.AreFriends(ctx, a, b)
}

// Statuses is the online status of the named accounts, as the friend list
// shows it. With no presence module everyone is offline.
func (s *Service) Statuses(userIDs []uuid.UUID) map[uuid.UUID]api.PresenceStatus {
	out := make(map[uuid.UUID]api.PresenceStatus, len(userIDs))
	for _, id := range userIDs {
		out[id] = api.Offline
	}
	if s.presence == nil {
		return out
	}
	for id, status := range s.presence.Statuses(userIDs) {
		out[id] = status
	}
	return out
}

func (s *Service) publishRequest(kind events.Kind, recipient User, other User, direction Direction, createdAt time.Time) {
	if s.publisher == nil {
		return
	}
	s.publisher.PublishToUser(recipient.ID, events.Event{
		Kind:    kind,
		Payload: api.FriendRequest{UserId: other.ID, Nickname: other.Nickname, Direction: api.FriendRequestDirection(direction), CreatedAt: createdAt},
	})
}

func (s *Service) publishFriend(kind events.Kind, recipient uuid.UUID, other User, since time.Time) {
	if s.publisher == nil {
		return
	}
	presence := api.Offline
	if kind == events.KindFriendAdded {
		presence = s.Statuses([]uuid.UUID{other.ID})[other.ID]
	}
	s.publisher.PublishToUser(recipient, events.Event{
		Kind:    kind,
		Payload: api.Friend{Id: other.ID, Nickname: other.Nickname, Presence: presence, Since: since},
	})
}

func pair(a, b uuid.UUID) (uuid.UUID, uuid.UUID) {
	if a.String() < b.String() {
		return a, b
	}
	return b, a
}

func nicknameOf(f Friendship, id uuid.UUID) string {
	other, ok := f.Other(id)
	if !ok {
		return ""
	}
	return other.Nickname
}
