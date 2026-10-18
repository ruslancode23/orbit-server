package channels

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/servers"
)

// fakeStore keeps channels in memory with the rules the database enforces:
// names unique per server, and a new channel at the end of the list.
type fakeStore struct {
	channels map[uuid.UUID]Channel
}

func newFakeStore() *fakeStore { return &fakeStore{channels: map[uuid.UUID]Channel{}} }

func (s *fakeStore) Create(_ context.Context, c NewChannel) (Channel, error) {
	position := 0
	for _, existing := range s.channels {
		if existing.WorkspaceID != c.WorkspaceID {
			continue
		}
		if strings.EqualFold(existing.Name, c.Name) {
			return Channel{}, ErrNameTaken
		}
		if existing.Position >= position {
			position = existing.Position + 1
		}
	}
	channel := Channel{
		ID: uuid.New(), WorkspaceID: c.WorkspaceID, Name: c.Name, Topic: c.Topic,
		Position: position, IsReadOnly: c.IsReadOnly, CreatedAt: time.Now(),
	}
	s.channels[channel.ID] = channel
	return channel, nil
}

func (s *fakeStore) Channel(_ context.Context, id uuid.UUID) (Channel, error) {
	channel, ok := s.channels[id]
	if !ok {
		return Channel{}, ErrNotFound
	}
	return channel, nil
}

func (s *fakeStore) List(_ context.Context, workspaceID uuid.UUID) ([]Channel, error) {
	var out []Channel
	for _, c := range s.channels {
		if c.WorkspaceID == workspaceID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out, nil
}

func (s *fakeStore) Update(_ context.Context, id uuid.UUID, u Update) (Channel, error) {
	channel, ok := s.channels[id]
	if !ok {
		return Channel{}, ErrNotFound
	}
	if u.Name != nil {
		for _, other := range s.channels {
			if other.ID != id && other.WorkspaceID == channel.WorkspaceID && strings.EqualFold(other.Name, *u.Name) {
				return Channel{}, ErrNameTaken
			}
		}
		channel.Name = *u.Name
	}
	if u.Topic != nil {
		channel.Topic = *u.Topic
	}
	if u.IsReadOnly != nil {
		channel.IsReadOnly = *u.IsReadOnly
	}
	s.channels[id] = channel
	return channel, nil
}

func (s *fakeStore) Delete(_ context.Context, id uuid.UUID) error {
	delete(s.channels, id)
	return nil
}

func (s *fakeStore) Reorder(_ context.Context, workspaceID uuid.UUID, ids []uuid.UUID) error {
	for position, id := range ids {
		channel, ok := s.channels[id]
		if !ok || channel.WorkspaceID != workspaceID {
			return ErrIncompleteOrder
		}
		channel.Position = position
		s.channels[id] = channel
	}
	return nil
}

// fakeRoles answers like internal/servers: a role per (server, account).
type fakeRoles struct {
	roles map[uuid.UUID]map[uuid.UUID]servers.Role
}

func (r *fakeRoles) RequireAtLeast(_ context.Context, workspaceID, userID uuid.UUID, least servers.Role) (servers.Role, error) {
	role, ok := r.roles[workspaceID][userID]
	if !ok {
		return "", servers.ErrNotMember
	}
	rank := map[servers.Role]int{servers.RoleMember: 1, servers.RoleAdmin: 2, servers.RoleOwner: 3}
	if rank[role] < rank[least] {
		return role, servers.ErrForbidden
	}
	return role, nil
}

type world struct {
	svc       *Service
	store     *fakeStore
	workspace uuid.UUID
	other     uuid.UUID // a second server, to prove isolation
	owner     uuid.UUID
	admin     uuid.UUID
	member    uuid.UUID
	outsider  uuid.UUID
	general   Channel
}

func newWorld(t *testing.T) world {
	t.Helper()
	store := newFakeStore()
	w := world{
		workspace: uuid.New(), other: uuid.New(),
		owner: uuid.New(), admin: uuid.New(), member: uuid.New(), outsider: uuid.New(),
		store: store,
	}
	roles := &fakeRoles{roles: map[uuid.UUID]map[uuid.UUID]servers.Role{
		w.workspace: {w.owner: servers.RoleOwner, w.admin: servers.RoleAdmin, w.member: servers.RoleMember},
		w.other:     {w.outsider: servers.RoleOwner},
	}}
	w.svc = NewService(store, roles)

	general, err := w.svc.Create(t.Context(), w.workspace, w.admin, NewChannel{Name: "общий"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	w.general = general.Channel
	return w
}

func TestCreateAppendsToTheEndAndTrims(t *testing.T) {
	w := newWorld(t)

	second, err := w.svc.Create(t.Context(), w.workspace, w.owner, NewChannel{Name: "  задачи  ", Topic: "  что делаем  "})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if second.Channel.Name != "задачи" || second.Channel.Topic != "что делаем" {
		t.Errorf("channel = %+v, want the name and topic trimmed", second.Channel)
	}
	if second.Channel.Position <= w.general.Position {
		t.Errorf("position = %d, want it after %d", second.Channel.Position, w.general.Position)
	}
	if !second.CanWrite {
		t.Error("the owner cannot write in a normal channel")
	}
}

func TestCreateRejectsBadNameTopicAndDuplicate(t *testing.T) {
	w := newWorld(t)

	for _, tc := range []struct {
		name    string
		channel NewChannel
		want    error
	}{
		{"empty name", NewChannel{Name: "   "}, ErrInvalidName},
		{"name too long", NewChannel{Name: strings.Repeat("я", maxNameLength+1)}, ErrInvalidName},
		{"topic too long", NewChannel{Name: "новый", Topic: strings.Repeat("я", maxTopicLength+1)}, ErrInvalidTopic},
		{"name taken in another case", NewChannel{Name: "ОБЩИЙ"}, ErrNameTaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.svc.Create(t.Context(), w.workspace, w.admin, tc.channel); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// Who may do what with channels: the rule is the role on the server.
func TestPermissionMatrix(t *testing.T) {
	actions := []struct {
		name string
		run  func(w world, caller uuid.UUID) error
	}{
		{"list", func(w world, caller uuid.UUID) error {
			_, err := w.svc.List(context.Background(), w.workspace, caller)
			return err
		}},
		{"read one", func(w world, caller uuid.UUID) error {
			_, err := w.svc.Get(context.Background(), w.general.ID, caller)
			return err
		}},
		{"create", func(w world, caller uuid.UUID) error {
			_, err := w.svc.Create(context.Background(), w.workspace, caller, NewChannel{Name: "новый"})
			return err
		}},
		{"rename", func(w world, caller uuid.UUID) error {
			name := "переименован"
			_, err := w.svc.Update(context.Background(), w.general.ID, caller, Update{Name: &name})
			return err
		}},
		{"delete", func(w world, caller uuid.UUID) error {
			return w.svc.Delete(context.Background(), w.general.ID, caller)
		}},
		{"reorder", func(w world, caller uuid.UUID) error {
			_, err := w.svc.Reorder(context.Background(), w.workspace, caller, []uuid.UUID{w.general.ID})
			return err
		}},
	}

	want := map[string]map[string]error{
		"list":     {"owner": nil, "admin": nil, "member": nil, "outsider": ErrNotFound},
		"read one": {"owner": nil, "admin": nil, "member": nil, "outsider": ErrNotFound},
		"create":   {"owner": nil, "admin": nil, "member": ErrForbidden, "outsider": ErrNotFound},
		"rename":   {"owner": nil, "admin": nil, "member": ErrForbidden, "outsider": ErrNotFound},
		"delete":   {"owner": nil, "admin": nil, "member": ErrForbidden, "outsider": ErrNotFound},
		"reorder":  {"owner": nil, "admin": nil, "member": ErrForbidden, "outsider": ErrNotFound},
	}

	for _, action := range actions {
		for _, role := range []string{"owner", "admin", "member", "outsider"} {
			t.Run(action.name+" as "+role, func(t *testing.T) {
				w := newWorld(t)
				caller := map[string]uuid.UUID{
					"owner": w.owner, "admin": w.admin, "member": w.member, "outsider": w.outsider,
				}[role]

				err := action.run(w, caller)
				expected := want[action.name][role]
				switch {
				case expected == nil && err != nil:
					t.Errorf("err = %v, want it allowed", err)
				case expected != nil && !errors.Is(err, expected):
					t.Errorf("err = %v, want %v", err, expected)
				}
			})
		}
	}
}

// A read-only channel is for announcements: members read, admins and the owner write.
func TestReadOnlyChannelWriteAccess(t *testing.T) {
	w := newWorld(t)
	announcements, err := w.svc.Create(t.Context(), w.workspace, w.admin, NewChannel{Name: "объявления", IsReadOnly: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, tc := range []struct {
		role     string
		caller   uuid.UUID
		canWrite bool
	}{
		{"owner", w.owner, true},
		{"admin", w.admin, true},
		{"member", w.member, false},
	} {
		t.Run(tc.role, func(t *testing.T) {
			view, err := w.svc.Get(t.Context(), announcements.Channel.ID, tc.caller)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if view.CanWrite != tc.canWrite {
				t.Errorf("canWrite = %v, want %v", view.CanWrite, tc.canWrite)
			}

			_, err = w.svc.MayWrite(t.Context(), announcements.Channel.ID, tc.caller)
			switch {
			case tc.canWrite && err != nil:
				t.Errorf("MayWrite: %v, want it allowed", err)
			case !tc.canWrite && !errors.Is(err, ErrForbidden):
				t.Errorf("MayWrite: err = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestMayWriteInANormalChannel(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.MayWrite(t.Context(), w.general.ID, w.member); err != nil {
		t.Errorf("a member cannot write in a normal channel: %v", err)
	}
	if _, err := w.svc.MayWrite(t.Context(), w.general.ID, w.outsider); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateChangesTopicAndWriteAccess(t *testing.T) {
	w := newWorld(t)
	topic := "о работе"
	readOnly := true

	updated, err := w.svc.Update(t.Context(), w.general.ID, w.admin, Update{Topic: &topic, IsReadOnly: &readOnly})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Channel.Topic != topic || !updated.Channel.IsReadOnly {
		t.Errorf("channel = %+v, want the topic and read-only set", updated.Channel)
	}

	// An empty topic clears it.
	empty := ""
	cleared, err := w.svc.Update(t.Context(), w.general.ID, w.admin, Update{Topic: &empty})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if cleared.Channel.Topic != "" {
		t.Errorf("topic = %q, want it cleared", cleared.Channel.Topic)
	}
}

func TestUpdateRejectsATakenName(t *testing.T) {
	w := newWorld(t)
	if _, err := w.svc.Create(t.Context(), w.workspace, w.admin, NewChannel{Name: "задачи"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	name := "задачи"
	if _, err := w.svc.Update(t.Context(), w.general.ID, w.admin, Update{Name: &name}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("err = %v, want ErrNameTaken", err)
	}
}

func TestReorderKeepsTheGivenOrder(t *testing.T) {
	w := newWorld(t)
	second, err := w.svc.Create(t.Context(), w.workspace, w.admin, NewChannel{Name: "задачи"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	third, err := w.svc.Create(t.Context(), w.workspace, w.admin, NewChannel{Name: "случайное"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	order := []uuid.UUID{third.Channel.ID, w.general.ID, second.Channel.ID}
	list, err := w.svc.Reorder(t.Context(), w.workspace, w.admin, order)
	if err != nil {
		t.Fatalf("Reorder: %v", err)
	}

	for i, view := range list {
		if view.Channel.ID != order[i] {
			t.Fatalf("channel %d = %v, want %v", i, view.Channel.ID, order[i])
		}
		if view.Channel.Position != i {
			t.Errorf("position of channel %d = %d, want %d", i, view.Channel.Position, i)
		}
	}

	// The order survives a plain read.
	again, err := w.svc.List(t.Context(), w.workspace, w.member)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i, view := range again {
		if view.Channel.ID != order[i] {
			t.Errorf("after a reread channel %d = %v, want %v", i, view.Channel.ID, order[i])
		}
	}
}

// A partial or foreign list is refused, so the order is never half applied.
func TestReorderRejectsAnIncompleteList(t *testing.T) {
	w := newWorld(t)
	second, err := w.svc.Create(t.Context(), w.workspace, w.admin, NewChannel{Name: "задачи"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	foreign, err := w.svc.Create(t.Context(), w.other, w.outsider, NewChannel{Name: "чужой"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, tc := range []struct {
		name string
		ids  []uuid.UUID
	}{
		{"misses a channel", []uuid.UUID{w.general.ID}},
		{"repeats one", []uuid.UUID{w.general.ID, w.general.ID}},
		{"names a channel of another server", []uuid.UUID{w.general.ID, foreign.Channel.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.svc.Reorder(t.Context(), w.workspace, w.admin, tc.ids); !errors.Is(err, ErrIncompleteOrder) {
				t.Errorf("err = %v, want ErrIncompleteOrder", err)
			}
			// Nothing moved.
			list, err := w.svc.List(t.Context(), w.workspace, w.admin)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if list[0].Channel.ID != w.general.ID || list[1].Channel.ID != second.Channel.ID {
				t.Errorf("order changed after a refused reorder: %+v", list)
			}
		})
	}
}

// Channels of one server never show up in another, whatever the caller's role elsewhere.
func TestChannelsAreIsolatedBetweenServers(t *testing.T) {
	w := newWorld(t)
	foreign, err := w.svc.Create(t.Context(), w.other, w.outsider, NewChannel{Name: "чужой"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := w.svc.Get(t.Context(), foreign.Channel.ID, w.owner); !errors.Is(err, ErrNotFound) {
		t.Errorf("the owner of one server reads a channel of another: %v", err)
	}
	if _, err := w.svc.List(t.Context(), w.other, w.admin); !errors.Is(err, ErrNotFound) {
		t.Errorf("an admin of one server lists channels of another: %v", err)
	}

	mine, err := w.svc.List(t.Context(), w.workspace, w.member)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, view := range mine {
		if view.Channel.WorkspaceID != w.workspace {
			t.Errorf("a channel of another server is in the list: %+v", view.Channel)
		}
	}
}

func TestDeleteRemovesTheChannel(t *testing.T) {
	w := newWorld(t)

	if err := w.svc.Delete(t.Context(), w.general.ID, w.admin); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := w.svc.Get(t.Context(), w.general.ID, w.admin); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// A demoted admin loses channel rights at once: the role is read per call.
func TestRoleChangeAppliesImmediately(t *testing.T) {
	w := newWorld(t)
	roles := w.svc.roles.(*fakeRoles)

	if _, err := w.svc.Create(t.Context(), w.workspace, w.admin, NewChannel{Name: "задачи"}); err != nil {
		t.Fatalf("Create as admin: %v", err)
	}

	roles.roles[w.workspace][w.admin] = servers.RoleMember

	if _, err := w.svc.Create(t.Context(), w.workspace, w.admin, NewChannel{Name: "ещё"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("the demoted admin can still create channels: %v", err)
	}
}
