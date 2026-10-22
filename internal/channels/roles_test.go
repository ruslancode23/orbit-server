package channels

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/perms"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

// staffServer is a server with @everyone and a «Персонал» role that may
// manage channels; members hold @everyone only.
type staffServer struct {
	workspace, everyone, staff uuid.UUID
	members                    map[uuid.UUID][]uuid.UUID
}

func (s staffServer) Access(_ context.Context, workspaceID, userID uuid.UUID) (servers.Access, error) {
	held, ok := s.members[userID]
	if !ok || workspaceID != s.workspace {
		return servers.Access{}, servers.ErrNotMember
	}
	return servers.Access{
		Server: perms.Server{Roles: []perms.Role{
			{ID: s.everyone, Default: true, Permissions: perms.ViewChannel | perms.SendMessages | perms.Speak},
			{ID: s.staff, Position: 1, Permissions: perms.ManageChannels},
		}},
		Member: perms.Member{Roles: held},
	}, nil
}

// A staff room: hidden from @everyone, open to the staff role. Members do not
// see it in the list and cannot reach it by id — the answer is «not found»,
// not «forbidden», so its existence stays hidden too.
func TestHiddenChannelForEveryoneButStaff(t *testing.T) {
	store := newFakeStore()
	server := staffServer{workspace: uuid.New(), everyone: uuid.New(), staff: uuid.New()}
	member, moderator := uuid.New(), uuid.New()
	server.members = map[uuid.UUID][]uuid.UUID{member: nil, moderator: {server.staff}}
	svc := NewService(store, server)

	general, err := store.Create(t.Context(), NewChannel{WorkspaceID: server.workspace, Kind: KindText, Name: "общий"})
	if err != nil {
		t.Fatal(err)
	}
	room, err := store.Create(t.Context(), NewChannel{WorkspaceID: server.workspace, Kind: KindText, Name: "персонал"})
	if err != nil {
		t.Fatal(err)
	}
	stored := store.channels[room.ID]
	stored.Overrides = []perms.Override{
		{RoleID: server.everyone, Deny: perms.ViewChannel},
		{RoleID: server.staff, Allow: perms.ViewChannel},
	}
	store.channels[room.ID] = stored

	list, err := svc.List(t.Context(), server.workspace, member)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Channel.ID != general.ID {
		t.Errorf("member sees %d channels, want only the general one", len(list))
	}
	if _, err := svc.Get(t.Context(), room.ID, member); !errors.Is(err, ErrNotFound) {
		t.Errorf("member Get: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.MayWrite(t.Context(), room.ID, member); !errors.Is(err, ErrNotFound) {
		t.Errorf("member MayWrite: err = %v, want ErrNotFound", err)
	}

	list, err = svc.List(t.Context(), server.workspace, moderator)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("staff sees %d channels, want both", len(list))
	}
	view, err := svc.Get(t.Context(), room.ID, moderator)
	if err != nil {
		t.Fatalf("staff Get: %v", err)
	}
	if !view.CanWrite || !view.Permissions.Has(perms.ManageChannels) {
		t.Errorf("staff view = %+v, want writing and managing", view)
	}

	visible, err := svc.Visible(t.Context(), server.workspace, member)
	if err != nil {
		t.Fatalf("Visible: %v", err)
	}
	if visible[room.ID] || !visible[general.ID] {
		t.Errorf("Visible = %v, want the general channel only", visible)
	}
}

// manageChannels from a role is enough to rename a channel; a plain member
// gets ErrForbidden on a channel they can see.
func TestManageChannelsFromARole(t *testing.T) {
	store := newFakeStore()
	server := staffServer{workspace: uuid.New(), everyone: uuid.New(), staff: uuid.New()}
	member, moderator := uuid.New(), uuid.New()
	server.members = map[uuid.UUID][]uuid.UUID{member: nil, moderator: {server.staff}}
	svc := NewService(store, server)
	channel, err := store.Create(t.Context(), NewChannel{WorkspaceID: server.workspace, Kind: KindText, Name: "общий"})
	if err != nil {
		t.Fatal(err)
	}

	name := "болталка"
	if _, err := svc.Update(t.Context(), channel.ID, member, Update{Name: &name}); !errors.Is(err, ErrForbidden) {
		t.Errorf("member rename: err = %v, want ErrForbidden", err)
	}
	if _, err := svc.Update(t.Context(), channel.ID, moderator, Update{Name: &name}); err != nil {
		t.Errorf("staff rename: %v", err)
	}
}
