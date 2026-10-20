package privacy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore keeps the users, their policies and the blocks in memory, with the
// rules the database keeps: a block is one row per direction, and blocking
// twice changes nothing.
type fakeStore struct {
	accounts  map[uuid.UUID]Account
	nicknames map[uuid.UUID]string
	shared    map[string]bool
	blocks    map[uuid.UUID]map[uuid.UUID]time.Time
	blocksAt  map[uuid.UUID]time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		accounts:  map[uuid.UUID]Account{},
		nicknames: map[uuid.UUID]string{},
		shared:    map[string]bool{},
		blocks:    map[uuid.UUID]map[uuid.UUID]time.Time{},
		blocksAt:  map[uuid.UUID]time.Time{},
	}
}

func (s *fakeStore) add(id uuid.UUID, policy Policy, guest bool) {
	// The default friend request policy matches the column default.
	s.accounts[id] = Account{Policy: policy, FriendRequestPolicy: FriendRequestsShared, IsGuest: guest}
	s.nicknames[id] = "user-" + id.String()[:8]
}

func (s *fakeStore) share(a, b uuid.UUID) {
	s.shared[a.String()+":"+b.String()] = true
}

func (s *fakeStore) block(blockerID, blockedID uuid.UUID) {
	if s.blocks[blockerID] == nil {
		s.blocks[blockerID] = map[uuid.UUID]time.Time{}
	}
	s.blocks[blockerID][blockedID] = time.Now()
}

func (s *fakeStore) UserPrivacy(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]Account, error) {
	out := make(map[uuid.UUID]Account, len(ids))
	for _, id := range ids {
		if account, ok := s.accounts[id]; ok {
			out[id] = account
		}
	}
	return out, nil
}

func (s *fakeStore) HasSharedWorkspace(_ context.Context, a, b uuid.UUID) (bool, error) {
	return s.shared[a.String()+":"+b.String()] || s.shared[b.String()+":"+a.String()], nil
}

func (s *fakeStore) BlockedEitherWay(_ context.Context, a, b uuid.UUID) (bool, error) {
	for _, pair := range [][2]uuid.UUID{{a, b}, {b, a}} {
		if _, ok := s.blocks[pair[0]][pair[1]]; ok {
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeStore) BlockedIDs(_ context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(s.blocks[userID]))
	for id := range s.blocks[userID] {
		out = append(out, id)
	}
	return out, nil
}

func (s *fakeStore) ListBlocks(_ context.Context, userID uuid.UUID) ([]BlockedUser, error) {
	blocked := make([]BlockedUser, 0, len(s.blocks[userID]))
	for id, at := range s.blocks[userID] {
		blocked = append(blocked, BlockedUser{ID: id, Nickname: s.nicknames[id], BlockedAt: at})
	}
	return blocked, nil
}

func (s *fakeStore) Block(_ context.Context, blockerID, blockedID uuid.UUID) error {
	s.block(blockerID, blockedID)
	return nil
}

func (s *fakeStore) Unblock(_ context.Context, blockerID, blockedID uuid.UUID) error {
	delete(s.blocks[blockerID], blockedID)
	return nil
}

func (s *fakeStore) UserExists(_ context.Context, id uuid.UUID) (bool, error) {
	_, ok := s.accounts[id]
	return ok, nil
}

func (s *fakeStore) Policy(_ context.Context, userID uuid.UUID) (Policy, error) {
	account, ok := s.accounts[userID]
	if !ok {
		return "", ErrNotFound
	}
	return account.Policy, nil
}

func (s *fakeStore) SetPolicy(_ context.Context, userID uuid.UUID, policy Policy) error {
	account, ok := s.accounts[userID]
	if !ok {
		return ErrNotFound
	}
	account.Policy = policy
	s.accounts[userID] = account
	return nil
}

func (s *fakeStore) SetFriendRequestPolicy(_ context.Context, userID uuid.UUID, policy FriendRequestPolicy) error {
	account, ok := s.accounts[userID]
	if !ok {
		return ErrNotFound
	}
	account.FriendRequestPolicy = policy
	s.accounts[userID] = account
	return nil
}

// fakeFriendship is the narrow interface the friends module implements.
type fakeFriendship struct {
	pairs map[string]bool
}

func newFakeFriendship() *fakeFriendship {
	return &fakeFriendship{pairs: map[string]bool{}}
}

func (f *fakeFriendship) add(a, b uuid.UUID) {
	f.pairs[a.String()+":"+b.String()] = true
}

func (f *fakeFriendship) AreFriends(_ context.Context, a, b uuid.UUID) (bool, error) {
	return f.pairs[a.String()+":"+b.String()] || f.pairs[b.String()+":"+a.String()], nil
}

// fakeBlockCleanup records what privacy asked the friends module to withdraw.
type fakeBlockCleanup struct {
	pairs [][2]uuid.UUID
}

func (f *fakeBlockCleanup) CutOnBlock(_ context.Context, a, b uuid.UUID) error {
	f.pairs = append(f.pairs, [2]uuid.UUID{a, b})
	return nil
}

type world struct {
	svc        *Service
	store      *fakeStore
	friendship *fakeFriendship
	anna       uuid.UUID
	bob        uuid.UUID
	vika       uuid.UUID
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{
		store:      newFakeStore(),
		friendship: newFakeFriendship(),
		anna:       uuid.New(),
		bob:        uuid.New(),
		vika:       uuid.New(),
	}
	for _, id := range []uuid.UUID{w.anna, w.bob, w.vika} {
		w.store.add(id, PolicyShared, false)
	}
	w.svc = NewService(w.store)
	w.svc.SetFriendship(w.friendship)
	return w
}

// Every door into a direct dialogue is closed by the same false answer: no
// common server, a stricter policy, a block, a guest, or no account at all.
func TestCanStartDirect(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*world)
		want  bool
	}{
		{
			name:  "a common server opens the door",
			setup: func(w *world) { w.store.share(w.anna, w.bob) },
			want:  true,
		},
		{
			name:  "no common server",
			setup: func(*world) {},
			want:  false,
		},
		{
			name: "the recipient said nobody",
			setup: func(w *world) {
				w.store.share(w.anna, w.bob)
				w.store.accounts[w.bob] = Account{Policy: PolicyNobody}
			},
			want: false,
		},
		{
			name: "a friend opens the door under friends",
			setup: func(w *world) {
				w.store.accounts[w.bob] = Account{Policy: PolicyFriends, FriendRequestPolicy: FriendRequestsShared}
				w.friendship.add(w.anna, w.bob)
			},
			want: true,
		},
		{
			name: "a stranger gets no door under friends",
			setup: func(w *world) {
				w.store.share(w.anna, w.bob)
				w.store.accounts[w.bob] = Account{Policy: PolicyFriends, FriendRequestPolicy: FriendRequestsShared}
			},
			want: false,
		},
		{
			name: "the recipient blocked the caller",
			setup: func(w *world) {
				w.store.share(w.anna, w.bob)
				w.store.block(w.bob, w.anna)
			},
			want: false,
		},
		{
			name: "the caller blocked the recipient",
			setup: func(w *world) {
				w.store.share(w.anna, w.bob)
				w.store.block(w.anna, w.bob)
			},
			want: false,
		},
		{
			name: "a guest may not start a dialogue",
			setup: func(w *world) {
				w.store.share(w.anna, w.bob)
				w.store.accounts[w.anna] = Account{Policy: PolicyShared, IsGuest: true}
			},
			want: false,
		},
		{
			name:  "an unknown recipient",
			setup: func(w *world) { w.store.share(w.anna, w.bob) },
			want:  false,
			// bob is replaced below by an id the store does not know.
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			tt.setup(w)
			to := w.bob
			if tt.name == "an unknown recipient" {
				to = uuid.New()
			}
			allowed, err := w.svc.CanStartDirect(t.Context(), w.anna, to)
			if err != nil {
				t.Fatalf("CanStartDirect: %v", err)
			}
			if allowed != tt.want {
				t.Errorf("allowed = %v, want %v", allowed, tt.want)
			}
		})
	}
}

// A guest cannot open a group either, and a blocked account cannot be added to
// one. The recipient's dm_policy is about dialogues, so a group is not refused
// because someone set it to nobody.
func TestCanStartGroup(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*world)
		want  bool
	}{
		{
			name:  "a plain group",
			setup: func(*world) {},
			want:  true,
		},
		{
			name: "the caller is a guest",
			setup: func(w *world) {
				w.store.accounts[w.anna] = Account{Policy: PolicyShared, IsGuest: true}
			},
			want: false,
		},
		{
			name: "a member blocked the caller",
			setup: func(w *world) {
				w.store.block(w.vika, w.anna)
			},
			want: false,
		},
		{
			name: "the caller blocked a member",
			setup: func(w *world) {
				w.store.block(w.anna, w.vika)
			},
			want: false,
		},
		{
			name: "a member set dm_policy to nobody",
			setup: func(w *world) {
				w.store.accounts[w.vika] = Account{Policy: PolicyNobody}
			},
			want: true,
		},
		{
			name:  "an unknown member",
			setup: func(*world) {},
			want:  false,
			// vika is replaced below by an id the store does not know.
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			tt.setup(w)
			member := w.vika
			if tt.name == "an unknown member" {
				member = uuid.New()
			}
			allowed, err := w.svc.CanStartGroup(t.Context(), w.anna, []uuid.UUID{w.bob, member})
			if err != nil {
				t.Fatalf("CanStartGroup: %v", err)
			}
			if allowed != tt.want {
				t.Errorf("allowed = %v, want %v", allowed, tt.want)
			}
		})
	}
}

// "You may not talk to them" and "there is no such account" are one answer, or
// the API tells strangers which accounts exist.
func TestRefusalAndUnknownAccountLookTheSame(t *testing.T) {
	w := newWorld(t)
	w.store.accounts[w.bob] = Account{Policy: PolicyNobody}

	refused, err := w.svc.CanStartDirect(t.Context(), w.anna, w.bob)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	unknown, err := w.svc.CanStartDirect(t.Context(), w.anna, uuid.New())
	if err != nil {
		t.Fatalf("unknown: %v", err)
	}
	if refused || unknown {
		t.Errorf("refused = %v, unknown = %v, want both false", refused, unknown)
	}
}

func TestBlockAndUnblock(t *testing.T) {
	w := newWorld(t)

	if err := w.svc.Block(t.Context(), w.anna, w.anna); !errors.Is(err, ErrCannotBlockSelf) {
		t.Errorf("blocking myself: err = %v, want ErrCannotBlockSelf", err)
	}
	if err := w.svc.Block(t.Context(), w.anna, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("blocking an unknown account: err = %v, want ErrNotFound", err)
	}

	// Blocking twice is fine: a retry after a lost response is harmless.
	for range 2 {
		if err := w.svc.Block(t.Context(), w.anna, w.bob); err != nil {
			t.Fatalf("Block: %v", err)
		}
	}
	blocked, err := w.svc.BlockedEitherWay(t.Context(), w.anna, w.bob)
	if err != nil {
		t.Fatalf("BlockedEitherWay: %v", err)
	}
	if !blocked {
		t.Error("a block is not reported in either direction")
	}

	if err := w.svc.Unblock(t.Context(), w.anna, w.bob); err != nil {
		t.Fatalf("Unblock: %v", err)
	}
	if err := w.svc.Unblock(t.Context(), w.anna, w.bob); err != nil {
		t.Errorf("unblocking twice: %v", err)
	}
	blocked, err = w.svc.BlockedEitherWay(t.Context(), w.anna, w.bob)
	if err != nil {
		t.Fatalf("BlockedEitherWay: %v", err)
	}
	if blocked {
		t.Error("the block survived the unblock")
	}
}

// Blocks belong to the account that made them: somebody else's list is not
// readable through any argument this module takes.
func TestEveryAccountSeesOnlyItsOwnBlocks(t *testing.T) {
	w := newWorld(t)
	if err := w.svc.Block(t.Context(), w.anna, w.bob); err != nil {
		t.Fatalf("Block: %v", err)
	}
	if err := w.svc.Block(t.Context(), w.vika, w.bob); err != nil {
		t.Fatalf("Block: %v", err)
	}

	mine, err := w.svc.ListBlocks(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("ListBlocks: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != w.bob {
		t.Errorf("Anna's list = %+v, want only Bob", mine)
	}

	none, err := w.svc.ListBlocks(t.Context(), w.bob)
	if err != nil {
		t.Fatalf("ListBlocks: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("Bob sees %d blocks, want 0", len(none))
	}
}

func TestDMPolicy(t *testing.T) {
	w := newWorld(t)

	policy, err := w.svc.Policy(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	if policy != PolicyShared {
		t.Errorf("default policy = %q, want %q", policy, PolicyShared)
	}

	if err := w.svc.SetPolicy(t.Context(), w.anna, PolicyNobody); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	policy, err = w.svc.Policy(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	if policy != PolicyNobody {
		t.Errorf("policy = %q, want %q", policy, PolicyNobody)
	}

	if err := w.svc.SetPolicy(t.Context(), w.anna, Policy("everyone")); !errors.Is(err, ErrInvalidPolicy) {
		t.Errorf("unknown policy: err = %v, want ErrInvalidPolicy", err)
	}
	if _, err := w.svc.Policy(t.Context(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("policy of an unknown account: err = %v, want ErrNotFound", err)
	}
}

func TestParsePolicy(t *testing.T) {
	for _, value := range []string{"shared", "friends", "nobody"} {
		if _, err := ParsePolicy(value); err != nil {
			t.Errorf("ParsePolicy(%q): %v", value, err)
		}
	}
	if _, err := ParsePolicy("everyone"); !errors.Is(err, ErrInvalidPolicy) {
		t.Errorf("ParsePolicy(everyone): err = %v, want ErrInvalidPolicy", err)
	}
}

// The friend request setting has three values, and a shared server is the proof
// of acquaintance behind the default one. A block outranks every value, and a
// guest neither sends nor receives.
func TestCanRequestFriendship(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*world)
		want  bool
	}{
		{
			name: "all lets a stranger ask",
			setup: func(w *world) {
				w.store.accounts[w.bob] = Account{Policy: PolicyShared, FriendRequestPolicy: FriendRequestsAll}
			},
			want: true,
		},
		{
			name: "shared lets a person on a common server ask",
			setup: func(w *world) {
				w.store.accounts[w.bob] = Account{Policy: PolicyShared, FriendRequestPolicy: FriendRequestsShared}
				w.store.share(w.anna, w.bob)
			},
			want: true,
		},
		{
			name: "shared keeps a stranger out",
			setup: func(w *world) {
				w.store.accounts[w.bob] = Account{Policy: PolicyShared, FriendRequestPolicy: FriendRequestsShared}
			},
			want: false,
		},
		{
			name: "nobody keeps everyone out",
			setup: func(w *world) {
				w.store.accounts[w.bob] = Account{Policy: PolicyShared, FriendRequestPolicy: FriendRequestsNobody}
				w.store.share(w.anna, w.bob)
			},
			want: false,
		},
		{
			name: "the recipient blocked the caller",
			setup: func(w *world) {
				w.store.accounts[w.bob] = Account{Policy: PolicyShared, FriendRequestPolicy: FriendRequestsAll}
				w.store.block(w.bob, w.anna)
			},
			want: false,
		},
		{
			name: "the caller blocked the recipient",
			setup: func(w *world) {
				w.store.accounts[w.bob] = Account{Policy: PolicyShared, FriendRequestPolicy: FriendRequestsAll}
				w.store.block(w.anna, w.bob)
			},
			want: false,
		},
		{
			name: "a guest may not send",
			setup: func(w *world) {
				w.store.accounts[w.bob] = Account{Policy: PolicyShared, FriendRequestPolicy: FriendRequestsAll}
				w.store.accounts[w.anna] = Account{Policy: PolicyShared, FriendRequestPolicy: FriendRequestsAll, IsGuest: true}
			},
			want: false,
		},
		{
			name: "a guest may not receive",
			setup: func(w *world) {
				w.store.accounts[w.bob] = Account{Policy: PolicyShared, FriendRequestPolicy: FriendRequestsAll, IsGuest: true}
			},
			want: false,
		},
		{
			name: "the account does not exist",
			setup: func(w *world) {
				w.store.accounts[w.bob] = Account{Policy: PolicyShared, FriendRequestPolicy: FriendRequestsAll}
			},
			want: false,
			// bob is replaced below by an id the store does not know.
		},
		{
			name:  "asking yourself",
			setup: func(*world) {},
			want:  false,
			// anna asks anna below.
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			tt.setup(w)
			to := w.bob
			switch tt.name {
			case "the account does not exist":
				to = uuid.New()
			case "asking yourself":
				to = w.anna
			}
			allowed, err := w.svc.CanRequestFriendship(t.Context(), w.anna, to)
			if err != nil {
				t.Fatalf("CanRequestFriendship: %v", err)
			}
			if allowed != tt.want {
				t.Errorf("allowed = %v, want %v", allowed, tt.want)
			}
		})
	}
}

func TestFriendRequestPolicySetting(t *testing.T) {
	w := newWorld(t)

	policy, err := w.svc.FriendRequestPolicy(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("FriendRequestPolicy: %v", err)
	}
	if policy != FriendRequestsShared {
		t.Errorf("default policy = %q, want %q", policy, FriendRequestsShared)
	}

	if err := w.svc.SetFriendRequestPolicy(t.Context(), w.anna, FriendRequestsNobody); err != nil {
		t.Fatalf("SetFriendRequestPolicy: %v", err)
	}
	policy, err = w.svc.FriendRequestPolicy(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("FriendRequestPolicy: %v", err)
	}
	if policy != FriendRequestsNobody {
		t.Errorf("policy = %q, want %q", policy, FriendRequestsNobody)
	}

	if err := w.svc.SetFriendRequestPolicy(t.Context(), w.anna, FriendRequestPolicy("everyone")); !errors.Is(err, ErrInvalidFriendRequestPolicy) {
		t.Errorf("unknown policy: err = %v, want ErrInvalidFriendRequestPolicy", err)
	}
	if _, err := w.svc.FriendRequestPolicy(t.Context(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("policy of an unknown account: err = %v, want ErrNotFound", err)
	}
}

func TestParseFriendRequestPolicy(t *testing.T) {
	for _, value := range []string{"all", "shared", "nobody"} {
		if _, err := ParseFriendRequestPolicy(value); err != nil {
			t.Errorf("ParseFriendRequestPolicy(%q): %v", value, err)
		}
	}
	if _, err := ParseFriendRequestPolicy("everyone"); !errors.Is(err, ErrInvalidFriendRequestPolicy) {
		t.Errorf("ParseFriendRequestPolicy(everyone): err = %v, want ErrInvalidFriendRequestPolicy", err)
	}
}

// A block is final for a friendship too: privacy tells the friends module to
// withdraw the pair after the block is stored.
func TestBlockAsksFriendsToWithdrawThePair(t *testing.T) {
	w := newWorld(t)
	cleanup := &fakeBlockCleanup{}
	w.svc.SetBlockCleanup(cleanup)

	if err := w.svc.Block(t.Context(), w.anna, w.bob); err != nil {
		t.Fatalf("Block: %v", err)
	}
	if len(cleanup.pairs) != 1 || cleanup.pairs[0] != [2]uuid.UUID{w.anna, w.bob} {
		t.Errorf("cleanup = %v, want one call for anna and bob", cleanup.pairs)
	}
}

func TestIsGuest(t *testing.T) {
	w := newWorld(t)
	guest := uuid.New()
	w.store.add(guest, PolicyShared, true)

	isGuest, err := w.svc.IsGuest(t.Context(), guest)
	if err != nil {
		t.Fatalf("IsGuest: %v", err)
	}
	if !isGuest {
		t.Error("a guest is not reported as one")
	}
	isGuest, err = w.svc.IsGuest(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("IsGuest: %v", err)
	}
	if isGuest {
		t.Error("a regular account is reported as a guest")
	}
}
