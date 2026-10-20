package friends

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/events"
)

// fakeStore keeps friendships in memory with the same rules as the table: one
// row per pair, stored low-high, and a pending row becomes accepted in place.
type fakeStore struct {
	users map[uuid.UUID]User
	pairs map[string]Friendship
	now   func() time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users: map[uuid.UUID]User{},
		pairs: map[string]Friendship{},
		now:   time.Now,
	}
}

func (s *fakeStore) add(user User) { s.users[user.ID] = user }

func (s *fakeStore) UserByNickname(_ context.Context, nickname string) (User, error) {
	for _, user := range s.users {
		if equalFold(user.Nickname, nickname) {
			return user, nil
		}
	}
	return User{}, ErrNotFound
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func (s *fakeStore) UserByID(_ context.Context, id uuid.UUID) (User, error) {
	user, ok := s.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return user, nil
}

func (s *fakeStore) Pair(_ context.Context, low, high uuid.UUID) (Friendship, error) {
	row, ok := s.pairs[low.String()+":"+high.String()]
	if !ok {
		return Friendship{}, ErrNotFound
	}
	return row, nil
}

func (s *fakeStore) CreateRequest(_ context.Context, low, high, requestedBy uuid.UUID) (time.Time, bool, error) {
	key := low.String() + ":" + high.String()
	if _, exists := s.pairs[key]; exists {
		return time.Time{}, false, nil
	}
	createdAt := s.now()
	s.pairs[key] = Friendship{
		Low: low, High: high, Status: StatusPending, RequestedBy: requestedBy,
		CreatedAt: createdAt, LowNickname: s.users[low].Nickname, HighNickname: s.users[high].Nickname,
	}
	return createdAt, true, nil
}

func (s *fakeStore) Accept(_ context.Context, low, high uuid.UUID) (time.Time, error) {
	key := low.String() + ":" + high.String()
	row, ok := s.pairs[key]
	if !ok || row.Status != StatusPending {
		return time.Time{}, ErrNotFound
	}
	row.Status = StatusAccepted
	row.AcceptedAt = s.now()
	s.pairs[key] = row
	return row.AcceptedAt, nil
}

func (s *fakeStore) DeletePending(_ context.Context, low, high uuid.UUID) (bool, error) {
	key := low.String() + ":" + high.String()
	row, ok := s.pairs[key]
	if !ok || row.Status != StatusPending {
		return false, nil
	}
	delete(s.pairs, key)
	return true, nil
}

func (s *fakeStore) DeleteAccepted(_ context.Context, low, high uuid.UUID) (bool, error) {
	key := low.String() + ":" + high.String()
	row, ok := s.pairs[key]
	if !ok || row.Status != StatusAccepted {
		return false, nil
	}
	delete(s.pairs, key)
	return true, nil
}

func (s *fakeStore) Delete(_ context.Context, low, high uuid.UUID) (bool, error) {
	key := low.String() + ":" + high.String()
	if _, ok := s.pairs[key]; !ok {
		return false, nil
	}
	delete(s.pairs, key)
	return true, nil
}

func (s *fakeStore) Incoming(_ context.Context, userID uuid.UUID) ([]Request, error) {
	var out []Request
	for _, row := range s.pairs {
		if row.Status != StatusPending || row.RequestedBy == userID {
			continue
		}
		other, ok := row.Other(userID)
		if !ok {
			continue
		}
		out = append(out, Request{User: other, Direction: DirectionIncoming, CreatedAt: row.CreatedAt})
	}
	sortRequests(out)
	return out, nil
}

func (s *fakeStore) Outgoing(_ context.Context, userID uuid.UUID) ([]Request, error) {
	var out []Request
	for _, row := range s.pairs {
		if row.Status != StatusPending || row.RequestedBy != userID {
			continue
		}
		other, ok := row.Other(userID)
		if !ok {
			continue
		}
		out = append(out, Request{User: other, Direction: DirectionOutgoing, CreatedAt: row.CreatedAt})
	}
	sortRequests(out)
	return out, nil
}

func sortRequests(list []Request) {
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
}

func (s *fakeStore) Friends(_ context.Context, userID uuid.UUID) ([]Friend, error) {
	var out []Friend
	for _, row := range s.pairs {
		if row.Status != StatusAccepted {
			continue
		}
		other, ok := row.Other(userID)
		if !ok {
			continue
		}
		out = append(out, Friend{ID: other.ID, Nickname: other.Nickname, Since: row.AcceptedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Nickname < out[j].Nickname })
	return out, nil
}

func (s *fakeStore) CountFriends(ctx context.Context, userID uuid.UUID) (int, error) {
	list, _ := s.Friends(ctx, userID)
	return len(list), nil
}

func (s *fakeStore) CountPending(ctx context.Context, userID uuid.UUID) (int, error) {
	list, _ := s.Outgoing(ctx, userID)
	return len(list), nil
}

func (s *fakeStore) FriendsOf(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	list, _ := s.Friends(ctx, userID)
	out := make([]uuid.UUID, 0, len(list))
	for _, friend := range list {
		out = append(out, friend.ID)
	}
	return out, nil
}

func (s *fakeStore) AreFriends(ctx context.Context, a, b uuid.UUID) (bool, error) {
	low, high := pair(a, b)
	row, err := s.Pair(ctx, low, high)
	if err != nil {
		return false, nil
	}
	return row.Status == StatusAccepted, nil
}

// fakePrivacy answers the three questions the service asks; the policy rules
// themselves are tested in internal/privacy.
type fakePrivacy struct {
	guests  map[uuid.UUID]bool
	blocked map[string]bool
	allow   map[string]bool
}

func newFakePrivacy() *fakePrivacy {
	return &fakePrivacy{
		guests:  map[uuid.UUID]bool{},
		blocked: map[string]bool{},
		allow:   map[string]bool{},
	}
}

func (p *fakePrivacy) IsGuest(_ context.Context, userID uuid.UUID) (bool, error) {
	return p.guests[userID], nil
}

func (p *fakePrivacy) CanRequestFriendship(_ context.Context, fromID, toID uuid.UUID) (bool, error) {
	if fromID == toID || p.guests[fromID] || p.guests[toID] {
		return false, nil
	}
	if p.blocked[fromID.String()+":"+toID.String()] || p.blocked[toID.String()+":"+fromID.String()] {
		return false, nil
	}
	if allowed, ok := p.allow[fromID.String()+":"+toID.String()]; ok {
		return allowed, nil
	}
	return true, nil
}

func (p *fakePrivacy) BlockedEitherWay(_ context.Context, a, b uuid.UUID) (bool, error) {
	return p.blocked[a.String()+":"+b.String()] || p.blocked[b.String()+":"+a.String()], nil
}

type fakePresence struct {
	statuses map[uuid.UUID]api.PresenceStatus
}

func (p *fakePresence) Statuses(ids []uuid.UUID) map[uuid.UUID]api.PresenceStatus {
	out := make(map[uuid.UUID]api.PresenceStatus, len(ids))
	for _, id := range ids {
		if status, ok := p.statuses[id]; ok {
			out[id] = status
			continue
		}
		out[id] = api.Offline
	}
	return out
}

type personalEvent struct {
	userID uuid.UUID
	event  events.Event
}

type recorder struct {
	personal []personalEvent
}

func (r *recorder) PublishToUser(userID uuid.UUID, event events.Event) {
	r.personal = append(r.personal, personalEvent{userID: userID, event: event})
}

func (r *recorder) taken() {
	r.personal = nil
}

func (r *recorder) forUser(userID uuid.UUID) []events.Event {
	var out []events.Event
	for _, event := range r.personal {
		if event.userID == userID {
			out = append(out, event.event)
		}
	}
	return out
}

type world struct {
	svc      *Service
	store    *fakeStore
	privacy  *fakePrivacy
	presence *fakePresence
	rec      *recorder
	anna     uuid.UUID
	bob      uuid.UUID
	vika     uuid.UUID
	guest    uuid.UUID
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{
		store:    newFakeStore(),
		privacy:  newFakePrivacy(),
		presence: &fakePresence{statuses: map[uuid.UUID]api.PresenceStatus{}},
		rec:      &recorder{},
		anna:     uuid.New(),
		bob:      uuid.New(),
		vika:     uuid.New(),
		guest:    uuid.New(),
	}
	w.store.add(User{ID: w.anna, Nickname: "Anna"})
	w.store.add(User{ID: w.bob, Nickname: "Bob"})
	w.store.add(User{ID: w.vika, Nickname: "Vika"})
	w.store.add(User{ID: w.guest, Nickname: "Guest", IsGuest: true})
	w.privacy.guests[w.guest] = true
	w.svc = NewService(w.store, w.privacy, Limits{MaxFriends: 1000, MaxPending: 100})
	w.svc.SetPublisher(w.rec)
	w.svc.SetPresence(w.presence)
	return w
}

func (w *world) friend(a, b uuid.UUID) {
	low, high := pair(a, b)
	w.store.pairs[low.String()+":"+high.String()] = Friendship{
		Low: low, High: high, Status: StatusAccepted, RequestedBy: a,
		CreatedAt: time.Now(), AcceptedAt: time.Now(),
		LowNickname: w.store.users[low].Nickname, HighNickname: w.store.users[high].Nickname,
	}
}

func (w *world) row(a, b uuid.UUID) (Friendship, error) {
	low, high := pair(a, b)
	return w.store.Pair(context.Background(), low, high)
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDirectRequestAndLists(t *testing.T) {
	w := newWorld(t)

	if err := w.svc.Request(t.Context(), w.anna, "bob"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	row, err := w.row(w.anna, w.bob)
	if err != nil || row.Status != StatusPending || row.RequestedBy != w.anna {
		t.Fatalf("pair = %+v, err = %v, want a pending request from Anna", row, err)
	}

	incoming, err := w.svc.Incoming(t.Context(), w.bob)
	if err != nil {
		t.Fatalf("Incoming: %v", err)
	}
	if len(incoming) != 1 || incoming[0].User.ID != w.anna || incoming[0].Direction != DirectionIncoming {
		t.Errorf("Bob's incoming = %+v, want Anna", incoming)
	}
	outgoing, err := w.svc.Outgoing(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("Outgoing: %v", err)
	}
	if len(outgoing) != 1 || outgoing[0].User.ID != w.bob || outgoing[0].Direction != DirectionOutgoing {
		t.Errorf("Anna's outgoing = %+v, want Bob", outgoing)
	}
	if none, _ := w.svc.Incoming(t.Context(), w.anna); len(none) != 0 {
		t.Errorf("Anna has incoming requests = %+v, want none", none)
	}

	// Both sides hear about the request, each with its own direction.
	annaEvents := w.rec.forUser(w.anna)
	bobEvents := w.rec.forUser(w.bob)
	if len(annaEvents) != 1 || len(bobEvents) != 1 {
		t.Fatalf("events = %d/%d, want one each", len(annaEvents), len(bobEvents))
	}
	annaPayload, ok := annaEvents[0].Payload.(api.FriendRequest)
	if !ok || annaPayload.Direction != api.Outgoing || annaPayload.UserId != w.bob {
		t.Errorf("Anna's payload = %+v, want outgoing to Bob", annaEvents[0].Payload)
	}
	bobPayload, ok := bobEvents[0].Payload.(api.FriendRequest)
	if !ok || bobPayload.Direction != api.Incoming || bobPayload.UserId != w.anna {
		t.Errorf("Bob's payload = %+v, want incoming from Anna", bobEvents[0].Payload)
	}
}

// Sending exactly the same request again changes nothing: one row, no second
// event, and still a neutral success.
func TestRepeatedRequestIsIdempotent(t *testing.T) {
	w := newWorld(t)
	if err := w.svc.Request(t.Context(), w.anna, "Anna"); err != nil {
		t.Fatalf("request to self: %v", err)
	}
	if len(w.store.pairs) != 0 {
		t.Fatalf("a request to self created a row")
	}

	for range 2 {
		if err := w.svc.Request(t.Context(), w.anna, "Bob"); err != nil {
			t.Fatalf("Request: %v", err)
		}
	}
	if len(w.store.pairs) != 1 {
		t.Fatalf("pairs = %d, want 1", len(w.store.pairs))
	}
	if got := len(w.rec.forUser(w.bob)); got != 1 {
		t.Errorf("Bob's events = %d, want 1", got)
	}
}

func TestMutualRequestBecomesFriendship(t *testing.T) {
	w := newWorld(t)
	if err := w.svc.Request(t.Context(), w.anna, "Bob"); err != nil {
		t.Fatalf("Anna's request: %v", err)
	}
	w.rec.taken()

	if err := w.svc.Request(t.Context(), w.bob, "anna"); err != nil {
		t.Fatalf("Bob's mutual request: %v", err)
	}
	row, err := w.row(w.anna, w.bob)
	if err != nil || row.Status != StatusAccepted {
		t.Fatalf("pair = %+v, err = %v, want accepted", row, err)
	}

	for _, id := range []uuid.UUID{w.anna, w.bob} {
		friends, err := w.svc.Friends(t.Context(), id)
		if err != nil || len(friends) != 1 {
			t.Errorf("friends of %s = %+v, err = %v, want one", id, friends, err)
		}
	}

	// Both learn that the request is gone and the friendship exists.
	for _, id := range []uuid.UUID{w.anna, w.bob} {
		kinds := map[events.Kind]int{}
		for _, event := range w.rec.forUser(id) {
			kinds[event.Kind]++
		}
		if kinds[events.KindFriendRequestRemoved] != 1 || kinds[events.KindFriendAdded] != 1 {
			t.Errorf("events of %s = %v, want one removed and one added", id, kinds)
		}
	}
}

func TestAccept(t *testing.T) {
	w := newWorld(t)
	w.presence.statuses[w.anna] = api.Online
	if err := w.svc.Request(t.Context(), w.anna, "Bob"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	w.rec.taken()

	friend, err := w.svc.Accept(t.Context(), w.bob, w.anna)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if friend.ID != w.anna || friend.Nickname != "Anna" {
		t.Errorf("friend = %+v, want Anna", friend)
	}

	// Only the addressee may accept: the author gets the same 404 as a stranger.
	if _, err := w.svc.Accept(t.Context(), w.anna, w.bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("author accepting their own request: err = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.Accept(t.Context(), w.vika, w.anna); !errors.Is(err, ErrNotFound) {
		t.Errorf("stranger accepting: err = %v, want ErrNotFound", err)
	}

	// The added friend carries the online status at the moment of the event.
	for _, event := range w.rec.forUser(w.bob) {
		if event.Kind != events.KindFriendAdded {
			continue
		}
		payload, ok := event.Payload.(api.Friend)
		if !ok || payload.Presence != api.Online || payload.Id != w.anna {
			t.Errorf("friend.added payload = %+v, want Anna online", event.Payload)
		}
	}
}

func TestDeclineAndCancel(t *testing.T) {
	t.Run("the addressee declines", func(t *testing.T) {
		w := newWorld(t)
		_ = w.svc.Request(t.Context(), w.anna, "Bob")
		w.rec.taken()

		if err := w.svc.Delete(t.Context(), w.bob, w.anna); err != nil {
			t.Fatalf("decline: %v", err)
		}
		if _, err := w.row(w.anna, w.bob); !errors.Is(err, ErrNotFound) {
			t.Errorf("pair survived the decline")
		}
		for _, id := range []uuid.UUID{w.anna, w.bob} {
			got := w.rec.forUser(id)
			if len(got) != 1 || got[0].Kind != events.KindFriendRequestRemoved {
				t.Errorf("events of %s = %+v, want one request.removed", id, got)
				continue
			}
			payload, ok := got[0].Payload.(api.FriendRequest)
			if !ok || payload.Nickname == "" {
				t.Errorf("payload of %s = %+v, want the other nickname", id, got[0].Payload)
			}
		}
	})

	t.Run("the author cancels", func(t *testing.T) {
		w := newWorld(t)
		_ = w.svc.Request(t.Context(), w.anna, "Bob")
		w.rec.taken()

		if err := w.svc.Delete(t.Context(), w.anna, w.bob); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := w.row(w.anna, w.bob); !errors.Is(err, ErrNotFound) {
			t.Errorf("pair survived the cancellation")
		}
	})

	t.Run("a stranger cannot touch the request", func(t *testing.T) {
		w := newWorld(t)
		_ = w.svc.Request(t.Context(), w.anna, "Bob")
		if err := w.svc.Delete(t.Context(), w.vika, w.anna); !errors.Is(err, ErrNotFound) {
			t.Errorf("stranger deleting: err = %v, want ErrNotFound", err)
		}
	})
}

func TestRemoveFriend(t *testing.T) {
	w := newWorld(t)
	w.friend(w.anna, w.bob)
	w.rec.taken()

	if err := w.svc.Remove(t.Context(), w.anna, w.bob); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := w.row(w.anna, w.bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("friendship survived the removal")
	}
	for _, id := range []uuid.UUID{w.anna, w.bob} {
		got := w.rec.forUser(id)
		if len(got) != 1 || got[0].Kind != events.KindFriendRemoved {
			t.Errorf("events of %s = %+v, want one friend.removed", id, got)
		}
	}

	// Removing something that is not there is fine and silent.
	if err := w.svc.Remove(t.Context(), w.anna, w.bob); err != nil {
		t.Errorf("removing twice: %v", err)
	}
	if err := w.svc.Remove(t.Context(), w.anna, w.vika); err != nil {
		t.Errorf("removing a stranger: %v", err)
	}
}

// A block in either direction withdraws a pending request and ends a
// friendship; the hook is what privacy calls after it stored the block.
func TestBlockWithdrawsThePair(t *testing.T) {
	t.Run("pending in either direction", func(t *testing.T) {
		w := newWorld(t)
		_ = w.svc.Request(t.Context(), w.anna, "Bob")
		if err := w.svc.CutOnBlock(t.Context(), w.bob, w.anna); err != nil {
			t.Fatalf("CutOnBlock: %v", err)
		}
		if _, err := w.row(w.anna, w.bob); !errors.Is(err, ErrNotFound) {
			t.Errorf("request survived the block")
		}

		_ = w.svc.Request(t.Context(), w.bob, "Anna")
		if err := w.svc.CutOnBlock(t.Context(), w.anna, w.bob); err != nil {
			t.Fatalf("CutOnBlock: %v", err)
		}
		if _, err := w.row(w.anna, w.bob); !errors.Is(err, ErrNotFound) {
			t.Errorf("request survived the block in the other direction")
		}
	})

	t.Run("an accepted friendship", func(t *testing.T) {
		w := newWorld(t)
		w.friend(w.anna, w.bob)
		w.rec.taken()

		if err := w.svc.CutOnBlock(t.Context(), w.anna, w.bob); err != nil {
			t.Fatalf("CutOnBlock: %v", err)
		}
		for _, id := range []uuid.UUID{w.anna, w.bob} {
			got := w.rec.forUser(id)
			if len(got) != 1 || got[0].Kind != events.KindFriendRemoved {
				t.Errorf("events of %s = %+v, want one friend.removed", id, got)
			}
		}
	})
}

func TestGuestsHaveNoFriendships(t *testing.T) {
	w := newWorld(t)

	// Neither direction stores anything or tells anyone.
	if err := w.svc.Request(t.Context(), w.guest, "Bob"); err != nil {
		t.Fatalf("guest request: %v", err)
	}
	if err := w.svc.Request(t.Context(), w.anna, "Guest"); err != nil {
		t.Fatalf("request to a guest: %v", err)
	}
	if len(w.store.pairs) != 0 {
		t.Fatalf("pairs = %d, want 0", len(w.store.pairs))
	}
	for _, list := range []func(context.Context, uuid.UUID) ([]Request, error){w.svc.Incoming, w.svc.Outgoing} {
		requests, err := list(t.Context(), w.guest)
		if err != nil {
			t.Fatalf("guest list: %v", err)
		}
		if len(requests) != 0 {
			t.Errorf("guest requests = %+v, want empty", requests)
		}
	}
	friends, err := w.svc.Friends(t.Context(), w.guest)
	if err != nil {
		t.Fatalf("guest friends: %v", err)
	}
	if len(friends) != 0 {
		t.Errorf("guest friends = %+v, want empty", friends)
	}
}

func TestBlockedPairIsNeutralAndStoresNothing(t *testing.T) {
	w := newWorld(t)
	w.privacy.blocked[w.bob.String()+":"+w.anna.String()] = true

	if err := w.svc.Request(t.Context(), w.anna, "Bob"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(w.store.pairs) != 0 {
		t.Errorf("a blocked pair got a request row")
	}
	if events := w.rec.forUser(w.bob); len(events) != 0 {
		t.Errorf("blocked recipient got %d events, want 0", len(events))
	}
}

func TestFriendLimit(t *testing.T) {
	w := newWorld(t)
	w.svc.limits.MaxFriends = 1
	// Bob already has one friend, so accepting Anna would be his second.
	w.friend(w.bob, w.vika)

	_ = w.svc.Request(t.Context(), w.anna, "Bob")
	if _, err := w.svc.Accept(t.Context(), w.bob, w.anna); !errors.Is(err, ErrFriendLimit) {
		t.Fatalf("Accept: err = %v, want ErrFriendLimit", err)
	}
	if row, err := w.row(w.anna, w.bob); err != nil || row.Status != StatusPending {
		t.Errorf("pair = %+v, err = %v, want it still pending", row, err)
	}
}

func TestPendingLimit(t *testing.T) {
	w := newWorld(t)
	w.svc.limits.MaxPending = 1
	if err := w.svc.Request(t.Context(), w.anna, "Bob"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	err := w.svc.Request(t.Context(), w.anna, "Vika")
	var limited *RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("second request: err = %v, want RateLimitedError", err)
	}
	if limited.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want positive", limited.RetryAfter)
	}
}

func TestFriendsOfAndAreFriends(t *testing.T) {
	w := newWorld(t)
	w.friend(w.anna, w.bob)

	ids, err := w.svc.FriendsOf(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("FriendsOf: %v", err)
	}
	if len(ids) != 1 || ids[0] != w.bob {
		t.Errorf("FriendsOf = %v, want Bob", ids)
	}
	friends, err := w.svc.AreFriends(t.Context(), w.bob, w.anna)
	if err != nil || !friends {
		t.Errorf("AreFriends = %v, %v, want true", friends, err)
	}
	friends, err = w.svc.AreFriends(t.Context(), w.anna, w.vika)
	if err != nil || friends {
		t.Errorf("AreFriends with a stranger = %v, %v, want false", friends, err)
	}
}
