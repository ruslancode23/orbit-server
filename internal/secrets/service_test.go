package secrets

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/profile"
)

type binding struct {
	userID   uuid.UUID
	deviceID uuid.UUID
}

// fakeStore keeps devices, keys, chats and envelopes in memory with the rules
// the database keeps: one device per account per chat, keys claimed once.
type fakeStore struct {
	devices   map[uuid.UUID]Device
	byKeys    map[string]uuid.UUID
	users     map[uuid.UUID]User
	keys      map[uuid.UUID][]OneTimeKey
	bindings  map[uuid.UUID]map[uuid.UUID]binding
	created   map[uuid.UUID]time.Time
	envelopes map[uuid.UUID]Envelope
	creates   int
	revokes   int
	// now is the store clock; tests move it to check the expiry sweep without
	// sleeping.
	now func() time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		devices:   map[uuid.UUID]Device{},
		byKeys:    map[string]uuid.UUID{},
		users:     map[uuid.UUID]User{},
		keys:      map[uuid.UUID][]OneTimeKey{},
		bindings:  map[uuid.UUID]map[uuid.UUID]binding{},
		created:   map[uuid.UUID]time.Time{},
		envelopes: map[uuid.UUID]Envelope{},
		now:       time.Now,
	}
}

func keysKey(userID uuid.UUID, identityKey, signingKey string) string {
	return userID.String() + "|" + identityKey + "|" + signingKey
}

func (s *fakeStore) CreateDevice(_ context.Context, userID uuid.UUID, name, identityKey, signingKey string) (Device, error) {
	s.creates++
	device := Device{
		ID: uuid.New(), UserID: userID, Name: name,
		IdentityKey: identityKey, SigningKey: signingKey, CreatedAt: time.Now(),
	}
	s.devices[device.ID] = device
	s.byKeys[keysKey(userID, identityKey, signingKey)] = device.ID
	return device, nil
}

func (s *fakeStore) DeviceByKeys(_ context.Context, userID uuid.UUID, identityKey, signingKey string) (Device, error) {
	id, ok := s.byKeys[keysKey(userID, identityKey, signingKey)]
	if !ok {
		return Device{}, ErrNotFound
	}
	return s.devices[id], nil
}

func (s *fakeStore) Device(_ context.Context, id uuid.UUID) (Device, error) {
	device, ok := s.devices[id]
	if !ok {
		return Device{}, ErrNotFound
	}
	return device, nil
}

func (s *fakeStore) ListDevices(_ context.Context, userID uuid.UUID) ([]Device, error) {
	var out []Device
	for _, device := range s.devices {
		if device.UserID == userID {
			out = append(out, device)
		}
	}
	return out, nil
}

func (s *fakeStore) RevokeDevice(_ context.Context, id, userID uuid.UUID) error {
	device, ok := s.devices[id]
	if !ok || device.UserID != userID {
		return ErrNotFound
	}
	s.revokes++
	delete(s.devices, id)
	delete(s.byKeys, keysKey(userID, device.IdentityKey, device.SigningKey))
	delete(s.keys, id)
	for chatID, byDevice := range s.bindings {
		delete(byDevice, id)
		if len(byDevice) == 0 {
			delete(s.bindings, chatID)
			delete(s.created, chatID)
		}
	}
	return nil
}

func (s *fakeStore) CountDevices(_ context.Context, userID uuid.UUID) (int, error) {
	count := 0
	for _, device := range s.devices {
		if device.UserID == userID {
			count++
		}
	}
	return count, nil
}

func (s *fakeStore) User(_ context.Context, id uuid.UUID) (User, error) {
	user, ok := s.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return user, nil
}

func (s *fakeStore) AddOneTimeKeys(_ context.Context, deviceID uuid.UUID, keys []OneTimeKey) error {
	s.keys[deviceID] = append(s.keys[deviceID], keys...)
	return nil
}

func (s *fakeStore) ClaimOneTimeKey(_ context.Context, deviceID uuid.UUID) (OneTimeKey, error) {
	keys := s.keys[deviceID]
	if len(keys) == 0 {
		return OneTimeKey{}, ErrNotFound
	}
	key := keys[0]
	s.keys[deviceID] = keys[1:]
	return key, nil
}

func (s *fakeStore) CountOneTimeKeys(_ context.Context, deviceID uuid.UUID) (int, error) {
	return len(s.keys[deviceID]), nil
}

func (s *fakeStore) AddSecretChatDevice(_ context.Context, chatID, userID, deviceID uuid.UUID) error {
	if s.bindings[chatID] == nil {
		s.bindings[chatID] = map[uuid.UUID]binding{}
	}
	s.bindings[chatID][deviceID] = binding{userID: userID, deviceID: deviceID}
	if _, ok := s.created[chatID]; !ok {
		s.created[chatID] = time.Now()
	}
	return nil
}

func (s *fakeStore) SecretChatDevice(_ context.Context, chatID, deviceID uuid.UUID) (bool, error) {
	_, ok := s.bindings[chatID][deviceID]
	return ok, nil
}

func (s *fakeStore) chatFor(chatID, deviceID, userID uuid.UUID) (SecretChat, error) {
	bindings := s.bindings[chatID]
	mine, ok := bindings[deviceID]
	if !ok || mine.userID != userID {
		return SecretChat{}, ErrNotFound
	}
	for _, other := range bindings {
		if other.deviceID == deviceID {
			continue
		}
		user := s.users[other.userID]
		device := s.devices[other.deviceID]
		return SecretChat{
			ID:        chatID,
			CreatedAt: s.created[chatID],
			Peer: Peer{
				UserID: other.userID, Nickname: user.Nickname, IsGuest: user.IsGuest,
				DeviceID: other.deviceID, IdentityKey: device.IdentityKey, SigningKey: device.SigningKey,
			},
		}, nil
	}
	return SecretChat{}, ErrNotFound
}

func (s *fakeStore) SecretChat(_ context.Context, chatID, deviceID, userID uuid.UUID) (SecretChat, error) {
	return s.chatFor(chatID, deviceID, userID)
}

func (s *fakeStore) SecretChats(_ context.Context, deviceID, userID uuid.UUID) ([]SecretChat, error) {
	var out []SecretChat
	for chatID := range s.bindings {
		chat, err := s.chatFor(chatID, deviceID, userID)
		if err == nil {
			out = append(out, chat)
		}
	}
	return out, nil
}

func (s *fakeStore) LeaveSecretChat(_ context.Context, chatID, deviceID, userID uuid.UUID) error {
	mine, ok := s.bindings[chatID][deviceID]
	if !ok || mine.userID != userID {
		return ErrNotFound
	}
	delete(s.bindings, chatID)
	delete(s.created, chatID)
	for id, envelope := range s.envelopes {
		if envelope.ChatID == chatID {
			delete(s.envelopes, id)
		}
	}
	return nil
}

func (s *fakeStore) CreateEnvelope(_ context.Context, envelope Envelope) (Envelope, error) {
	envelope.ID = uuid.New()
	envelope.CreatedAt = time.Now()
	s.envelopes[envelope.ID] = envelope
	return envelope, nil
}

func (s *fakeStore) Envelopes(_ context.Context, chatID, recipientDeviceID uuid.UUID, limit int) ([]Envelope, error) {
	var out []Envelope
	for _, envelope := range s.envelopes {
		if envelope.ChatID == chatID && envelope.RecipientDeviceID == recipientDeviceID {
			out = append(out, envelope)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (s *fakeStore) Envelope(_ context.Context, id uuid.UUID) (Envelope, error) {
	envelope, ok := s.envelopes[id]
	if !ok {
		return Envelope{}, ErrNotFound
	}
	return envelope, nil
}

func (s *fakeStore) DeleteEnvelope(_ context.Context, id uuid.UUID) error {
	delete(s.envelopes, id)
	return nil
}

func (s *fakeStore) DeleteExpiredEnvelopes(_ context.Context) (int64, error) {
	var removed int64
	for id, envelope := range s.envelopes {
		if !s.now().Before(envelope.ExpiresAt) {
			delete(s.envelopes, id)
			removed++
		}
	}
	return removed, nil
}

// fakeChats hands out one conversation per pair of devices, like the real module.
type fakeChats struct {
	chats   map[string]conversations.Conversation
	creates int
}

func newFakeChats() *fakeChats {
	return &fakeChats{chats: map[string]conversations.Conversation{}}
}

func (c *fakeChats) EnsureSecret(_ context.Context, key string, memberIDs []uuid.UUID) (conversations.Conversation, bool, error) {
	if chat, ok := c.chats[key]; ok {
		return chat, false, nil
	}
	c.creates++
	members := make([]conversations.Member, 0, len(memberIDs))
	for _, id := range memberIDs {
		members = append(members, conversations.Member{Public: profile.Public{ID: id}, JoinedAt: time.Now()})
	}
	chat := conversations.Conversation{
		ID: uuid.New(), Kind: conversations.KindSecret, Members: members, CreatedAt: time.Now(),
	}
	c.chats[key] = chat
	return chat, true, nil
}

type recorder struct {
	events []struct {
		user  uuid.UUID
		event events.Event
	}
}

func (r *recorder) PublishToUser(userID uuid.UUID, event events.Event) {
	r.events = append(r.events, struct {
		user  uuid.UUID
		event events.Event
	}{user: userID, event: event})
}

type world struct {
	svc     *Service
	store   *fakeStore
	chats   *fakeChats
	anna    uuid.UUID
	bob     uuid.UUID
	vika    uuid.UUID
	annaKey ed25519.PrivateKey
	annaPub ed25519.PublicKey
	device  uuid.UUID
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{
		store: newFakeStore(),
		chats: newFakeChats(),
		anna:  uuid.New(), bob: uuid.New(), vika: uuid.New(),
	}
	w.store.users[w.anna] = User{ID: w.anna, Nickname: "Аня"}
	w.store.users[w.bob] = User{ID: w.bob, Nickname: "Боб"}
	w.store.users[w.vika] = User{ID: w.vika, Nickname: "Вика"}

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	w.annaKey, w.annaPub = private, public
	w.svc = NewService(w.store, w.chats, &recorder{})
	device, _, err := w.svc.RegisterDevice(t.Context(), w.anna, Registration{
		Name:        "Chrome",
		IdentityKey: "anna-identity-key",
		SigningKey:  base64.StdEncoding.EncodeToString(public),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private,
			registrationMessage(w.anna, "anna-identity-key", base64.StdEncoding.EncodeToString(public)))),
	})
	if err != nil {
		t.Fatalf("register device: %v", err)
	}
	w.device = device.ID
	return w
}

func (w *world) registerPeer(t *testing.T, userID uuid.UUID) uuid.UUID {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signingKey := base64.StdEncoding.EncodeToString(public)
	identityKey := "identity-" + uuid.NewString()
	device, _, err := w.svc.RegisterDevice(t.Context(), userID, Registration{
		Name:        "Phone",
		IdentityKey: identityKey,
		SigningKey:  signingKey,
		Signature:   base64.StdEncoding.EncodeToString(ed25519.Sign(private, registrationMessage(userID, identityKey, signingKey))),
	})
	if err != nil {
		t.Fatalf("register peer device: %v", err)
	}
	return device.ID
}

func (w *world) startChat(t *testing.T, peerDevice uuid.UUID) SecretChat {
	t.Helper()
	chat, _, err := w.svc.StartSecretChat(t.Context(), w.anna, w.device, w.bob, &peerDevice)
	if err != nil {
		t.Fatalf("StartSecretChat: %v", err)
	}
	return chat
}

func TestRegisterDeviceChecksTheSignature(t *testing.T) {
	w := newWorld(t)

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signingKey := base64.StdEncoding.EncodeToString(public)
	message := registrationMessage(w.bob, "bob-identity", signingKey)

	// A signature over something else is refused.
	if _, _, err := w.svc.RegisterDevice(t.Context(), w.bob, Registration{
		IdentityKey: "bob-identity",
		SigningKey:  signingKey,
		Signature:   base64.StdEncoding.EncodeToString(ed25519.Sign(private, []byte("another message"))),
	}); !errors.Is(err, ErrInvalid) {
		t.Errorf("wrong signature: err = %v, want ErrInvalid", err)
	}

	good := base64.StdEncoding.EncodeToString(ed25519.Sign(private, message))
	device, created, err := w.svc.RegisterDevice(t.Context(), w.bob, Registration{
		IdentityKey: "bob-identity", SigningKey: signingKey, Signature: good,
	})
	if err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	if !created {
		t.Error("first registration was reported as an existing device")
	}

	// The same keys again are the same device, not a second one.
	again, created, err := w.svc.RegisterDevice(t.Context(), w.bob, Registration{
		IdentityKey: "bob-identity", SigningKey: signingKey, Signature: good,
	})
	if err != nil {
		t.Fatalf("RegisterDevice again: %v", err)
	}
	if created || again.ID != device.ID {
		t.Errorf("second registration = %+v created=%v, want the same device", again, created)
	}
}

func TestRegisterDeviceLimit(t *testing.T) {
	w := newWorld(t)
	for range maxDevicesPerUser {
		w.registerPeer(t, w.bob)
	}

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signingKey := base64.StdEncoding.EncodeToString(public)
	_, _, err = w.svc.RegisterDevice(t.Context(), w.bob, Registration{
		IdentityKey: "one-too-many", SigningKey: signingKey,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private,
			registrationMessage(w.bob, "one-too-many", signingKey))),
	})
	if !errors.Is(err, ErrTooManyDevices) {
		t.Errorf("err = %v, want ErrTooManyDevices", err)
	}
}

func TestOneTimeKeyIsHandedOutOnce(t *testing.T) {
	w := newWorld(t)
	peer := w.registerPeer(t, w.bob)

	if _, err := w.svc.UploadOneTimeKeys(t.Context(), w.bob, peer, []OneTimeKey{
		{KeyID: "k1", Key: "key-one"}, {KeyID: "k2", Key: "key-two"},
	}); err != nil {
		t.Fatalf("UploadOneTimeKeys: %v", err)
	}

	_, firstKey, err := w.svc.KeyBundle(t.Context(), peer)
	if err != nil {
		t.Fatalf("first KeyBundle: %v", err)
	}
	_, secondKey, err := w.svc.KeyBundle(t.Context(), peer)
	if err != nil {
		t.Fatalf("second KeyBundle: %v", err)
	}
	if firstKey.KeyID == secondKey.KeyID {
		t.Errorf("the same one-time key was handed out twice: %v", firstKey)
	}
	if _, _, err := w.svc.KeyBundle(t.Context(), peer); !errors.Is(err, ErrNoKeys) {
		t.Errorf("third KeyBundle: err = %v, want ErrNoKeys", err)
	}
}

func TestOneTimeKeyUploadBelongsToTheCaller(t *testing.T) {
	w := newWorld(t)
	peer := w.registerPeer(t, w.bob)

	if _, err := w.svc.UploadOneTimeKeys(t.Context(), w.anna, peer, []OneTimeKey{{KeyID: "k", Key: "k"}}); !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestStartSecretChatNeedsOnePeerDevice(t *testing.T) {
	w := newWorld(t)
	if _, _, err := w.svc.StartSecretChat(t.Context(), w.anna, w.device, w.bob, nil); !errors.Is(err, ErrNoDevices) {
		t.Errorf("no devices: err = %v, want ErrNoDevices", err)
	}

	first := w.registerPeer(t, w.bob)
	second := w.registerPeer(t, w.bob)
	if _, _, err := w.svc.StartSecretChat(t.Context(), w.anna, w.device, w.bob, nil); !errors.Is(err, ErrManyDevices) {
		t.Errorf("several devices without a choice: err = %v, want ErrManyDevices", err)
	}

	chat, created, err := w.svc.StartSecretChat(t.Context(), w.anna, w.device, w.bob, &first)
	if err != nil {
		t.Fatalf("StartSecretChat: %v", err)
	}
	if !created {
		t.Error("a new chat was reported as existing")
	}
	if chat.Peer.DeviceID != first {
		t.Errorf("peer device = %v, want %v", chat.Peer.DeviceID, first)
	}

	// The same pair of devices is the same chat.
	again, created, err := w.svc.StartSecretChat(t.Context(), w.anna, w.device, w.bob, &first)
	if err != nil {
		t.Fatalf("StartSecretChat again: %v", err)
	}
	if created || again.ID != chat.ID {
		t.Errorf("second chat = %+v created=%v, want the same %v", again, created, chat.ID)
	}

	// The other device is a different secret chat: keys live per device.
	other, created, err := w.svc.StartSecretChat(t.Context(), w.anna, w.device, w.bob, &second)
	if err != nil {
		t.Fatalf("StartSecretChat with the second device: %v", err)
	}
	if !created || other.ID == chat.ID {
		t.Error("a second device of the same person reused the first chat")
	}
}

func TestStartSecretChatRefusesSomebodyElsesDevice(t *testing.T) {
	w := newWorld(t)
	peer := w.registerPeer(t, w.bob)

	if _, _, err := w.svc.StartSecretChat(t.Context(), w.bob, w.device, w.anna, &peer); !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
	if _, _, err := w.svc.StartSecretChat(t.Context(), w.anna, w.device, w.anna, &peer); !errors.Is(err, ErrInvalid) {
		t.Errorf("chat with myself: err = %v, want ErrInvalid", err)
	}
}

func TestEnvelopeRoundTripAndAcknowledgement(t *testing.T) {
	w := newWorld(t)
	peerDevice := w.registerPeer(t, w.bob)
	chat := w.startChat(t, peerDevice)

	expiresAt := time.Now().Add(time.Minute)
	envelope, err := w.svc.SendEnvelope(t.Context(), w.anna, w.device, chat.ID, EnvelopeInput{
		RecipientDeviceID: peerDevice, Type: 0, Body: []byte("opaque ciphertext"), ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}

	pending, err := w.svc.PendingEnvelopes(t.Context(), w.bob, peerDevice, chat.ID)
	if err != nil {
		t.Fatalf("PendingEnvelopes: %v", err)
	}
	if len(pending) != 1 || string(pending[0].Body) != "opaque ciphertext" {
		t.Errorf("pending = %+v, want one envelope with its ciphertext", pending)
	}

	// Only the recipient device may delete it.
	if err := w.svc.AcknowledgeEnvelope(t.Context(), w.bob, w.device, envelope.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("ack by another device: err = %v, want ErrForbidden", err)
	}
	if err := w.svc.AcknowledgeEnvelope(t.Context(), w.bob, peerDevice, envelope.ID); err != nil {
		t.Fatalf("AcknowledgeEnvelope: %v", err)
	}
	if err := w.svc.AcknowledgeEnvelope(t.Context(), w.bob, peerDevice, envelope.ID); err != nil {
		t.Errorf("acknowledging twice: err = %v, want nil", err)
	}
}

func TestEnvelopeValidation(t *testing.T) {
	w := newWorld(t)
	peerDevice := w.registerPeer(t, w.bob)
	chat := w.startChat(t, peerDevice)
	future := time.Now().Add(time.Minute)

	send := func(input EnvelopeInput) error {
		_, err := w.svc.SendEnvelope(t.Context(), w.anna, w.device, chat.ID, input)
		return err
	}
	cases := []struct {
		name  string
		input EnvelopeInput
		want  error
	}{
		{"unknown type", EnvelopeInput{RecipientDeviceID: peerDevice, Type: 2, Body: []byte("x"), ExpiresAt: future}, ErrInvalid},
		{"empty body", EnvelopeInput{RecipientDeviceID: peerDevice, Type: 0, ExpiresAt: future}, ErrInvalid},
		{"too large", EnvelopeInput{RecipientDeviceID: peerDevice, Type: 0, Body: make([]byte, maxEnvelopeBytes+1), ExpiresAt: future}, ErrInvalid},
		{"past deadline", EnvelopeInput{RecipientDeviceID: peerDevice, Type: 0, Body: []byte("x"), ExpiresAt: time.Now().Add(-time.Minute)}, ErrInvalid},
		{"beyond a week", EnvelopeInput{RecipientDeviceID: peerDevice, Type: 0, Body: []byte("x"), ExpiresAt: time.Now().Add(MaxEnvelopeTTL + time.Hour)}, ErrInvalid},
		{"somebody else's device", EnvelopeInput{RecipientDeviceID: w.device, Type: 0, Body: []byte("x"), ExpiresAt: future}, ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := send(tc.input); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestExpiredEnvelopesAreSwept(t *testing.T) {
	w := newWorld(t)
	peerDevice := w.registerPeer(t, w.bob)
	chat := w.startChat(t, peerDevice)

	now := time.Now()
	w.svc.now = func() time.Time { return now }
	if _, err := w.svc.SendEnvelope(t.Context(), w.anna, w.device, chat.ID, EnvelopeInput{
		RecipientDeviceID: peerDevice, Type: 0, Body: []byte("x"), ExpiresAt: now.Add(30 * time.Second),
	}); err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}

	removed, err := w.svc.SweepExpired(t.Context())
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 before the deadline", removed)
	}

	// The clock moves past the deadline: the store deletes it.
	w.store.now = func() time.Time { return now.Add(time.Minute) }
	removed, err = w.svc.SweepExpired(t.Context())
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 after the deadline", removed)
	}
}

func TestSecondDeviceIsItsOwnChat(t *testing.T) {
	w := newWorld(t)
	peerDevice := w.registerPeer(t, w.bob)
	chat := w.startChat(t, peerDevice)

	second := w.registerPeer(t, w.anna)
	if _, err := w.svc.Chat(t.Context(), w.anna, second, chat.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another device of the same person: err = %v, want ErrNotFound", err)
	}
	chats, err := w.svc.ListSecretChats(t.Context(), w.anna, second)
	if err != nil {
		t.Fatalf("ListSecretChats: %v", err)
	}
	if len(chats) != 0 {
		t.Errorf("chats = %+v, want none for a device without keys", chats)
	}
}

// The live event carries the API shape: the client reads camelCase fields, and
// a domain struct would arrive with Go names instead.
func TestEnvelopeEventIsWireShaped(t *testing.T) {
	w := newWorld(t)
	peerDevice := w.registerPeer(t, w.bob)
	chat := w.startChat(t, peerDevice)
	recorder := &recorder{}
	w.svc = NewService(w.store, w.chats, recorder)

	envelope, err := w.svc.SendEnvelope(t.Context(), w.anna, w.device, chat.ID, EnvelopeInput{
		RecipientDeviceID: peerDevice, Type: 0, Body: []byte("opaque"), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("published %d events, want one", len(recorder.events))
	}
	payload, ok := recorder.events[0].event.Payload.(api.SecretEnvelope)
	if !ok {
		t.Fatalf("payload type = %T, want api.SecretEnvelope", recorder.events[0].event.Payload)
	}
	if payload.SecretChatId != chat.ID || payload.Id != envelope.ID {
		t.Errorf("payload = %+v, want the chat and envelope ids", payload)
	}
	if payload.Body != "b3BhcXVl" {
		t.Errorf("body = %q, want base64 of the ciphertext", payload.Body)
	}
}

// Deleting a secret chat deletes it for both devices: the server drops the
// binding and the pending envelopes, and both streams hear about it.
func TestLeaveSecretChatDeletesThePair(t *testing.T) {
	w := newWorld(t)
	peerDevice := w.registerPeer(t, w.bob)
	chat := w.startChat(t, peerDevice)
	recorder := &recorder{}
	w.svc = NewService(w.store, w.chats, recorder)

	if _, err := w.svc.SendEnvelope(t.Context(), w.anna, w.device, chat.ID, EnvelopeInput{
		RecipientDeviceID: peerDevice, Type: 0, Body: []byte("x"), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}
	recorder.events = nil
	if err := w.svc.LeaveChat(t.Context(), w.anna, w.device, chat.ID); err != nil {
		t.Fatalf("LeaveChat: %v", err)
	}
	if len(w.store.envelopes) != 0 {
		t.Errorf("envelopes left: %v", w.store.envelopes)
	}
	if _, err := w.svc.Chat(t.Context(), w.bob, peerDevice, chat.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("peer still sees the chat: %v", err)
	}
	if len(recorder.events) != 2 {
		t.Fatalf("published %d events, want one per participant", len(recorder.events))
	}
	for _, published := range recorder.events {
		if published.event.Kind != events.KindSecretChatDeleted {
			t.Errorf("event = %v, want %v", published.event.Kind, events.KindSecretChatDeleted)
		}
	}

	// Only a device in the chat may delete it.
	other := w.registerPeer(t, w.anna)
	if err := w.svc.LeaveChat(t.Context(), w.anna, other, chat.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another device: err = %v, want ErrNotFound", err)
	}
}
