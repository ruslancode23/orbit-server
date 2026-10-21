package conversations

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ruslancode23/orbit-server/internal/platform/postgres/pgtest"
)

// These tests run the conversation SQL against a real PostgreSQL: the list
// order and its cursor, the unread and mention counts, hiding and the group
// title constraint. The fakes in the other tests cannot check any of that.

type pgFixture struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	store *PostgresStore
}

func newPGFixture(t *testing.T) *pgFixture {
	t.Helper()
	pool := pgtest.New(t)
	return &pgFixture{t: t, ctx: context.Background(), pool: pool, store: NewPostgresStore(pool)}
}

func (f *pgFixture) user(nickname string) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO users (nickname, is_guest) VALUES ($1, true) RETURNING id`, nickname,
	).Scan(&id); err != nil {
		f.t.Fatalf("insert user: %v", err)
	}
	return id
}

func (f *pgFixture) conversation(kind Kind, key string, members ...uuid.UUID) uuid.UUID {
	f.t.Helper()
	created, err := f.store.Create(f.ctx, kind, key, members)
	if err != nil {
		f.t.Fatalf("create %s: %v", kind, err)
	}
	return created.ID
}

// message writes one ordinary message and returns its seq and id.
func (f *pgFixture) message(channelID, authorID uuid.UUID, content string) (int64, uuid.UUID) {
	f.t.Helper()
	var (
		seq int64
		id  uuid.UUID
	)
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO messages (channel_id, author_id, client_message_id, content)
		 VALUES ($1, $2, $3, $4) RETURNING seq, id`,
		channelID, authorID, uuid.NewString(), content,
	).Scan(&seq, &id); err != nil {
		f.t.Fatalf("insert message: %v", err)
	}
	return seq, id
}

func (f *pgFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *pgFixture) read(channelID, userID uuid.UUID, seq int64) {
	f.t.Helper()
	f.exec(`INSERT INTO channel_reads (channel_id, user_id, last_read_seq) VALUES ($1, $2, $3)
	        ON CONFLICT (channel_id, user_id) DO UPDATE SET last_read_seq = excluded.last_read_seq`,
		channelID, userID, seq)
}

func (f *pgFixture) list(userID uuid.UUID) []Conversation {
	f.t.Helper()
	list, err := f.store.List(f.ctx, userID, nil, maxPageSize)
	if err != nil {
		f.t.Fatalf("list: %v", err)
	}
	return list
}

func (f *pgFixture) unread(userID uuid.UUID) UnreadSummary {
	f.t.Helper()
	summary, err := f.store.Unread(f.ctx, userID)
	if err != nil {
		f.t.Fatalf("unread: %v", err)
	}
	return summary
}

func ids(list []Conversation) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(list))
	for _, c := range list {
		out = append(out, c.ID)
	}
	return out
}

func sameIDs(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPostgresListSummaries(t *testing.T) {
	t.Parallel()
	f := newPGFixture(t)
	me, anna, boris := f.user("me"), f.user("anna"), f.user("boris")

	direct := f.conversation(KindDirect, "me:anna", me, anna)
	group := f.conversation(KindGroup, "", me, anna, boris)
	empty := f.conversation(KindDirect, "me:boris", me, boris)
	secret := f.conversation(KindSecret, "secret:me:anna", me, anna)
	strangers := f.conversation(KindDirect, "anna:boris", anna, boris)

	f.message(direct, anna, "привет")
	_, mentionID := f.message(direct, anna, "@me смотри")
	f.message(direct, me, "моё не считается")
	_, deletedID := f.message(direct, anna, "удалено")
	f.exec(`UPDATE messages SET deleted_at = now() WHERE id = $1`, deletedID)
	f.exec(`INSERT INTO message_mentions (message_id, user_id) VALUES ($1, $2)`, mentionID, me)

	readSeq, _ := f.message(group, boris, "прочитано")
	f.read(group, me, readSeq)
	long := strings.Repeat("я", 200)
	f.message(group, boris, long)
	f.message(secret, anna, "в списке быть не должно")
	f.message(strangers, anna, "чужая беседа")

	list := f.list(me)
	if want := []uuid.UUID{group, direct, empty}; !sameIDs(ids(list), want) {
		t.Fatalf("order = %v, want group, direct, empty %v", ids(list), want)
	}

	byID := map[uuid.UUID]Conversation{}
	for _, c := range list {
		byID[c.ID] = c
	}

	d := byID[direct]
	// The deleted line is the newest, so it is the preview, but it is not unread.
	if d.Unread != 2 || d.Mentions != 1 {
		t.Errorf("direct unread/mentions = %d/%d, want 2/1", d.Unread, d.Mentions)
	}
	if d.LastMessage == nil || d.LastMessage.DeletedAt == nil {
		t.Errorf("direct last message = %+v, want the deleted one", d.LastMessage)
	}
	if len(d.Members) != 2 {
		t.Errorf("direct members = %d, want 2", len(d.Members))
	}

	g := byID[group]
	if g.Unread != 1 || g.Mentions != 0 || g.LastReadSeq != readSeq {
		t.Errorf("group unread/mentions/read = %d/%d/%d, want 1/0/%d", g.Unread, g.Mentions, g.LastReadSeq, readSeq)
	}
	if g.LastMessage == nil || len([]rune(g.LastMessage.Content)) != 120 {
		t.Errorf("group preview should be cut to 120 characters, got %+v", g.LastMessage)
	}
	if g.LastMessage != nil && (g.LastMessage.Author == nil || g.LastMessage.Author.ID != boris) {
		t.Errorf("group preview author = %+v, want boris", g.LastMessage.Author)
	}
	if len(g.Members) != 3 {
		t.Errorf("group members = %d, want 3", len(g.Members))
	}

	e := byID[empty]
	if e.LastMessage != nil || e.Unread != 0 {
		t.Errorf("empty dialogue = %+v, want no preview and nothing unread", e)
	}

	if got := f.unread(me); got != (UnreadSummary{Unread: 3, Mentions: 1}) {
		t.Errorf("unread badge = %+v, want 3 unread, 1 mention", got)
	}
}

func TestPostgresListSystemPreview(t *testing.T) {
	t.Parallel()
	f := newPGFixture(t)
	me, anna := f.user("me"), f.user("anna")
	group := f.conversation(KindGroup, "", me, anna)

	f.exec(`INSERT INTO messages (channel_id, author_id, client_message_id, content, kind, system_type, system_name)
	        VALUES ($1, $2, $3, 'Анна переименовала беседу', 'system', 'conversation_renamed', 'Планы')`,
		group, anna, uuid.NewString())

	list := f.list(me)
	if len(list) != 1 || list[0].LastMessage == nil {
		t.Fatalf("list = %+v, want one group with a preview", list)
	}
	last := list[0].LastMessage
	if last.Kind != MessageKindSystem || last.System == nil ||
		last.System.Type != SystemConversationRenamed || last.System.Name != "Планы" {
		t.Errorf("system preview = %+v / %+v", last, last.System)
	}
}

// Conversations without messages share last_seq 0, so only created_at and id
// tell them apart: walking the pages with the API's own cursor must visit each
// exactly once and in the order of the first page.
func TestPostgresListPagination(t *testing.T) {
	t.Parallel()
	f := newPGFixture(t)
	me := f.user("me")

	var all []uuid.UUID
	for i := range 7 {
		other := f.user("peer" + string(rune('a'+i)))
		id := f.conversation(KindDirect, "me:"+other.String(), me, other)
		all = append(all, id)
		if i%3 == 0 {
			f.message(id, other, "есть сообщение")
		}
	}
	// Two rows created in the same statement get the same created_at, so the
	// id must break the tie.
	f.exec(`UPDATE channels SET created_at = '2026-01-01T00:00:00Z' WHERE id = ANY($1)`, all[1:3])

	whole, err := f.store.List(f.ctx, me, nil, maxPageSize)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(whole) != len(all) {
		t.Fatalf("whole list = %d rows, want %d", len(whole), len(all))
	}

	var walked []uuid.UUID
	var cursor *Cursor
	for range len(all) {
		page, err := f.store.List(f.ctx, me, cursor, 2)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		if len(page) == 0 {
			break
		}
		walked = append(walked, ids(page)...)
		cursor, err = decodeCursor(encodeCursor(page[len(page)-1]))
		if err != nil {
			t.Fatalf("cursor round trip: %v", err)
		}
	}
	if !sameIDs(walked, ids(whole)) {
		t.Errorf("paged walk = %v\nwant       %v", walked, ids(whole))
	}
}

func TestPostgresHideUntilNewMessage(t *testing.T) {
	t.Parallel()
	f := newPGFixture(t)
	me, anna := f.user("me"), f.user("anna")
	direct := f.conversation(KindDirect, "me:anna", me, anna)
	empty := f.conversation(KindGroup, "", me, anna)
	f.message(direct, anna, "до скрытия")

	if err := f.store.Hide(f.ctx, direct, me); err != nil {
		t.Fatalf("hide: %v", err)
	}
	if err := f.store.Hide(f.ctx, empty, me); err != nil {
		t.Fatalf("hide empty: %v", err)
	}
	if got := f.list(me); len(got) != 0 {
		t.Fatalf("after hiding, list = %v, want empty", ids(got))
	}
	if got := f.unread(me); got != (UnreadSummary{}) {
		t.Errorf("after hiding, badge = %+v, want zero", got)
	}
	// The other member still sees both.
	if got := f.list(anna); len(got) != 2 {
		t.Errorf("anna's list = %d rows, want 2", len(got))
	}

	f.message(direct, anna, "новое")
	if got := ids(f.list(me)); !sameIDs(got, []uuid.UUID{direct}) {
		t.Fatalf("a new message should bring the dialogue back, list = %v", got)
	}
	if got := f.unread(me); got.Unread != 2 {
		t.Errorf("badge after return = %+v, want both unread messages", got)
	}

	if err := f.store.Unhide(f.ctx, empty, me); err != nil {
		t.Fatalf("unhide: %v", err)
	}
	if got := f.list(me); len(got) != 2 {
		t.Errorf("after unhide, list = %d rows, want 2", len(got))
	}
}

func TestPostgresRenameGroupUpToLimit(t *testing.T) {
	t.Parallel()
	f := newPGFixture(t)
	me, anna := f.user("me"), f.user("anna")
	group := f.conversation(KindGroup, "", me, anna)
	direct := f.conversation(KindDirect, "me:anna", me, anna)

	name := strings.Repeat("ы", maxNameLength)
	if err := f.store.Rename(f.ctx, group, name); err != nil {
		t.Fatalf("rename to %d characters: %v", maxNameLength, err)
	}
	got, err := f.store.Conversation(f.ctx, group)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != name {
		t.Errorf("name = %q, want the full title", got.Name)
	}

	// The query only touches groups, so a dialogue keeps no name.
	if err := f.store.Rename(f.ctx, direct, "нельзя"); err != nil {
		t.Fatalf("rename direct: %v", err)
	}
	if got, _ := f.store.Conversation(f.ctx, direct); got.Name != "" {
		t.Errorf("direct name = %q, want none", got.Name)
	}
}

func TestPostgresAddMembersLimit(t *testing.T) {
	t.Parallel()
	f := newPGFixture(t)
	me, anna, boris, vera := f.user("me"), f.user("anna"), f.user("boris"), f.user("vera")
	group := f.conversation(KindGroup, "", me, anna)

	added, err := f.store.AddMembers(f.ctx, group, []uuid.UUID{anna, boris}, 3)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !sameIDs(added, []uuid.UUID{boris}) {
		t.Errorf("added = %v, want only boris", added)
	}
	if _, err := f.store.AddMembers(f.ctx, group, []uuid.UUID{vera}, 3); !errors.Is(err, ErrTooManyMembers) {
		t.Errorf("over the limit err = %v, want ErrTooManyMembers", err)
	}
	removed, err := f.store.RemoveMember(f.ctx, group, anna)
	if err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if got := f.list(anna); len(got) != 0 {
		t.Errorf("a member who left still lists %v", ids(got))
	}
}
