package secrets

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
)

// Pool is the part of pgxpool.Pool this store needs: queries plus a
// transaction, because revoking a device touches several tables at once.
type Pool interface {
	db.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

type PostgresStore struct {
	pool Pool
	q    *db.Queries
}

func NewPostgresStore(pool Pool) *PostgresStore {
	return &PostgresStore{pool: pool, q: db.New(pool)}
}

var _ Store = (*PostgresStore)(nil)

func (s *PostgresStore) CreateDevice(ctx context.Context, userID uuid.UUID, name, identityKey, signingKey string) (Device, error) {
	row, err := s.q.CreateDevice(ctx, db.CreateDeviceParams{
		UserID:      pgUUID(userID),
		Name:        name,
		IdentityKey: identityKey,
		SigningKey:  signingKey,
	})
	if err != nil {
		return Device{}, fmt.Errorf("create device: %w", err)
	}
	return toDevice(row), nil
}

func (s *PostgresStore) DeviceByKeys(ctx context.Context, userID uuid.UUID, identityKey, signingKey string) (Device, error) {
	row, err := s.q.GetUserDeviceByKeys(ctx, db.GetUserDeviceByKeysParams{
		UserID:      pgUUID(userID),
		IdentityKey: identityKey,
		SigningKey:  signingKey,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Device{}, ErrNotFound
		}
		return Device{}, fmt.Errorf("get device by keys: %w", err)
	}
	return toDevice(row), nil
}

func (s *PostgresStore) Device(ctx context.Context, id uuid.UUID) (Device, error) {
	row, err := s.q.GetDevice(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Device{}, ErrNotFound
		}
		return Device{}, fmt.Errorf("get device: %w", err)
	}
	return toDevice(row), nil
}

func (s *PostgresStore) ListDevices(ctx context.Context, userID uuid.UUID) ([]Device, error) {
	rows, err := s.q.ListUserDevices(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	out := make([]Device, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDevice(row))
	}
	return out, nil
}

func (s *PostgresStore) CountDevices(ctx context.Context, userID uuid.UUID) (int, error) {
	count, err := s.q.CountUserDevices(ctx, pgUUID(userID))
	if err != nil {
		return 0, fmt.Errorf("count devices: %w", err)
	}
	return int(count), nil
}

// RevokeDevice forgets the device and everything the server held for it: the
// pending one-time keys, the envelopes in both directions and the bindings of
// its secret chats. A chat that loses a device is deleted, because a secret chat
// is a pair and half of it is nobody.
func (s *PostgresStore) RevokeDevice(ctx context.Context, id, userID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// Which chats this device was in, before the bindings go.
	chatIDs, err := q.DeviceSecretChatIDs(ctx, pgUUID(id))
	if err != nil {
		return fmt.Errorf("list device chats: %w", err)
	}
	revoked, err := q.RevokeDevice(ctx, db.RevokeDeviceParams{ID: pgUUID(id), UserID: pgUUID(userID)})
	if err != nil {
		return fmt.Errorf("revoke device: %w", err)
	}
	if revoked == 0 {
		return ErrNotFound
	}
	if err := q.DeleteDeviceEnvelopes(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("delete device envelopes: %w", err)
	}
	if err := q.DeleteDeviceSecretChatBindings(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("delete device bindings: %w", err)
	}
	for _, chatID := range chatIDs {
		if err := q.DeleteSecretChatChannel(ctx, chatID); err != nil {
			return fmt.Errorf("delete secret chat: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (s *PostgresStore) User(ctx context.Context, id uuid.UUID) (User, error) {
	row, err := s.q.GetUser(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return User{ID: uuid.UUID(row.ID.Bytes), Nickname: row.Nickname, IsGuest: row.IsGuest}, nil
}

func (s *PostgresStore) AddOneTimeKeys(ctx context.Context, deviceID uuid.UUID, keys []OneTimeKey) error {
	for _, key := range keys {
		if err := s.q.AddOneTimeKey(ctx, db.AddOneTimeKeyParams{
			DeviceID: pgUUID(deviceID),
			KeyID:    key.KeyID,
			Key:      key.Key,
		}); err != nil {
			return fmt.Errorf("add one-time key: %w", err)
		}
	}
	return nil
}

func (s *PostgresStore) ClaimOneTimeKey(ctx context.Context, deviceID uuid.UUID) (OneTimeKey, error) {
	row, err := s.q.ClaimOneTimeKey(ctx, pgUUID(deviceID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return OneTimeKey{}, ErrNotFound
		}
		return OneTimeKey{}, fmt.Errorf("claim one-time key: %w", err)
	}
	return OneTimeKey{KeyID: row.KeyID, Key: row.Key}, nil
}

func (s *PostgresStore) CountOneTimeKeys(ctx context.Context, deviceID uuid.UUID) (int, error) {
	count, err := s.q.CountOneTimeKeys(ctx, pgUUID(deviceID))
	if err != nil {
		return 0, fmt.Errorf("count one-time keys: %w", err)
	}
	return int(count), nil
}

func (s *PostgresStore) AddSecretChatDevice(ctx context.Context, chatID, userID, deviceID uuid.UUID) error {
	if err := s.q.AddSecretChatDevice(ctx, db.AddSecretChatDeviceParams{
		ChannelID: pgUUID(chatID),
		UserID:    pgUUID(userID),
		DeviceID:  pgUUID(deviceID),
	}); err != nil {
		return fmt.Errorf("add secret chat device: %w", err)
	}
	return nil
}

func (s *PostgresStore) SecretChatDevice(ctx context.Context, chatID, deviceID uuid.UUID) (bool, error) {
	inside, err := s.q.IsSecretChatDevice(ctx, db.IsSecretChatDeviceParams{
		ChannelID: pgUUID(chatID),
		DeviceID:  pgUUID(deviceID),
	})
	if err != nil {
		return false, fmt.Errorf("is secret chat device: %w", err)
	}
	return inside, nil
}

func (s *PostgresStore) SecretChat(ctx context.Context, chatID, deviceID, userID uuid.UUID) (SecretChat, error) {
	row, err := s.q.GetUserSecretChat(ctx, db.GetUserSecretChatParams{
		ID:       pgUUID(chatID),
		DeviceID: pgUUID(deviceID),
		UserID:   pgUUID(userID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SecretChat{}, ErrNotFound
		}
		return SecretChat{}, fmt.Errorf("get secret chat: %w", err)
	}
	return toSecretChat(row.ChannelID, row.ChannelCreatedAt, row.PeerUserID, row.PeerNickname,
		row.PeerIsGuest, row.PeerDeviceID, row.PeerIdentityKey, row.PeerSigningKey), nil
}

func (s *PostgresStore) SecretChats(ctx context.Context, deviceID, userID uuid.UUID) ([]SecretChat, error) {
	rows, err := s.q.ListUserSecretChats(ctx, db.ListUserSecretChatsParams{
		DeviceID: pgUUID(deviceID),
		UserID:   pgUUID(userID),
	})
	if err != nil {
		return nil, fmt.Errorf("list secret chats: %w", err)
	}
	out := make([]SecretChat, 0, len(rows))
	for _, row := range rows {
		out = append(out, toSecretChat(row.ChannelID, row.ChannelCreatedAt, row.PeerUserID, row.PeerNickname,
			row.PeerIsGuest, row.PeerDeviceID, row.PeerIdentityKey, row.PeerSigningKey))
	}
	return out, nil
}

func (s *PostgresStore) CreateEnvelope(ctx context.Context, envelope Envelope) (Envelope, error) {
	row, err := s.q.CreateSecretEnvelope(ctx, db.CreateSecretEnvelopeParams{
		ChannelID:         pgUUID(envelope.ChatID),
		SenderDeviceID:    pgUUID(envelope.SenderDeviceID),
		RecipientDeviceID: pgUUID(envelope.RecipientDeviceID),
		Type:              int16(envelope.Type), //nolint:gosec // checked to be 0 or 1
		Body:              envelope.Body,
		ExpiresAt:         pgtype.Timestamptz{Time: envelope.ExpiresAt, Valid: true},
	})
	if err != nil {
		return Envelope{}, fmt.Errorf("create envelope: %w", err)
	}
	return toEnvelope(row), nil
}

func (s *PostgresStore) Envelopes(ctx context.Context, chatID, recipientDeviceID uuid.UUID, limit int) ([]Envelope, error) {
	rows, err := s.q.ListPendingSecretEnvelopes(ctx, db.ListPendingSecretEnvelopesParams{
		ChannelID:         pgUUID(chatID),
		RecipientDeviceID: pgUUID(recipientDeviceID),
		Limit:             int32(limit), //nolint:gosec // a fixed page size
	})
	if err != nil {
		return nil, fmt.Errorf("list envelopes: %w", err)
	}
	out := make([]Envelope, 0, len(rows))
	for _, row := range rows {
		out = append(out, toEnvelope(row))
	}
	return out, nil
}

func (s *PostgresStore) Envelope(ctx context.Context, id uuid.UUID) (Envelope, error) {
	row, err := s.q.GetSecretEnvelope(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Envelope{}, ErrNotFound
		}
		return Envelope{}, fmt.Errorf("get envelope: %w", err)
	}
	return toEnvelope(row), nil
}

func (s *PostgresStore) DeleteEnvelope(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteSecretEnvelope(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("delete envelope: %w", err)
	}
	return nil
}

func (s *PostgresStore) DeleteExpiredEnvelopes(ctx context.Context) (int64, error) {
	count, err := s.q.DeleteExpiredSecretEnvelopes(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete expired envelopes: %w", err)
	}
	return count, nil
}

func toDevice(row db.Device) Device {
	return Device{
		ID:          uuid.UUID(row.ID.Bytes),
		UserID:      uuid.UUID(row.UserID.Bytes),
		Name:        row.Name,
		IdentityKey: row.IdentityKey,
		SigningKey:  row.SigningKey,
		CreatedAt:   row.CreatedAt.Time,
	}
}

func toSecretChat(channelID pgtype.UUID, createdAt pgtype.Timestamptz, peerUserID pgtype.UUID, nickname string,
	isGuest bool, peerDeviceID pgtype.UUID, identityKey, signingKey string,
) SecretChat {
	return SecretChat{
		ID: uuid.UUID(channelID.Bytes),
		Peer: Peer{
			UserID:      uuid.UUID(peerUserID.Bytes),
			Nickname:    nickname,
			IsGuest:     isGuest,
			DeviceID:    uuid.UUID(peerDeviceID.Bytes),
			IdentityKey: identityKey,
			SigningKey:  signingKey,
		},
		CreatedAt: createdAt.Time,
	}
}

func toEnvelope(row db.SecretEnvelope) Envelope {
	return Envelope{
		ID:                uuid.UUID(row.ID.Bytes),
		ChatID:            uuid.UUID(row.ChannelID.Bytes),
		SenderDeviceID:    uuid.UUID(row.SenderDeviceID.Bytes),
		RecipientDeviceID: uuid.UUID(row.RecipientDeviceID.Bytes),
		Type:              int(row.Type),
		Body:              row.Body,
		CreatedAt:         row.CreatedAt.Time,
		ExpiresAt:         row.ExpiresAt.Time,
	}
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
