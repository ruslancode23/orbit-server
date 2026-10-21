package secrets

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/ruslancode23/orbit-server/internal/events"
)

// A new or revoked device reaches the account's other devices, so the list in
// the settings follows; registering the same keys again changes nothing.
func TestDeviceChangesReachTheOwnDevices(t *testing.T) {
	w := newWorld(t)
	recorder := &recorder{}
	w.svc = NewService(w.store, w.chats, recorder)

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signing := base64.StdEncoding.EncodeToString(public)
	registration := Registration{
		Name:        "Firefox",
		IdentityKey: "anna-second-identity",
		SigningKey:  signing,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private,
			registrationMessage(w.anna, "anna-second-identity", signing))),
	}
	device, created, err := w.svc.RegisterDevice(t.Context(), w.anna, registration)
	if err != nil || !created {
		t.Fatalf("RegisterDevice: created=%v err=%v", created, err)
	}
	if _, again, err := w.svc.RegisterDevice(t.Context(), w.anna, registration); err != nil || again {
		t.Fatalf("second registration: created=%v err=%v", again, err)
	}
	if err := w.svc.RevokeDevice(t.Context(), w.anna, device.ID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}

	var heard []events.Kind
	for _, item := range recorder.events {
		if item.user != w.anna {
			t.Errorf("%s heard %s", item.user, item.event.Kind)
			continue
		}
		heard = append(heard, item.event.Kind)
	}
	if len(heard) != 2 || heard[0] != events.KindDevicesChanged || heard[1] != events.KindDevicesChanged {
		t.Errorf("anna heard %v, want devices.changed twice (registered, revoked)", heard)
	}
}
