package reads

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

// message is what the fake store counts: who wrote it and whom it mentions.
type message struct {
	channelID uuid.UUID
	seq       int64
	author    uuid.UUID
	mentions  []uuid.UUID
	deleted   bool
}

// fakeStore counts unread the way the SQL does: messages after the cursor,
// the reader's own excluded, deleted ones excluded, mentions counted apart.
type fakeStore struct {
	messages []message
	cursors  map[uuid.UUID]map[uuid.UUID]int64
	// channelsOf maps a server to its channels.
	channelsOf map[uuid.UUID][]uuid.UUID
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		cursors:    map[uuid.UUID]map[uuid.UUID]int64{},
		channelsOf: map[uuid.UUID][]uuid.UUID{},
	}
}

func (s *fakeStore) cursor(channelID, userID uuid.UUID) int64 {
	return s.cursors[channelID][userID]
}

func (s *fakeStore) Mark(ctx context.Context, channelID, userID uuid.UUID, seq int64) (Unread, error) {
	if s.cursors[channelID] == nil {
		s.cursors[channelID] = map[uuid.UUID]int64{}
	}
	// Forward only, like greatest() in the query.
	if seq > s.cursors[channelID][userID] {
		s.cursors[channelID][userID] = seq
	}
	return s.Channel(ctx, channelID, userID)
}

func (s *fakeStore) Channel(_ context.Context, channelID, userID uuid.UUID) (Unread, error) {
	state := Unread{ChannelID: channelID, LastReadSeq: s.cursor(channelID, userID)}
	for _, m := range s.messages {
		if m.channelID != channelID || m.deleted || m.author == userID || m.seq <= state.LastReadSeq {
			continue
		}
		state.Unread++
		for _, mentioned := range m.mentions {
			if mentioned == userID {
				state.Mentions++
			}
		}
	}
	return state, nil
}

func (s *fakeStore) Workspace(ctx context.Context, workspaceID, userID uuid.UUID) ([]Unread, error) {
	out := make([]Unread, 0, len(s.channelsOf[workspaceID]))
	for _, channelID := range s.channelsOf[workspaceID] {
		state, err := s.Channel(ctx, channelID, userID)
		if err != nil {
			return nil, err
		}
		out = append(out, state)
	}
	return out, nil
}

type fakeChannels struct {
	channels map[uuid.UUID]channels.Channel
	readers  map[uuid.UUID]map[uuid.UUID]bool
}

func (c *fakeChannels) Get(_ context.Context, channelID, callerID uuid.UUID) (channels.View, error) {
	channel, ok := c.channels[channelID]
	if !ok || !c.readers[channelID][callerID] {
		return channels.View{}, channels.ErrNotFound
	}
	return channels.View{Channel: channel, CanWrite: true}, nil
}

type fakeMembers struct {
	roles map[uuid.UUID]map[uuid.UUID]servers.Role
}

func (m *fakeMembers) RequireAtLeast(_ context.Context, workspaceID, userID uuid.UUID, _ servers.Role) (servers.Role, error) {
	role, ok := m.roles[workspaceID][userID]
	if !ok {
		return "", servers.ErrNotMember
	}
	return role, nil
}

// recorder keeps what was delivered to whom.
type recorder struct {
	to     []uuid.UUID
	events []events.Event
}

func (r *recorder) PublishTo(userID uuid.UUID, event events.Event) {
	r.to = append(r.to, userID)
	r.events = append(r.events, event)
}

type world struct {
	svc       *Service
	store     *fakeStore
	published *recorder
	workspace uuid.UUID
	general   uuid.UUID
	tasks     uuid.UUID
	anna      uuid.UUID
	bob       uuid.UUID
	outsider  uuid.UUID
}

func newWorld(t *testing.T) world {
	t.Helper()
	w := world{
		store:     newFakeStore(),
		published: &recorder{},
		workspace: uuid.New(), general: uuid.New(), tasks: uuid.New(),
		anna: uuid.New(), bob: uuid.New(), outsider: uuid.New(),
	}
	w.store.channelsOf[w.workspace] = []uuid.UUID{w.general, w.tasks}

	access := &fakeChannels{
		channels: map[uuid.UUID]channels.Channel{
			w.general: {ID: w.general, WorkspaceID: w.workspace, Name: "общий"},
			w.tasks:   {ID: w.tasks, WorkspaceID: w.workspace, Name: "задачи"},
		},
		readers: map[uuid.UUID]map[uuid.UUID]bool{
			w.general: {w.anna: true, w.bob: true},
			w.tasks:   {w.anna: true, w.bob: true},
		},
	}
	members := &fakeMembers{roles: map[uuid.UUID]map[uuid.UUID]servers.Role{
		w.workspace: {w.anna: servers.RoleMember, w.bob: servers.RoleMember},
	}}
	w.svc = NewService(w.store, access, members, w.published)
	return w
}

func TestUnreadCountsOthersMessagesOnly(t *testing.T) {
	w := newWorld(t)
	w.store.messages = []message{
		{channelID: w.general, seq: 1, author: w.bob},
		{channelID: w.general, seq: 2, author: w.anna}, // her own does not count
		{channelID: w.general, seq: 3, author: w.bob, mentions: []uuid.UUID{w.anna}},
		{channelID: w.general, seq: 4, author: w.bob, deleted: true}, // deleted does not count
	}

	state, err := w.svc.Channel(t.Context(), w.general, w.anna)
	if err != nil {
		t.Fatalf("Channel: %v", err)
	}
	if state.Unread != 2 {
		t.Errorf("unread = %d, want 2", state.Unread)
	}
	if state.Mentions != 1 {
		t.Errorf("mentions = %d, want 1", state.Mentions)
	}
}

func TestMarkMovesTheCursorAndClearsTheCount(t *testing.T) {
	w := newWorld(t)
	w.store.messages = []message{
		{channelID: w.general, seq: 1, author: w.bob},
		{channelID: w.general, seq: 2, author: w.bob, mentions: []uuid.UUID{w.anna}},
	}

	state, err := w.svc.Mark(t.Context(), w.general, w.anna, 2)
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if state.LastReadSeq != 2 || state.Unread != 0 || state.Mentions != 0 {
		t.Errorf("state = %+v, want the cursor at 2 and nothing unread", state)
	}
}

// A device coming back from offline must not make messages unread again.
func TestCursorOnlyMovesForward(t *testing.T) {
	w := newWorld(t)
	w.store.messages = []message{
		{channelID: w.general, seq: 1, author: w.bob},
		{channelID: w.general, seq: 2, author: w.bob},
	}
	if _, err := w.svc.Mark(t.Context(), w.general, w.anna, 2); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	state, err := w.svc.Mark(t.Context(), w.general, w.anna, 1)
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if state.LastReadSeq != 2 {
		t.Errorf("lastReadSeq = %d, want it to stay at 2", state.LastReadSeq)
	}
	if state.Unread != 0 {
		t.Errorf("unread = %d, want 0", state.Unread)
	}
}

// The sidebar comes in one request, with a line per channel.
func TestWorkspaceCountsEveryChannelAtOnce(t *testing.T) {
	w := newWorld(t)
	w.store.messages = []message{
		{channelID: w.general, seq: 1, author: w.bob},
		{channelID: w.general, seq: 2, author: w.bob, mentions: []uuid.UUID{w.anna}},
		{channelID: w.tasks, seq: 3, author: w.bob},
	}
	if _, err := w.svc.Mark(t.Context(), w.general, w.anna, 1); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	list, err := w.svc.Workspace(t.Context(), w.workspace, w.anna)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("channels = %d, want 2", len(list))
	}

	byChannel := map[uuid.UUID]Unread{}
	for _, state := range list {
		byChannel[state.ChannelID] = state
	}
	if got := byChannel[w.general]; got.Unread != 1 || got.Mentions != 1 || got.LastReadSeq != 1 {
		t.Errorf("общий = %+v, want 1 unread, 1 mention, cursor 1", got)
	}
	if got := byChannel[w.tasks]; got.Unread != 1 || got.Mentions != 0 || got.LastReadSeq != 0 {
		t.Errorf("задачи = %+v, want 1 unread, no mention, cursor 0", got)
	}
}

// Moving the cursor is told to the other devices of the same account — and to
// nobody else: what someone has read is their business.
func TestCursorChangeGoesOnlyToItsOwner(t *testing.T) {
	w := newWorld(t)
	w.store.messages = []message{{channelID: w.general, seq: 1, author: w.bob}}

	if _, err := w.svc.Mark(t.Context(), w.general, w.anna, 1); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	if len(w.published.to) != 1 || w.published.to[0] != w.anna {
		t.Fatalf("delivered to %v, want only %v", w.published.to, w.anna)
	}
	event := w.published.events[0]
	if event.Kind != events.KindReadChanged {
		t.Errorf("kind = %q, want %q", event.Kind, events.KindReadChanged)
	}
	if event.WorkspaceID != w.workspace || event.ChannelID != w.general || event.Seq != 1 {
		t.Errorf("event = %+v, want it addressed to the channel with seq 1", event)
	}
}

func TestOutsidersSeeNothing(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.Channel(t.Context(), w.general, w.outsider); !errors.Is(err, ErrNotFound) {
		t.Errorf("Channel: err = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.Mark(t.Context(), w.general, w.outsider, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Mark: err = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.Workspace(t.Context(), w.workspace, w.outsider); !errors.Is(err, ErrNotFound) {
		t.Errorf("Workspace: err = %v, want ErrNotFound", err)
	}
	if len(w.published.to) != 0 {
		t.Errorf("an outsider's attempt was announced: %v", w.published.to)
	}
}

func TestNegativeCursorIsTreatedAsTheStart(t *testing.T) {
	w := newWorld(t)
	w.store.messages = []message{{channelID: w.general, seq: 1, author: w.bob}}

	state, err := w.svc.Mark(t.Context(), w.general, w.anna, -5)
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if state.LastReadSeq != 0 || state.Unread != 1 {
		t.Errorf("state = %+v, want the cursor at 0 and one unread", state)
	}
}
