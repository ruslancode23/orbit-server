package files

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

// A server may stop members from attaching files; admins attach anyway.
func TestUploadFollowsTheAttachFlag(t *testing.T) {
	w := newWorld(t)
	workspaceID := w.access.channel.WorkspaceID

	if _, err := w.svc.Upload(t.Context(), w.channel, w.anna, "f.png", bytes.NewReader(pngBytes), nil); err != nil {
		t.Fatalf("default: %v", err)
	}

	w.perms.perms[workspaceID] = servers.Permissions{AttachFiles: false}
	_, err := w.svc.Upload(t.Context(), w.channel, w.anna, "f.png", bytes.NewReader(pngBytes), nil)
	if !errors.Is(err, ErrAttachNotAllowed) || !errors.Is(err, ErrForbidden) {
		t.Errorf("flag off: err = %v, want ErrAttachNotAllowed", err)
	}
	if len(w.storage.objects) != 1 {
		t.Errorf("objects = %d, want the refused file never stored", len(w.storage.objects))
	}

	w.perms.roles[workspaceID][w.anna] = servers.RoleAdmin
	if _, err := w.svc.Upload(t.Context(), w.channel, w.anna, "f.png", bytes.NewReader(pngBytes), nil); err != nil {
		t.Errorf("admin with the flag off: %v", err)
	}
}

// The flag of one server does not reach a channel of another, and a
// conversation belongs to no server: everyone in it may attach.
func TestAttachFlagIsPerServer(t *testing.T) {
	w := newWorld(t)
	workspaceID := w.access.channel.WorkspaceID
	other := uuid.New()
	w.perms.perms[other] = servers.Permissions{AttachFiles: false}
	w.perms.roles[other] = map[uuid.UUID]servers.Role{w.anna: servers.RoleMember}

	if _, err := w.svc.Upload(t.Context(), w.channel, w.anna, "f.png", bytes.NewReader(pngBytes), nil); err != nil {
		t.Errorf("own server unaffected by the other: %v", err)
	}
	if err := w.svc.MayAttach(t.Context(), channels.Channel{ID: uuid.New(), WorkspaceID: other}, w.anna); !errors.Is(err, ErrAttachNotAllowed) {
		t.Errorf("other server: err = %v, want ErrAttachNotAllowed", err)
	}
	if err := w.svc.MayAttach(t.Context(), channels.Channel{ID: uuid.New(), WorkspaceID: uuid.New()}, w.anna); !errors.Is(err, ErrNotFound) {
		t.Errorf("a server the account is not on: err = %v, want ErrNotFound", err)
	}

	w.perms.perms[workspaceID] = servers.Permissions{}
	w.access.channel.WorkspaceID = uuid.Nil // the same channel as a conversation
	if _, err := w.svc.Upload(t.Context(), w.channel, w.anna, "f.png", bytes.NewReader(pngBytes), nil); err != nil {
		t.Errorf("conversation: %v", err)
	}
}

// Unwired permissions mean unknown, and unknown is no.
func TestAttachIsRefusedWithoutPermissions(t *testing.T) {
	svc := NewService(nil, nil, nil, nil, nil, 1, 0)
	if err := svc.MayAttach(t.Context(), channels.Channel{WorkspaceID: uuid.New()}, uuid.New()); !errors.Is(err, ErrAttachNotAllowed) {
		t.Errorf("err = %v, want ErrAttachNotAllowed", err)
	}
}
