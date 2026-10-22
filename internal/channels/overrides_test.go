package channels

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/perms"
)

// Overrides take manageChannels in the channel, change channel permissions
// only, never both ways at once, and change what members see at once.
func TestSetOverrideRules(t *testing.T) {
	store := newFakeStore()
	server := staffServer{workspace: uuid.New(), everyone: uuid.New(), staff: uuid.New()}
	member, moderator, owner := uuid.New(), uuid.New(), uuid.New()
	server.members = map[uuid.UUID][]uuid.UUID{member: nil, moderator: {server.staff}, owner: nil}
	server.owner = owner
	svc := NewService(store, server)
	channel, err := store.Create(t.Context(), NewChannel{WorkspaceID: server.workspace, Kind: KindText, Name: "общий"})
	if err != nil {
		t.Fatal(err)
	}

	hide := perms.Override{RoleID: server.everyone, Deny: perms.ViewChannel}
	if _, err := svc.SetOverride(t.Context(), channel.ID, member, hide); !errors.Is(err, ErrForbidden) {
		t.Errorf("member SetOverride: err = %v, want ErrForbidden", err)
	}
	if _, err := svc.SetOverride(t.Context(), channel.ID, moderator,
		perms.Override{RoleID: server.everyone, Allow: perms.BanMembers}); !errors.Is(err, ErrInvalidOverride) {
		t.Errorf("a server permission in a channel: err = %v, want ErrInvalidOverride", err)
	}
	if _, err := svc.SetOverride(t.Context(), channel.ID, moderator,
		perms.Override{RoleID: server.everyone, Allow: perms.SendMessages, Deny: perms.SendMessages}); !errors.Is(err, ErrInvalidOverride) {
		t.Errorf("allow and deny at once: err = %v, want ErrInvalidOverride", err)
	}
	if _, err := svc.SetOverride(t.Context(), channel.ID, moderator,
		perms.Override{RoleID: uuid.New(), Deny: perms.ViewChannel}); !errors.Is(err, ErrUnknownRole) {
		t.Errorf("unknown role: err = %v, want ErrUnknownRole", err)
	}

	// The staff may hide the channel from @everyone but keep it for themselves.
	if _, err := svc.SetOverride(t.Context(), channel.ID, moderator,
		perms.Override{RoleID: server.staff, Allow: perms.ViewChannel}); !errors.Is(err, ErrForbidden) {
		t.Errorf("an override for the caller's own role: err = %v, want ErrForbidden", err)
	}
	overrides, err := svc.SetOverride(t.Context(), channel.ID, moderator, hide)
	if err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if len(overrides) != 1 {
		t.Errorf("overrides = %+v, want one", overrides)
	}
	if _, err := svc.Get(t.Context(), channel.ID, member); !errors.Is(err, ErrNotFound) {
		t.Errorf("member sees a hidden channel: err = %v", err)
	}
	// As in Discord, managing channels does not mean seeing them: the staff
	// role never allowed viewing here, so the moderator lost it too.
	if _, err := svc.Get(t.Context(), channel.ID, moderator); !errors.Is(err, ErrNotFound) {
		t.Errorf("moderator still sees the channel they hid: err = %v", err)
	}

	// The owner sees everything and removes the override; it shows again.
	if _, err := svc.SetOverride(t.Context(), channel.ID, owner, perms.Override{RoleID: server.everyone}); err != nil {
		t.Fatalf("remove override: %v", err)
	}
	if _, err := svc.Get(t.Context(), channel.ID, member); err != nil {
		t.Errorf("member after removal: %v", err)
	}
}
