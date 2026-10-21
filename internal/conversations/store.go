package conversations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/profile"
)

// Pool is the part of pgxpool.Pool this store needs: queries plus a
// transaction, because a conversation and its members are written together.
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

// Create writes the conversation and everyone in it at once: a conversation
// without its people would be unreachable.
func (s *PostgresStore) Create(ctx context.Context, kind Kind, directKey string, memberIDs []uuid.UUID) (Conversation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Conversation{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)
	row, err := q.CreateConversation(ctx, db.CreateConversationParams{
		Kind:      db.ChannelKind(kind),
		DirectKey: text(directKey),
	})
	if err != nil {
		return Conversation{}, fmt.Errorf("create conversation: %w", err)
	}
	for _, id := range memberIDs {
		if err := q.AddChannelMember(ctx, db.AddChannelMemberParams{
			ChannelID: row.ID,
			UserID:    pgUUID(id),
		}); err != nil {
			return Conversation{}, fmt.Errorf("add conversation member: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Conversation{}, fmt.Errorf("commit: %w", err)
	}

	return s.Conversation(ctx, uuid.UUID(row.ID.Bytes))
}

func (s *PostgresStore) Direct(ctx context.Context, directKey string) (Conversation, error) {
	row, err := s.q.GetDirectConversation(ctx, text(directKey))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Conversation{}, ErrNotFound
		}
		return Conversation{}, fmt.Errorf("get direct conversation: %w", err)
	}
	return s.Conversation(ctx, uuid.UUID(row.ID.Bytes))
}

// Secret finds the secret chat of a pair of devices. It is a separate lookup
// because the direct key belongs to ordinary dialogues only.
func (s *PostgresStore) Secret(ctx context.Context, key string) (Conversation, error) {
	row, err := s.q.GetSecretConversation(ctx, text(key))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Conversation{}, ErrNotFound
		}
		return Conversation{}, fmt.Errorf("get secret conversation: %w", err)
	}
	return s.Conversation(ctx, uuid.UUID(row.ID.Bytes))
}

// MemberIDs is who belongs to a conversation.
func (s *PostgresStore) MemberIDs(ctx context.Context, channelID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.ListConversationMemberIDs(ctx, pgUUID(channelID))
	if err != nil {
		return nil, fmt.Errorf("list conversation members: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		out = append(out, uuid.UUID(row.Bytes))
	}
	return out, nil
}

func (s *PostgresStore) Conversation(ctx context.Context, id uuid.UUID) (Conversation, error) {
	row, err := s.q.GetChannel(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Conversation{}, ErrNotFound
		}
		return Conversation{}, fmt.Errorf("get conversation: %w", err)
	}
	if row.WorkspaceID.Valid {
		// A channel of a server is not a conversation.
		return Conversation{}, ErrNotFound
	}

	members, err := s.members(ctx, id)
	if err != nil {
		return Conversation{}, err
	}
	return Conversation{
		ID:        id,
		Kind:      Kind(row.Kind),
		Name:      row.Name.String,
		Members:   members,
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

// List is one page of the caller's sidebar. The summaries come from one query
// and the participants of the whole page from another; no conversation costs
// its own query.
func (s *PostgresStore) List(ctx context.Context, userID uuid.UUID, before *Cursor, limit int) ([]Conversation, error) {
	params := db.ListConversationSummariesParams{
		UserID:   pgUUID(userID),
		PageSize: int32(limit), //nolint:gosec // capped at maxPageSize+1 by the service
	}
	if before != nil {
		params.CursorSeq = pgtype.Int8{Int64: before.Seq, Valid: true}
		params.CursorCreatedAt = pgtype.Timestamptz{Time: before.CreatedAt, Valid: true}
		params.CursorID = pgUUID(before.ID)
	}
	rows, err := s.q.ListConversationSummaries(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list conversation summaries: %w", err)
	}
	if len(rows) == 0 {
		return []Conversation{}, nil
	}

	ids := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	byChannel, err := s.membersForChannels(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make([]Conversation, 0, len(rows))
	for _, row := range rows {
		id := uuid.UUID(row.ID.Bytes)
		conversation := Conversation{
			ID:          id,
			Kind:        Kind(row.Kind),
			Name:        row.Name.String,
			Members:     byChannel[id],
			CreatedAt:   row.CreatedAt.Time,
			LastReadSeq: row.LastReadSeq,
			Unread:      int(row.Unread),
			Mentions:    int(row.Mentions),
		}
		if row.LastMessageID.Valid {
			last := &LastMessage{
				ID:        uuid.UUID(row.LastMessageID.Bytes),
				Seq:       row.LastMessageSeq,
				Content:   row.LastMessageContent,
				Kind:      MessageKind(row.LastMessageKind),
				CreatedAt: row.LastMessageCreatedAt.Time,
				DeletedAt: timePtr(row.LastMessageDeletedAt),
			}
			if row.LastMessageAuthorID.Valid {
				last.Author = &profile.Public{ID: uuid.UUID(row.LastMessageAuthorID.Bytes)}
			}
			if row.LastMessageSystemType != "" {
				last.System = &SystemMessage{
					Type:     SystemType(row.LastMessageSystemType),
					TargetID: uuidPtr(row.LastMessageSystemTargetID),
				}
				if row.LastMessageSystemName != "" {
					last.System.Name = row.LastMessageSystemName
				}
			}
			conversation.LastMessage = last
		}
		out = append(out, conversation)
	}
	return out, nil
}

// Unread sums the unread counters over every visible conversation in one query.
func (s *PostgresStore) Unread(ctx context.Context, userID uuid.UUID) (UnreadSummary, error) {
	row, err := s.q.CountConversationUnread(ctx, pgUUID(userID))
	if err != nil {
		return UnreadSummary{}, fmt.Errorf("count conversation unread: %w", err)
	}
	return UnreadSummary{Unread: int(row.Unread), Mentions: int(row.Mentions)}, nil
}

func (s *PostgresStore) IsParticipant(ctx context.Context, channelID, userID uuid.UUID) (bool, error) {
	inside, err := s.q.IsChannelMember(ctx, db.IsChannelMemberParams{
		ChannelID: pgUUID(channelID),
		UserID:    pgUUID(userID),
	})
	if err != nil {
		return false, fmt.Errorf("is channel member: %w", err)
	}
	return inside, nil
}

func (s *PostgresStore) KnownUsers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	pgIDs := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		pgIDs = append(pgIDs, pgUUID(id))
	}
	rows, err := s.q.ListKnownUsers(ctx, pgIDs)
	if err != nil {
		return nil, fmt.Errorf("list known users: %w", err)
	}
	known := make(map[uuid.UUID]bool, len(rows))
	for _, row := range rows {
		known[uuid.UUID(row.Bytes)] = true
	}
	return known, nil
}

// Rename sets a group title. The service has already checked that this is a
// group the caller belongs to.
func (s *PostgresStore) Rename(ctx context.Context, channelID uuid.UUID, name string) error {
	if err := s.q.RenameConversation(ctx, db.RenameConversationParams{
		ID:   pgUUID(channelID),
		Name: pgtype.Text{String: name, Valid: true},
	}); err != nil {
		return fmt.Errorf("rename conversation: %w", err)
	}
	return nil
}

// AddMembers locks the conversation row, so two additions cannot both pass the
// same maxMembers count, then inserts only the people who are new.
func (s *PostgresStore) AddMembers(ctx context.Context, channelID uuid.UUID, userIDs []uuid.UUID, maxMembers int) ([]uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if _, err := q.LockConversation(ctx, pgUUID(channelID)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("lock conversation: %w", err)
	}

	candidates := make([]pgtype.UUID, 0, len(userIDs))
	for _, id := range userIDs {
		candidates = append(candidates, pgUUID(id))
	}
	existingRows, err := q.ListExistingConversationMembers(ctx, db.ListExistingConversationMembersParams{
		ChannelID: pgUUID(channelID),
		UserIds:   candidates,
	})
	if err != nil {
		return nil, fmt.Errorf("list existing conversation members: %w", err)
	}
	existing := make(map[uuid.UUID]bool, len(existingRows))
	for _, row := range existingRows {
		existing[uuid.UUID(row.Bytes)] = true
	}
	added := make([]uuid.UUID, 0, len(userIDs))
	addedPG := make([]pgtype.UUID, 0, len(userIDs))
	for _, id := range userIDs {
		if existing[id] {
			continue
		}
		added = append(added, id)
		addedPG = append(addedPG, pgUUID(id))
	}
	if len(added) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		return added, nil
	}

	count, err := q.CountChannelMembers(ctx, pgUUID(channelID))
	if err != nil {
		return nil, fmt.Errorf("count conversation members: %w", err)
	}
	if count+int64(len(added)) > int64(maxMembers) {
		return nil, ErrTooManyMembers
	}

	addedRows, err := q.AddConversationMembers(ctx, db.AddConversationMembersParams{
		ChannelID: pgUUID(channelID),
		UserIds:   addedPG,
	})
	if err != nil {
		return nil, fmt.Errorf("add conversation members: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// The query skips conflicts; normally every candidate is new, but a racing
	// insert can still take one away, so report what was really written.
	out := make([]uuid.UUID, 0, len(addedRows))
	for _, row := range addedRows {
		out = append(out, uuid.UUID(row.Bytes))
	}
	return out, nil
}

func (s *PostgresStore) RemoveMember(ctx context.Context, channelID, userID uuid.UUID) (bool, error) {
	removed, err := s.q.RemoveConversationMember(ctx, db.RemoveConversationMemberParams{
		ChannelID: pgUUID(channelID),
		UserID:    pgUUID(userID),
	})
	if err != nil {
		return false, fmt.Errorf("remove conversation member: %w", err)
	}
	return removed > 0, nil
}

func (s *PostgresStore) Hide(ctx context.Context, channelID, userID uuid.UUID) error {
	changed, err := s.q.HideConversation(ctx, db.HideConversationParams{
		ChannelID: pgUUID(channelID),
		UserID:    pgUUID(userID),
	})
	if err != nil {
		return fmt.Errorf("hide conversation: %w", err)
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Unhide(ctx context.Context, channelID, userID uuid.UUID) error {
	changed, err := s.q.UnhideConversation(ctx, db.UnhideConversationParams{
		ChannelID: pgUUID(channelID),
		UserID:    pgUUID(userID),
	})
	if err != nil {
		return fmt.Errorf("unhide conversation: %w", err)
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) members(ctx context.Context, channelID uuid.UUID) ([]Member, error) {
	rows, err := s.q.ListConversationMembers(ctx, pgUUID(channelID))
	if err != nil {
		return nil, fmt.Errorf("list conversation members: %w", err)
	}
	out := make([]Member, 0, len(rows))
	for _, row := range rows {
		out = append(out, Member{
			// Only the id is known here: Service fills the public part in one batch.
			Public:   profile.Public{ID: uuid.UUID(row.UserID.Bytes)},
			JoinedAt: row.JoinedAt.Time,
		})
	}
	return out, nil
}

// membersForChannels loads the participants of a whole page in one query.
func (s *PostgresStore) membersForChannels(ctx context.Context, channelIDs []pgtype.UUID) (map[uuid.UUID][]Member, error) {
	rows, err := s.q.ListConversationMembersForChannels(ctx, channelIDs)
	if err != nil {
		return nil, fmt.Errorf("list conversation members of a page: %w", err)
	}
	out := make(map[uuid.UUID][]Member, len(channelIDs))
	for _, row := range rows {
		channelID := uuid.UUID(row.ChannelID.Bytes)
		out[channelID] = append(out[channelID], Member{
			Public:   profile.Public{ID: uuid.UUID(row.UserID.Bytes)},
			JoinedAt: row.JoinedAt.Time,
		})
	}
	return out, nil
}

func (s *PostgresStore) SharesConversation(ctx context.Context, a, b uuid.UUID) (bool, error) {
	shared, err := s.q.HasSharedConversation(ctx, db.HasSharedConversationParams{
		A: pgUUID(a),
		B: pgUUID(b),
	})
	if err != nil {
		return false, fmt.Errorf("has shared conversation: %w", err)
	}
	return shared, nil
}

func (s *PostgresStore) Peers(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.ListConversationPeers(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list conversation peers: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		out = append(out, uuid.UUID(row.Bytes))
	}
	return out, nil
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func uuidPtr(id pgtype.UUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	value := uuid.UUID(id.Bytes)
	return &value
}

func timePtr(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	t := value.Time
	return &t
}

func text(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
