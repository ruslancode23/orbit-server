package servers

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMutualServersAreTheIntersectionWithTheCallersRole(t *testing.T) {
	store := newFakeStore()
	storage := newFakeStorage()
	svc := NewService(store, nil, Icons{Storage: storage, LinkTTL: time.Hour})

	anna, bob, outsider := uuid.New(), uuid.New(), uuid.New()
	shared, annaOnly, bobOnly := uuid.New(), uuid.New(), uuid.New()

	seed := func(id uuid.UUID, name, iconKey string, members map[uuid.UUID]Role) {
		store.workspaces[id] = Workspace{ID: id, Name: name, IconKey: iconKey, OwnerID: bob}
		store.members[id] = map[uuid.UUID]Member{}
		for userID, role := range members {
			store.members[id][userID] = Member{User: store.account(userID), Role: role}
		}
	}
	seed(shared, "Orbit", "workspaces/shared/icon/one.png", map[uuid.UUID]Role{
		anna: RoleAdmin, bob: RoleOwner,
	})
	seed(annaOnly, "Anna only", "", map[uuid.UUID]Role{anna: RoleMember})
	seed(bobOnly, "Bob only", "", map[uuid.UUID]Role{bob: RoleMember})

	got, err := svc.MutualServers(t.Context(), anna, bob)
	if err != nil {
		t.Fatalf("MutualServers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("servers = %+v, want only the shared one", got)
	}
	server := got[0]
	if server.ID != shared || server.Name != "Orbit" {
		t.Errorf("server = %+v, want the shared Orbit", server)
	}
	if server.Role != string(RoleAdmin) {
		t.Errorf("role = %q, want the caller's admin role", server.Role)
	}
	if !strings.Contains(server.IconURL, "workspaces/shared/icon/one.png") || !strings.Contains(server.IconURL, "signature=") {
		t.Errorf("icon = %q, want a signed link to the icon key", server.IconURL)
	}

	// The role follows the caller, not the account being looked at.
	got, err = svc.MutualServers(t.Context(), bob, anna)
	if err != nil {
		t.Fatalf("MutualServers reversed: %v", err)
	}
	if len(got) != 1 || got[0].Role != string(RoleOwner) {
		t.Errorf("servers = %+v, want bob's owner role", got)
	}

	// A pair with no common server gets an empty list, and asking about yourself
	// is empty rather than a list of all your servers.
	for _, tt := range []struct {
		name string
		a, b uuid.UUID
	}{
		{name: "no common server", a: anna, b: outsider},
		{name: "self", a: anna, b: anna},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := svc.MutualServers(t.Context(), tt.a, tt.b)
			if err != nil {
				t.Fatalf("MutualServers: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("servers = %+v, want none", got)
			}
		})
	}
}
