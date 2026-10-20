// Package secrets owns secret chats: devices and their public keys, one-time
// prekeys, and the opaque ciphertext envelopes the server forwards without
// reading. The private keys never leave the device; the server cannot decrypt a
// message, and the decrypted history is never uploaded.
package secrets

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/events"
)

var (
	// ErrNotFound covers an unknown device, chat or envelope, and one that
	// belongs to somebody else: the same answer, so nothing is revealed.
	ErrNotFound = errors.New("secrets: not found")
	// ErrForbidden means the caller does not own the device it named.
	ErrForbidden = errors.New("secrets: not allowed")
	// ErrInvalid means a key, a signature or an envelope is malformed.
	ErrInvalid = errors.New("secrets: invalid request")
	// ErrTooManyDevices means this account already has as many devices as one
	// instance allows.
	ErrTooManyDevices = errors.New("secrets: too many devices")
	// ErrTooManyKeys means the pending one-time key supply is full.
	ErrTooManyKeys = errors.New("secrets: too many one-time keys")
	// ErrNoKeys means a session cannot be started: the device has no one-time
	// key left and its owner must upload more.
	ErrNoKeys = errors.New("secrets: the device has no one-time key left")
	// ErrNoDevices means the person has not opened a secret chat on any device.
	ErrNoDevices = errors.New("secrets: this person has no device for secret chats")
	// ErrManyDevices means the caller must name which of the person's devices.
	ErrManyDevices = errors.New("secrets: this person has several devices; name one")
)

const (
	// maxDevicesPerUser keeps one account from flooding a peer's device list.
	maxDevicesPerUser = 10
	// maxPendingOneTimeKeys is how many unclaimed keys one device may hold.
	maxPendingOneTimeKeys = 200
	// maxKeysPerUpload matches the API schema.
	maxKeysPerUpload = 100
	// maxEnvelopeBytes is what one encrypted message may weigh, decoded.
	maxEnvelopeBytes = 64 * 1024
	// MaxEnvelopeTTL is the longest an undelivered envelope lives on the server:
	// the longest self-destruct timer is one week.
	MaxEnvelopeTTL = 7 * 24 * time.Hour
	// maxDeviceNameLength matches the API schema.
	maxDeviceNameLength = 64
	// maxKeyLength caps the base64 keys a client may send.
	maxKeyLength = 128
	// envelopePageLimit is how many pending envelopes one request returns.
	envelopePageLimit = 100
)

// Device is one browser or app of an account, as the API shows it. Only public
// keys are here.
type Device struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Name        string
	IdentityKey string
	SigningKey  string
	CreatedAt   time.Time
}

// OneTimeKey is one pre-key handed out once to start an Olm session.
type OneTimeKey struct {
	KeyID string
	Key   string
}

// Peer is the other device of a secret chat, with the keys needed to verify it.
type Peer struct {
	UserID      uuid.UUID
	Nickname    string
	IsGuest     bool
	DeviceID    uuid.UUID
	IdentityKey string
	SigningKey  string
}

// SecretChat is one chat as this device sees it.
type SecretChat struct {
	ID        uuid.UUID
	Peer      Peer
	CreatedAt time.Time
}

// Envelope is one ciphertext on its way from one device to another.
type Envelope struct {
	ID                uuid.UUID
	ChatID            uuid.UUID
	SenderDeviceID    uuid.UUID
	RecipientDeviceID uuid.UUID
	Type              int
	Body              []byte
	CreatedAt         time.Time
	ExpiresAt         time.Time
}

// Registration is what a device sends to register its public keys.
type Registration struct {
	Name        string
	IdentityKey string
	SigningKey  string
	Signature   string
}

// EnvelopeInput is one encrypted message as the sender hands it over.
type EnvelopeInput struct {
	RecipientDeviceID uuid.UUID
	Type              int
	Body              []byte
	ExpiresAt         time.Time
}

// User is the little this module needs to know about an account.
type User struct {
	ID       uuid.UUID
	Nickname string
	IsGuest  bool
}

// Store is the persistence this package needs.
type Store interface {
	CreateDevice(ctx context.Context, userID uuid.UUID, name, identityKey, signingKey string) (Device, error)
	DeviceByKeys(ctx context.Context, userID uuid.UUID, identityKey, signingKey string) (Device, error)
	Device(ctx context.Context, id uuid.UUID) (Device, error)
	ListDevices(ctx context.Context, userID uuid.UUID) ([]Device, error)
	// RevokeDevice returns ErrNotFound when the device is not the caller's.
	RevokeDevice(ctx context.Context, id, userID uuid.UUID) error
	CountDevices(ctx context.Context, userID uuid.UUID) (int, error)

	User(ctx context.Context, id uuid.UUID) (User, error)

	AddOneTimeKeys(ctx context.Context, deviceID uuid.UUID, keys []OneTimeKey) error
	ClaimOneTimeKey(ctx context.Context, deviceID uuid.UUID) (OneTimeKey, error)
	CountOneTimeKeys(ctx context.Context, deviceID uuid.UUID) (int, error)

	AddSecretChatDevice(ctx context.Context, chatID, userID, deviceID uuid.UUID) error
	SecretChatDevice(ctx context.Context, chatID, deviceID uuid.UUID) (bool, error)
	SecretChat(ctx context.Context, chatID, deviceID, userID uuid.UUID) (SecretChat, error)
	SecretChats(ctx context.Context, deviceID, userID uuid.UUID) ([]SecretChat, error)

	CreateEnvelope(ctx context.Context, envelope Envelope) (Envelope, error)
	Envelopes(ctx context.Context, chatID, recipientDeviceID uuid.UUID, limit int) ([]Envelope, error)
	Envelope(ctx context.Context, id uuid.UUID) (Envelope, error)
	DeleteEnvelope(ctx context.Context, id uuid.UUID) error
	DeleteExpiredEnvelopes(ctx context.Context) (int64, error)
}

// Chats opens the channel a secret chat lives in. internal/conversations owns
// channels, so this package asks it rather than writing the table itself.
type Chats interface {
	EnsureSecret(ctx context.Context, key string, memberIDs []uuid.UUID) (conversations.Conversation, bool, error)
}

// Publisher reaches one account's connections with a personal event.
type Publisher interface {
	PublishToUser(userID uuid.UUID, event events.Event)
}

type Service struct {
	store     Store
	chats     Chats
	publisher Publisher
	// now is the clock; tests replace it to check expiry without sleeping.
	now func() time.Time
}

func NewService(store Store, chats Chats, publisher Publisher) *Service {
	return &Service{store: store, chats: chats, publisher: publisher, now: time.Now}
}

// RegisterDevice stores the public keys of one device after checking that it
// holds the private signing key. The same keys registered twice return the
// device already stored, so a page reload does not multiply devices.
func (s *Service) RegisterDevice(ctx context.Context, userID uuid.UUID, registration Registration) (Device, bool, error) {
	name := strings.TrimSpace(registration.Name)
	if len([]rune(name)) > maxDeviceNameLength {
		return Device{}, false, ErrInvalid
	}
	identityKey := strings.TrimSpace(registration.IdentityKey)
	signingKey := strings.TrimSpace(registration.SigningKey)
	if identityKey == "" || len(identityKey) > maxKeyLength || signingKey == "" || len(signingKey) > maxKeyLength {
		return Device{}, false, ErrInvalid
	}

	key, err := decodeKey(signingKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return Device{}, false, ErrInvalid
	}
	signature, err := decodeKey(registration.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Device{}, false, ErrInvalid
	}
	if !ed25519.Verify(ed25519.PublicKey(key), registrationMessage(userID, identityKey, signingKey), signature) {
		return Device{}, false, ErrInvalid
	}

	existing, err := s.store.DeviceByKeys(ctx, userID, identityKey, signingKey)
	switch {
	case err == nil:
		return existing, false, nil
	case !errors.Is(err, ErrNotFound):
		return Device{}, false, err
	}

	count, err := s.store.CountDevices(ctx, userID)
	if err != nil {
		return Device{}, false, err
	}
	if count >= maxDevicesPerUser {
		return Device{}, false, ErrTooManyDevices
	}

	device, err := s.store.CreateDevice(ctx, userID, name, identityKey, signingKey)
	if err != nil {
		// A parallel registration of the same keys wins the race; return it.
		if existing, lookupErr := s.store.DeviceByKeys(ctx, userID, identityKey, signingKey); lookupErr == nil {
			return existing, false, nil
		}
		return Device{}, false, err
	}
	return device, true, nil
}

// ListDevices is the caller's own devices.
func (s *Service) ListDevices(ctx context.Context, userID uuid.UUID) ([]Device, error) {
	return s.store.ListDevices(ctx, userID)
}

// UserDevices are the public keys of another person's devices.
func (s *Service) UserDevices(ctx context.Context, userID uuid.UUID) ([]Device, error) {
	if _, err := s.store.User(ctx, userID); err != nil {
		return nil, err
	}
	return s.store.ListDevices(ctx, userID)
}

// RevokeDevice forgets one of the caller's devices, its pending keys and its
// undelivered envelopes.
func (s *Service) RevokeDevice(ctx context.Context, userID, deviceID uuid.UUID) error {
	return s.store.RevokeDevice(ctx, deviceID, userID)
}

// UploadOneTimeKeys adds a batch of pre-keys to a device the caller owns.
func (s *Service) UploadOneTimeKeys(ctx context.Context, userID, deviceID uuid.UUID, keys []OneTimeKey) (int, error) {
	if err := s.requireOwnedDevice(ctx, userID, deviceID); err != nil {
		return 0, err
	}
	if len(keys) == 0 || len(keys) > maxKeysPerUpload {
		return 0, ErrInvalid
	}

	pending, err := s.store.CountOneTimeKeys(ctx, deviceID)
	if err != nil {
		return 0, err
	}
	if pending+len(keys) > maxPendingOneTimeKeys {
		return 0, ErrTooManyKeys
	}
	for _, key := range keys {
		if key.KeyID == "" || len(key.KeyID) > 64 || len(key.Key) > maxKeyLength || key.Key == "" {
			return 0, ErrInvalid
		}
	}
	if err := s.store.AddOneTimeKeys(ctx, deviceID, keys); err != nil {
		return 0, err
	}
	return s.store.CountOneTimeKeys(ctx, deviceID)
}

// CountOneTimeKeys is how many pre-keys the caller's device can still hand out.
// A client polls it to refill before a sender is turned away.
func (s *Service) CountOneTimeKeys(ctx context.Context, userID, deviceID uuid.UUID) (int, error) {
	if err := s.requireOwnedDevice(ctx, userID, deviceID); err != nil {
		return 0, err
	}
	return s.store.CountOneTimeKeys(ctx, deviceID)
}

// KeyBundle returns a device's identity keys and one one-time key. The key is
// consumed by this call: two senders never get the same pre-key.
func (s *Service) KeyBundle(ctx context.Context, deviceID uuid.UUID) (Device, OneTimeKey, error) {
	device, err := s.store.Device(ctx, deviceID)
	if err != nil {
		return Device{}, OneTimeKey{}, err
	}
	key, err := s.store.ClaimOneTimeKey(ctx, deviceID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Device{}, OneTimeKey{}, ErrNoKeys
		}
		return Device{}, OneTimeKey{}, err
	}
	return device, key, nil
}

// StartSecretChat opens the chat of two devices, or returns the one that already
// exists. A device pair has exactly one chat.
func (s *Service) StartSecretChat(ctx context.Context, callerID, deviceID, peerUserID uuid.UUID, peerDeviceID *uuid.UUID) (SecretChat, bool, error) {
	if peerUserID == uuid.Nil || peerUserID == callerID {
		return SecretChat{}, false, ErrInvalid
	}
	if err := s.requireOwnedDevice(ctx, callerID, deviceID); err != nil {
		return SecretChat{}, false, err
	}
	if _, err := s.store.User(ctx, peerUserID); err != nil {
		return SecretChat{}, false, err
	}

	peerDevices, err := s.store.ListDevices(ctx, peerUserID)
	if err != nil {
		return SecretChat{}, false, err
	}
	if len(peerDevices) == 0 {
		return SecretChat{}, false, ErrNoDevices
	}
	var peerDevice Device
	switch {
	case peerDeviceID != nil:
		found := false
		for _, candidate := range peerDevices {
			if candidate.ID == *peerDeviceID {
				peerDevice, found = candidate, true
				break
			}
		}
		if !found {
			return SecretChat{}, false, ErrNotFound
		}
	case len(peerDevices) == 1:
		peerDevice = peerDevices[0]
	default:
		return SecretChat{}, false, ErrManyDevices
	}

	key := secretKey(deviceID, peerDevice.ID)
	chat, created, err := s.chats.EnsureSecret(ctx, key, []uuid.UUID{callerID, peerUserID})
	if err != nil {
		return SecretChat{}, false, err
	}
	if err := s.store.AddSecretChatDevice(ctx, chat.ID, callerID, deviceID); err != nil {
		return SecretChat{}, false, err
	}
	if err := s.store.AddSecretChatDevice(ctx, chat.ID, peerUserID, peerDevice.ID); err != nil {
		return SecretChat{}, false, err
	}

	opened, err := s.store.SecretChat(ctx, chat.ID, deviceID, callerID)
	if err != nil {
		return SecretChat{}, false, err
	}
	if created && s.publisher != nil {
		// Both sides refetch their list; the payload names the chat only, so one
		// event fits both recipients.
		payload := map[string]any{"id": chat.ID}
		s.publisher.PublishToUser(callerID, events.Event{
			Kind: events.KindConversationCreated, ChannelID: chat.ID, Payload: payload,
		})
		s.publisher.PublishToUser(peerUserID, events.Event{
			Kind: events.KindConversationCreated, ChannelID: chat.ID, Payload: payload,
		})
	}
	return opened, created, nil
}

// ListSecretChats is the chats this device holds keys for.
func (s *Service) ListSecretChats(ctx context.Context, userID, deviceID uuid.UUID) ([]SecretChat, error) {
	if err := s.requireOwnedDevice(ctx, userID, deviceID); err != nil {
		return nil, err
	}
	return s.store.SecretChats(ctx, deviceID, userID)
}

// SecretChat is one chat this device takes part in.
func (s *Service) Chat(ctx context.Context, userID, deviceID, chatID uuid.UUID) (SecretChat, error) {
	if err := s.requireOwnedDevice(ctx, userID, deviceID); err != nil {
		return SecretChat{}, err
	}
	return s.store.SecretChat(ctx, chatID, deviceID, userID)
}

// SendEnvelope stores one ciphertext and tells the recipient's stream about it.
// The body is never decoded beyond its base64 envelope, logged or measured.
func (s *Service) SendEnvelope(ctx context.Context, userID, deviceID, chatID uuid.UUID, input EnvelopeInput) (Envelope, error) {
	chat, err := s.Chat(ctx, userID, deviceID, chatID)
	if err != nil {
		return Envelope{}, err
	}
	if input.RecipientDeviceID != chat.Peer.DeviceID {
		return Envelope{}, ErrNotFound
	}
	if input.Type != 0 && input.Type != 1 {
		return Envelope{}, ErrInvalid
	}
	if len(input.Body) == 0 || len(input.Body) > maxEnvelopeBytes {
		return Envelope{}, ErrInvalid
	}
	now := s.now()
	if !input.ExpiresAt.After(now.Add(time.Second)) || input.ExpiresAt.After(now.Add(MaxEnvelopeTTL)) {
		return Envelope{}, ErrInvalid
	}

	stored, err := s.store.CreateEnvelope(ctx, Envelope{
		ChatID:            chatID,
		SenderDeviceID:    deviceID,
		RecipientDeviceID: input.RecipientDeviceID,
		Type:              input.Type,
		Body:              input.Body,
		ExpiresAt:         input.ExpiresAt,
	})
	if err != nil {
		return Envelope{}, err
	}

	if s.publisher != nil {
		// The recipient's other connections hear the envelope too; only the
		// device bound to the chat has the keys to open it.
		s.publisher.PublishToUser(chat.Peer.UserID, events.Event{
			Kind:      events.KindSecretEnvelope,
			ChannelID: chatID,
			// The wire shape, not the domain struct: the client reads camelCase.
			Payload: toAPIEnvelope(stored),
		})
	}
	return stored, nil
}

// PendingEnvelopes is what was sent to this device while it was away.
func (s *Service) PendingEnvelopes(ctx context.Context, userID, deviceID, chatID uuid.UUID) ([]Envelope, error) {
	if _, err := s.Chat(ctx, userID, deviceID, chatID); err != nil {
		return nil, err
	}
	return s.store.Envelopes(ctx, chatID, deviceID, envelopePageLimit)
}

// AcknowledgeEnvelope deletes a delivered envelope. Acknowledging one that is
// already gone is fine: the goal is that it is gone.
func (s *Service) AcknowledgeEnvelope(ctx context.Context, userID, deviceID, envelopeID uuid.UUID) error {
	if err := s.requireOwnedDevice(ctx, userID, deviceID); err != nil {
		return err
	}
	envelope, err := s.store.Envelope(ctx, envelopeID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if envelope.RecipientDeviceID != deviceID {
		return ErrNotFound
	}
	return s.store.DeleteEnvelope(ctx, envelopeID)
}

// SweepExpired removes envelopes nobody picked up before their deadline.
func (s *Service) SweepExpired(ctx context.Context) (int64, error) {
	return s.store.DeleteExpiredEnvelopes(ctx)
}

// ownedDevice loads a device and checks it belongs to the caller.
func (s *Service) requireOwnedDevice(ctx context.Context, userID, deviceID uuid.UUID) error {
	device, err := s.store.Device(ctx, deviceID)
	if err != nil {
		return err
	}
	if device.UserID != userID {
		return ErrForbidden
	}
	return nil
}

// secretKey names a pair of devices. Sorting makes the same two devices one key
// whichever side starts the chat.
func secretKey(first, second uuid.UUID) string {
	ids := []string{first.String(), second.String()}
	sort.Strings(ids)
	return "secret:" + strings.Join(ids, ":")
}

// registrationMessage is exactly what a device signs; the account id is in it,
// so a signature cannot be replayed for somebody else.
func registrationMessage(userID uuid.UUID, identityKey, signingKey string) []byte {
	return []byte(fmt.Sprintf("orbit-device-v1|%s|%s|%s", userID, identityKey, signingKey))
}

// decodeKey accepts base64 with or without padding: Olm emits unpadded keys
// while most clients pad them.
func decodeKey(value string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err == nil {
		return decoded, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	return raw, nil
}
