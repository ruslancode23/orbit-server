package channels

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/servers"
)

// A server may let members create channels; the rest of channel management
// stays with admins whatever the flag says.
func TestMembersCreateChannelsWhenTheServerAllows(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.Create(t.Context(), w.workspace, w.member, NewChannel{Name: "мой"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("flag off: err = %v, want ErrForbidden", err)
	}

	w.roles.perms = map[uuid.UUID]servers.Permissions{
		w.workspace: {CreateChannels: true, AttachFiles: true},
	}
	created, err := w.svc.Create(t.Context(), w.workspace, w.member, NewChannel{Name: "мой"})
	if err != nil {
		t.Fatalf("flag on: %v", err)
	}
	if !created.CanWrite {
		t.Error("the member cannot write in the channel they made")
	}

	// A read-only channel is a moderation tool: not for members to make.
	if _, err := w.svc.Create(t.Context(), w.workspace, w.member, NewChannel{Name: "объявления", IsReadOnly: true}); !errors.Is(err, ErrForbidden) {
		t.Errorf("read-only by a member: err = %v, want ErrForbidden", err)
	}
	// Creating is all the flag opens.
	name := "чужое имя"
	if _, err := w.svc.Update(t.Context(), w.general.ID, w.member, Update{Name: &name}); !errors.Is(err, ErrForbidden) {
		t.Errorf("rename: err = %v, want ErrForbidden", err)
	}
	if err := w.svc.Delete(t.Context(), created.Channel.ID, w.member); !errors.Is(err, ErrForbidden) {
		t.Errorf("delete own channel: err = %v, want ErrForbidden", err)
	}
	// Admins keep every right with the flag off again.
	w.roles.perms = map[uuid.UUID]servers.Permissions{w.workspace: {}}
	if _, err := w.svc.Create(t.Context(), w.workspace, w.admin, NewChannel{Name: "админский", IsReadOnly: true}); err != nil {
		t.Errorf("admin with every flag off: %v", err)
	}
	if _, err := w.svc.Create(t.Context(), w.workspace, w.member, NewChannel{Name: "ещё"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("flag off again: err = %v, want ErrForbidden", err)
	}
}

// An open server does not open another one.
func TestCreateChannelsFlagIsPerServer(t *testing.T) {
	w := newWorld(t)
	w.roles.roles[w.other][w.member] = servers.RoleMember
	w.roles.perms = map[uuid.UUID]servers.Permissions{
		w.other:     {CreateChannels: true},
		w.workspace: servers.DefaultPermissions(),
	}

	if _, err := w.svc.Create(t.Context(), w.other, w.member, NewChannel{Name: "там можно"}); err != nil {
		t.Errorf("open server: %v", err)
	}
	if _, err := w.svc.Create(t.Context(), w.workspace, w.member, NewChannel{Name: "тут нельзя"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("strict server: err = %v, want ErrForbidden", err)
	}
	if _, err := w.svc.Create(t.Context(), w.other, w.admin, NewChannel{Name: "не мой сервер"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("admin of another server: err = %v, want ErrNotFound", err)
	}
}
