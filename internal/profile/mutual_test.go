package profile

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

// mutualHarness seeds a pair of accounts with a common server, a common friend
// and one-sided data on both sides, so a test can tell an intersection from a
// union.
type mutualHarness struct {
	*harness
	anna, bob   uuid.UUID
	vika        uuid.UUID
	server      uuid.UUID
	annaOnlySrv uuid.UUID
	bobOnlySrv  uuid.UUID
}

func newMutualHarness(t *testing.T) *mutualHarness {
	t.Helper()
	h := newHarness(t)
	m := &mutualHarness{
		harness:     h,
		anna:        uuid.New(),
		bob:         uuid.New(),
		vika:        uuid.New(),
		server:      uuid.New(),
		annaOnlySrv: uuid.New(),
		bobOnlySrv:  uuid.New(),
	}
	for id, nickname := range map[uuid.UUID]string{
		m.anna: "anna", m.bob: "bob", m.vika: "vika",
	} {
		m.seed(id, nickname)
	}

	m.servers.servers[m.anna] = []uuid.UUID{m.server, m.annaOnlySrv}
	m.servers.servers[m.bob] = []uuid.UUID{m.server, m.bobOnlySrv}
	m.servers.details[m.server] = MutualServer{
		Name:    "Orbit",
		IconURL: "https://store.test/workspaces/orbit/icon.webp?signature=x",
		Role:    "admin",
	}
	m.servers.details[m.annaOnlySrv] = MutualServer{Name: "Anna only", Role: "member"}
	m.servers.details[m.bobOnlySrv] = MutualServer{Name: "Bob only", Role: "member"}

	m.friends.friends[m.anna] = []uuid.UUID{m.vika}
	m.friends.friends[m.bob] = []uuid.UUID{m.vika}
	return m
}

func TestMutualListsAreTheIntersection(t *testing.T) {
	m := newMutualHarness(t)
	// An account only one side is friends with must not show up.
	annaOnly, bobOnly := uuid.New(), uuid.New()
	m.seed(annaOnly, "dina")
	m.seed(bobOnly, "egor")
	m.friends.friends[m.anna] = append(m.friends.friends[m.anna], annaOnly)
	m.friends.friends[m.bob] = append(m.friends.friends[m.bob], bobOnly)

	mutual, err := m.svc.Mutual(t.Context(), m.anna, m.bob)
	if err != nil {
		t.Fatalf("Mutual: %v", err)
	}

	if len(mutual.Servers) != 1 {
		t.Fatalf("servers = %+v, want only the shared one", mutual.Servers)
	}
	server := mutual.Servers[0]
	if server.ID != m.server || server.Name != "Orbit" || server.Role != "admin" {
		t.Errorf("server = %+v, want the shared server with the caller's role", server)
	}
	if server.IconURL != m.servers.details[m.server].IconURL {
		t.Errorf("icon = %q, want the signed link from the servers module", server.IconURL)
	}

	if len(mutual.Friends) != 1 || mutual.Friends[0].ID != m.vika {
		t.Fatalf("friends = %+v, want only vika", mutual.Friends)
	}
	if mutual.Friends[0].Nickname != "vika" || mutual.Friends[0].DisplayName != "vika" {
		t.Errorf("friend = %+v, want the filled profile", mutual.Friends[0])
	}
}

func TestMutualSortsFriendsByNickname(t *testing.T) {
	m := newMutualHarness(t)
	zoe, alice := uuid.New(), uuid.New()
	m.seed(zoe, "zoe")
	m.seed(alice, "alice")
	m.friends.friends[m.anna] = []uuid.UUID{zoe, m.vika, alice}
	m.friends.friends[m.bob] = []uuid.UUID{alice, zoe, m.vika}

	mutual, err := m.svc.Mutual(t.Context(), m.anna, m.bob)
	if err != nil {
		t.Fatalf("Mutual: %v", err)
	}
	got := make([]string, 0, len(mutual.Friends))
	for _, friend := range mutual.Friends {
		got = append(got, friend.Nickname)
	}
	want := []string{"alice", "vika", "zoe"}
	if len(got) != len(want) {
		t.Fatalf("friends = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("friends = %v, want %v", got, want)
		}
	}
}

func TestMutualNeedsTheProfileAudience(t *testing.T) {
	h := newHarness(t)
	anna, bob, vika, dina, blocked, stranger := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{anna, bob, vika, dina, blocked, stranger} {
		h.seed(id, "user")
	}

	shared := uuid.New()
	h.servers.servers[anna] = []uuid.UUID{shared}
	h.servers.servers[bob] = []uuid.UUID{shared}
	h.servers.servers[blocked] = []uuid.UUID{shared}
	h.friends.pairs[pairKey(anna, vika)] = true
	h.conversations.shared[pairKey(anna, dina)] = true
	h.privacy.blocked[pairKey(anna, blocked)] = true

	tests := []struct {
		name   string
		target uuid.UUID
		err    error
	}{
		{name: "shared server", target: bob},
		{name: "friendship", target: vika},
		{name: "shared conversation", target: dina},
		{name: "stranger", target: stranger, err: ErrNotFound},
		{name: "unknown account", target: uuid.New(), err: ErrNotFound},
		{name: "blocked pair", target: blocked, err: ErrNotFound},
		{name: "self", target: anna, err: ErrSelf},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.svc.Mutual(t.Context(), anna, tt.target)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
		})
	}
}

func TestMutualLoadsEveryListInOneCall(t *testing.T) {
	m := newMutualHarness(t)
	// Both sides have many friends; only the intersection is loaded, and it is
	// loaded in one batch rather than one profile read per friend.
	const sharedFriends = 7
	m.friends.friends[m.anna] = nil
	m.friends.friends[m.bob] = nil
	for i := 0; i < sharedFriends; i++ {
		friend := uuid.New()
		m.seed(friend, "shared-"+uuid.NewString()[:8])
		m.friends.friends[m.anna] = append(m.friends.friends[m.anna], friend)
		m.friends.friends[m.bob] = append(m.friends.friends[m.bob], friend)

		annaOnly, bobOnly := uuid.New(), uuid.New()
		m.seed(annaOnly, "anna-only")
		m.seed(bobOnly, "bob-only")
		m.friends.friends[m.anna] = append(m.friends.friends[m.anna], annaOnly)
		m.friends.friends[m.bob] = append(m.friends.friends[m.bob], bobOnly)
	}

	mutual, err := m.svc.Mutual(t.Context(), m.anna, m.bob)
	if err != nil {
		t.Fatalf("Mutual: %v", err)
	}
	if len(mutual.Friends) != sharedFriends {
		t.Fatalf("friends = %d, want the %d shared ones", len(mutual.Friends), sharedFriends)
	}
	for _, friend := range mutual.Friends {
		if friend.DisplayName == "" {
			t.Errorf("friend %v has no profile: the batch did not fill it", friend.ID)
		}
	}

	// The cost is constant: two friend lists, one batch of profiles, one server
	// query — never one call per friend or per server.
	if m.store.batches != 1 {
		t.Errorf("profile batches = %d, want 1", m.store.batches)
	}
	if m.store.reads != 0 {
		t.Errorf("single profile reads = %d, want 0 (use the batch)", m.store.reads)
	}
	if m.friends.friendsOfCalls != 2 {
		t.Errorf("friend list calls = %d, want 2 (one per account)", m.friends.friendsOfCalls)
	}
	if m.servers.mutualCalls != 1 {
		t.Errorf("mutual server calls = %d, want 1", m.servers.mutualCalls)
	}
}

func TestHandlerMutualMapsServersAndFriends(t *testing.T) {
	m := newMutualHarness(t)
	handler := NewHandler(m.svc, testLogger())

	resp, err := handler.GetMutual(sessionFor(m.anna), api.GetMutualRequestObject{UserId: m.bob})
	if err != nil {
		t.Fatalf("GetMutual: %v", err)
	}
	ok, isOK := resp.(api.GetMutual200JSONResponse)
	if !isOK {
		t.Fatalf("response = %T, want 200", resp)
	}
	if len(ok.Servers) != 1 || ok.Servers[0].Id != m.server {
		t.Fatalf("servers = %+v, want the shared one", ok.Servers)
	}
	if ok.Servers[0].IconUrl == nil || *ok.Servers[0].IconUrl == "" {
		t.Errorf("iconUrl = %v, want the signed link", ok.Servers[0].IconUrl)
	}
	if ok.Servers[0].Role != api.Admin {
		t.Errorf("role = %q, want the caller's admin role", ok.Servers[0].Role)
	}
	if len(ok.Friends) != 1 || ok.Friends[0].Id != m.vika || ok.Friends[0].DisplayName != "vika" {
		t.Errorf("friends = %+v, want vika as a UserProfile", ok.Friends)
	}
}

func TestHandlerMutualAnswersErrors(t *testing.T) {
	m := newMutualHarness(t)
	handler := NewHandler(m.svc, testLogger())
	stranger := uuid.New()
	m.seed(stranger, "stranger")

	t.Run("no session", func(t *testing.T) {
		resp, err := handler.GetMutual(t.Context(), api.GetMutualRequestObject{UserId: m.bob})
		if err != nil {
			t.Fatalf("GetMutual: %v", err)
		}
		if _, ok := resp.(api.GetMutual401JSONResponse); !ok {
			t.Errorf("response = %T, want 401", resp)
		}
	})

	t.Run("self", func(t *testing.T) {
		resp, err := handler.GetMutual(sessionFor(m.anna), api.GetMutualRequestObject{UserId: m.anna})
		if err != nil {
			t.Fatalf("GetMutual: %v", err)
		}
		bad, ok := resp.(api.GetMutual400JSONResponse)
		if !ok {
			t.Fatalf("response = %T, want 400", resp)
		}
		if bad.Error.Code != httpx.CodeInvalidRequest || bad.Error.Message == "" {
			t.Errorf("error = %+v, want invalid_request with a message", bad.Error)
		}
	})

	t.Run("stranger", func(t *testing.T) {
		resp, err := handler.GetMutual(sessionFor(m.anna), api.GetMutualRequestObject{UserId: stranger})
		if err != nil {
			t.Fatalf("GetMutual: %v", err)
		}
		if _, ok := resp.(api.GetMutual404JSONResponse); !ok {
			t.Errorf("response = %T, want 404", resp)
		}
	})
}
