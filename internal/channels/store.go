package channels

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ruslancode23/orbit-server/internal/perms"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
)

// uniqueViolation is PostgreSQL's SQLSTATE for a broken unique constraint.
const uniqueViolation = "23505"

// Pool is the part of pgxpool.Pool this store needs: queries plus transactions,
// because reordering writes every channel of a server at once.
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

func (s *PostgresStore) Create(ctx context.Context, c NewChannel) (Channel, error) {
	row, err := s.q.CreateChannel(ctx, db.CreateChannelParams{
		WorkspaceID: pgUUID(c.WorkspaceID),
		Kind:        db.ChannelKind(c.Kind),
		Name:        text(c.Name),
		Topic:       text(c.Topic),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return Channel{}, ErrNameTaken
		}
		return Channel{}, fmt.Errorf("create channel: %w", err)
	}
	return toChannel(row, nil), nil
}

func (s *PostgresStore) Channel(ctx context.Context, id uuid.UUID) (Channel, error) {
	row, err := s.q.GetChannel(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Channel{}, ErrNotFound
		}
		return Channel{}, fmt.Errorf("get channel: %w", err)
	}
	rows, err := s.q.ListChannelOverrides(ctx, row.ID)
	if err != nil {
		return Channel{}, fmt.Errorf("list channel overrides: %w", err)
	}
	overrides := make([]override, 0, len(rows))
	for _, o := range rows {
		overrides = append(overrides, override{roleID: o.RoleID, allow: o.Allow, deny: o.Deny, everyone: o.IsDefault})
	}
	return toChannel(row, overrides), nil
}

func (s *PostgresStore) Channels(ctx context.Context, ids []uuid.UUID) ([]Channel, error) {
	keys := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, pgUUID(id))
	}
	rows, err := s.q.GetChannelsByIDs(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("get channels by ids: %w", err)
	}
	out := make([]Channel, 0, len(rows))
	for _, row := range rows {
		out = append(out, toChannel(row, nil))
	}
	return out, nil
}

// List reads the channels of a server and the overrides of all of them in two
// queries, never one per channel.
func (s *PostgresStore) List(ctx context.Context, workspaceID uuid.UUID) ([]Channel, error) {
	rows, err := s.q.ListWorkspaceChannels(ctx, pgUUID(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	overrideRows, err := s.q.ListWorkspaceOverrides(ctx, pgUUID(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("list channel overrides: %w", err)
	}
	byChannel := map[uuid.UUID][]override{}
	for _, o := range overrideRows {
		id := uuid.UUID(o.ChannelID.Bytes)
		byChannel[id] = append(byChannel[id], override{roleID: o.RoleID, allow: o.Allow, deny: o.Deny, everyone: o.IsDefault})
	}
	out := make([]Channel, 0, len(rows))
	for _, row := range rows {
		out = append(out, toChannel(row, byChannel[uuid.UUID(row.ID.Bytes)]))
	}
	return out, nil
}

// SetReadOnly turns «only those who may manage write here» on or off: an
// @everyone override that denies writing and speaking. Other bits of that
// override stay as they are.
func (s *PostgresStore) SetReadOnly(ctx context.Context, channel Channel, readOnly bool) error {
	everyone, err := s.q.GetDefaultRole(ctx, pgUUID(channel.WorkspaceID))
	if err != nil {
		return fmt.Errorf("get @everyone: %w", err)
	}
	current, err := s.q.GetChannelOverride(ctx, db.GetChannelOverrideParams{ChannelID: pgUUID(channel.ID), RoleID: everyone.ID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("get @everyone override: %w", err)
	}
	quiet := int64(perms.SendMessages | perms.Speak)
	allow, deny := current.Allow&^quiet, current.Deny&^quiet
	if readOnly {
		deny |= quiet
	}
	if allow == 0 && deny == 0 {
		if err := s.q.DeleteChannelOverride(ctx, db.DeleteChannelOverrideParams{ChannelID: pgUUID(channel.ID), RoleID: everyone.ID}); err != nil {
			return fmt.Errorf("delete @everyone override: %w", err)
		}
		return nil
	}
	if err := s.q.SetChannelOverride(ctx, db.SetChannelOverrideParams{
		ChannelID: pgUUID(channel.ID), RoleID: everyone.ID, Allow: allow, Deny: deny,
	}); err != nil {
		return fmt.Errorf("set @everyone override: %w", err)
	}
	return nil
}

func (s *PostgresStore) Update(ctx context.Context, id uuid.UUID, u Update) (Channel, error) {
	params := db.UpdateChannelParams{ID: pgUUID(id)}
	if u.Name != nil {
		params.Name = text(*u.Name)
	}
	if u.Topic != nil {
		// An empty topic clears it; the query needs the flag because a NULL
		// parameter means "leave this field alone".
		params.ClearTopic = *u.Topic == ""
		params.Topic = text(*u.Topic)
	}

	row, err := s.q.UpdateChannel(ctx, params)
	if err != nil {
		var pgErr *pgconn.PgError
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return Channel{}, ErrNotFound
		case errors.As(err, &pgErr) && pgErr.Code == uniqueViolation:
			return Channel{}, ErrNameTaken
		}
		return Channel{}, fmt.Errorf("update channel: %w", err)
	}
	return s.Channel(ctx, uuid.UUID(row.ID.Bytes))
}

func (s *PostgresStore) Delete(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteChannel(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	return nil
}

// Reorder writes every position in one transaction: a half-applied order would
// show the channels in a sequence nobody asked for.
func (s *PostgresStore) Reorder(ctx context.Context, workspaceID uuid.UUID, ids []uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)
	for position, id := range ids {
		affected, err := q.SetChannelPosition(ctx, db.SetChannelPositionParams{
			Position:    int32(position), //nolint:gosec // a list index, far below int32
			ID:          pgUUID(id),
			WorkspaceID: pgUUID(workspaceID),
		})
		if err != nil {
			return fmt.Errorf("set channel position: %w", err)
		}
		if affected == 0 {
			// The channel vanished or belongs to another server: nothing is written.
			return ErrIncompleteOrder
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// override is one stored override with whether its role is @everyone.
type override struct {
	roleID      pgtype.UUID
	allow, deny int64
	everyone    bool
}

func (s *PostgresStore) SetOverride(ctx context.Context, channelID uuid.UUID, o perms.Override) error {
	if o.Allow == 0 && o.Deny == 0 {
		if err := s.q.DeleteChannelOverride(ctx, db.DeleteChannelOverrideParams{ChannelID: pgUUID(channelID), RoleID: pgUUID(o.RoleID)}); err != nil {
			return fmt.Errorf("delete channel override: %w", err)
		}
		return nil
	}
	if err := s.q.SetChannelOverride(ctx, db.SetChannelOverrideParams{
		ChannelID: pgUUID(channelID),
		RoleID:    pgUUID(o.RoleID),
		Allow:     int64(o.Allow), //nolint:gosec // perms.Set fits in 13 bits
		Deny:      int64(o.Deny),  //nolint:gosec // perms.Set fits in 13 bits
	}); err != nil {
		return fmt.Errorf("set channel override: %w", err)
	}
	return nil
}

func toChannel(row db.Channel, overrides []override) Channel {
	channel := Channel{
		ID:          uuid.UUID(row.ID.Bytes),
		WorkspaceID: uuid.UUID(row.WorkspaceID.Bytes),
		Kind:        Kind(row.Kind),
		Name:        row.Name.String,
		Topic:       row.Topic.String,
		Position:    int(row.Position),
		CreatedAt:   row.CreatedAt.Time,
	}
	for _, o := range overrides {
		deny := perms.Set(o.deny) //nolint:gosec // stored from a perms.Set
		channel.Overrides = append(channel.Overrides, perms.Override{
			RoleID: uuid.UUID(o.roleID.Bytes),
			Allow:  perms.Set(o.allow), //nolint:gosec // stored from a perms.Set
			Deny:   deny,
		})
		// The old «only admins write» is @everyone not writing here.
		if o.everyone && deny.Has(perms.SendMessages) {
			channel.IsReadOnly = true
		}
	}
	return channel
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func text(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
