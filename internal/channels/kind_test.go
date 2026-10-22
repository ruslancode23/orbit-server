package channels

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/perms"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

func TestCreateChannelKind(t *testing.T) {
	store := newFakeStore()
	workspace := uuid.New()
	owner := uuid.New()
	roles := &fakeRoles{roles: map[uuid.UUID]map[uuid.UUID]servers.Role{
		workspace: {owner: servers.RoleOwner},
	}}
	svc := NewService(store, roles)

	text, err := svc.Create(t.Context(), workspace, owner, NewChannel{Name: "general"})
	if err != nil {
		t.Fatalf("Create(text) error = %v", err)
	}
	if text.Channel.Kind != KindText {
		t.Errorf("kind of a channel created without one = %q, want %q", text.Channel.Kind, KindText)
	}
	if !text.CanWrite {
		t.Error("the owner must be able to write in a text channel")
	}

	voice, err := svc.Create(t.Context(), workspace, owner, NewChannel{Kind: KindVoice, Name: "room"})
	if err != nil {
		t.Fatalf("Create(voice) error = %v", err)
	}
	if voice.Channel.Kind != KindVoice {
		t.Errorf("kind = %q, want %q", voice.Channel.Kind, KindVoice)
	}
	// A voice channel carries no messages; CanWrite is false even for the owner.
	if voice.CanWrite {
		t.Error("a voice channel must not accept messages")
	}
}

func TestCreateRejectsUnknownKind(t *testing.T) {
	store := newFakeStore()
	workspace := uuid.New()
	owner := uuid.New()
	roles := &fakeRoles{roles: map[uuid.UUID]map[uuid.UUID]servers.Role{
		workspace: {owner: servers.RoleOwner},
	}}
	svc := NewService(store, roles)

	_, err := svc.Create(t.Context(), workspace, owner, NewChannel{Kind: "screen", Name: "room"})
	if !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("Create() error = %v, want ErrInvalidKind", err)
	}
}

func TestVoiceAccess(t *testing.T) {
	store := newFakeStore()
	workspace := uuid.New()
	owner := uuid.New()
	member := uuid.New()
	outsider := uuid.New()
	roles := &fakeRoles{roles: map[uuid.UUID]map[uuid.UUID]servers.Role{
		workspace: {owner: servers.RoleOwner, member: servers.RoleMember},
	}}
	svc := NewService(store, roles)

	voice, err := svc.Create(t.Context(), workspace, owner, NewChannel{Kind: KindVoice, Name: "room"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	canSpeak, err := svc.VoiceAccess(t.Context(), voice.Channel.ID, member)
	if err != nil {
		t.Fatalf("VoiceAccess(member) error = %v", err)
	}
	if !canSpeak {
		t.Error("a member must be able to speak in a normal voice channel")
	}

	text, err := svc.Create(t.Context(), workspace, owner, NewChannel{Name: "general"})
	if err != nil {
		t.Fatalf("Create(text) error = %v", err)
	}
	if _, err := svc.VoiceAccess(t.Context(), text.Channel.ID, member); !errors.Is(err, ErrNotFound) {
		t.Errorf("VoiceAccess(text) error = %v, want ErrNotFound", err)
	}
	if _, err := svc.VoiceAccess(t.Context(), voice.Channel.ID, outsider); !errors.Is(err, ErrNotFound) {
		t.Errorf("VoiceAccess(outsider) error = %v, want ErrNotFound", err)
	}

	if _, err := svc.Update(t.Context(), voice.Channel.ID, owner, Update{IsReadOnly: new(true)}); err != nil {
		t.Fatalf("Update(read-only) error = %v", err)
	}
	canSpeak, err = svc.VoiceAccess(t.Context(), voice.Channel.ID, member)
	if err != nil {
		t.Fatalf("VoiceAccess(read-only member) error = %v", err)
	}
	if canSpeak {
		t.Error("a plain member must not speak in a read-only voice channel")
	}
	canSpeak, err = svc.VoiceAccess(t.Context(), voice.Channel.ID, owner)
	if err != nil {
		t.Fatalf("VoiceAccess(read-only owner) error = %v", err)
	}
	if !canSpeak {
		t.Error("the owner must speak in a read-only voice channel")
	}
}

func TestChannelKindPermissions(t *testing.T) {
	everything := perms.All
	voice := Channel{Kind: KindVoice}
	if canWrite(voice, everything) {
		t.Error("voice channel must not be writable")
	}
	if !canSpeak(voice, everything) {
		t.Error("voice channel must be speakable")
	}
	text := Channel{Kind: KindText}
	if canSpeak(text, everything) {
		t.Error("text channel must not be speakable")
	}
	if canWrite(text, perms.SendMessages) {
		t.Error("writing without seeing the channel must not be possible")
	}
}
