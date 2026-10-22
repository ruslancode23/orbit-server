package messages

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/perms"
)

// fakeStore keeps messages in memory with the rules the database enforces:
// one message per (channel, author, client id), and a growing seq per channel.
type fakeStore struct {
	messages  map[uuid.UUID]*Message
	clientIDs map[string]uuid.UUID
	mentions  map[uuid.UUID][]uuid.UUID
	reactions map[uuid.UUID]map[string]map[uuid.UUID]bool
	seq       int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		messages:  map[uuid.UUID]*Message{},
		clientIDs: map[string]uuid.UUID{},
		mentions:  map[uuid.UUID][]uuid.UUID{},
		reactions: map[uuid.UUID]map[string]map[uuid.UUID]bool{},
	}
}

func (s *fakeStore) key(m NewMessage) string {
	return m.ChannelID.String() + "/" + m.AuthorID.String() + "/" + m.ClientMessageID
}

func (s *fakeStore) Create(_ context.Context, m NewMessage) (Message, bool, error) {
	if id, ok := s.clientIDs[s.key(m)]; ok {
		return *s.messages[id], true, nil
	}
	s.seq++
	message := Message{
		ID: uuid.New(), ChannelID: m.ChannelID, Seq: s.seq,
		Kind:      MessageKindUser,
		Author:    &Author{ID: m.AuthorID, Nickname: "кто-то"},
		Content:   m.Content,
		ReplyToID: m.ReplyToID,
		CreatedAt: time.Now(),
	}
	s.messages[message.ID] = &message
	s.clientIDs[s.key(m)] = message.ID
	return message, false, nil
}

func (s *fakeStore) CreateSystem(_ context.Context, m NewSystemMessage) (Message, error) {
	s.seq++
	message := Message{
		ID: uuid.New(), ChannelID: m.ChannelID, Seq: s.seq,
		Kind:      MessageKindSystem,
		Author:    &Author{ID: m.AuthorID, Nickname: "кто-то"},
		Content:   m.Content,
		System:    &System{Type: m.Type, TargetID: m.TargetID, Name: m.Name},
		CreatedAt: time.Now(),
	}
	s.messages[message.ID] = &message
	return message, nil
}

func (s *fakeStore) Message(_ context.Context, id uuid.UUID) (Message, error) {
	m, ok := s.messages[id]
	if !ok {
		return Message{}, ErrNotFound
	}
	return *m, nil
}

func (s *fakeStore) List(_ context.Context, channelID uuid.UUID, before *int64, limit int, viewerID uuid.UUID) ([]Message, error) {
	var out []Message
	for _, m := range s.messages {
		if m.ChannelID != channelID || (before != nil && m.Seq >= *before) {
			continue
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	if len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i] = s.decorate(out[i], viewerID)
	}
	return out, nil
}

func (s *fakeStore) Since(_ context.Context, channelID uuid.UUID, afterSeq int64, limit int, viewerID uuid.UUID) ([]Message, error) {
	var out []Message
	for _, m := range s.messages {
		if m.ChannelID == channelID && m.Seq > afterSeq {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i] = s.decorate(out[i], viewerID)
	}
	return out, nil
}

func (s *fakeStore) Edit(_ context.Context, id, authorID uuid.UUID, content string) (Message, error) {
	m, ok := s.messages[id]
	if !ok {
		return Message{}, ErrNotFound
	}
	if m.Author == nil || m.Author.ID != authorID || m.DeletedAt != nil || m.Kind == MessageKindSystem {
		return Message{}, ErrForbidden
	}
	now := time.Now()
	m.Content = content
	m.EditedAt = &now
	return *m, nil
}

func (s *fakeStore) Delete(_ context.Context, id, authorID uuid.UUID) (Message, error) {
	m, ok := s.messages[id]
	if !ok {
		return Message{}, ErrNotFound
	}
	if m.Author == nil || m.Author.ID != authorID || m.DeletedAt != nil || m.Kind == MessageKindSystem {
		return Message{}, ErrForbidden
	}
	now := time.Now()
	m.Content = ""
	m.DeletedAt = &now
	return *m, nil
}

func (s *fakeStore) AddMentions(_ context.Context, messageID uuid.UUID, userIDs []uuid.UUID) error {
	s.mentions[messageID] = append(s.mentions[messageID], userIDs...)
	return nil
}

func (s *fakeStore) AddReaction(_ context.Context, messageID, userID uuid.UUID, emoji string) error {
	if s.reactions[messageID] == nil {
		s.reactions[messageID] = map[string]map[uuid.UUID]bool{}
	}
	if s.reactions[messageID][emoji] == nil {
		s.reactions[messageID][emoji] = map[uuid.UUID]bool{}
	}
	s.reactions[messageID][emoji][userID] = true
	return nil
}

func (s *fakeStore) RemoveReaction(_ context.Context, messageID, userID uuid.UUID, emoji string) error {
	delete(s.reactions[messageID][emoji], userID)
	if len(s.reactions[messageID][emoji]) == 0 {
		delete(s.reactions[messageID], emoji)
	}
	return nil
}

func (s *fakeStore) Load(_ context.Context, m Message, viewerID uuid.UUID) (Message, error) {
	stored, ok := s.messages[m.ID]
	if !ok {
		return Message{}, ErrNotFound
	}
	return s.decorate(*stored, viewerID), nil
}

func (s *fakeStore) decorate(m Message, viewerID uuid.UUID) Message {
	m.Mentions = s.mentions[m.ID]
	m.Reactions = nil
	emojis := make([]string, 0, len(s.reactions[m.ID]))
	for emoji := range s.reactions[m.ID] {
		emojis = append(emojis, emoji)
	}
	sort.Strings(emojis)
	for _, emoji := range emojis {
		people := s.reactions[m.ID][emoji]
		m.Reactions = append(m.Reactions, Reaction{
			Emoji: emoji, Count: len(people), Reacted: people[viewerID],
		})
	}
	return m
}

// fakeChannels answers like internal/channels.
type fakeChannels struct {
	channels map[uuid.UUID]channels.Channel
	// readers and writers hold who may read and write each channel.
	readers map[uuid.UUID]map[uuid.UUID]bool
	writers map[uuid.UUID]map[uuid.UUID]bool
	// managers may delete anyone's messages wherever they may read.
	managers map[uuid.UUID]bool
}

func (c *fakeChannels) Get(_ context.Context, channelID, callerID uuid.UUID) (channels.View, error) {
	channel, ok := c.channels[channelID]
	if !ok || !c.readers[channelID][callerID] {
		return channels.View{}, channels.ErrNotFound
	}
	view := channels.View{Channel: channel, CanWrite: c.writers[channelID][callerID], Permissions: perms.ViewChannel}
	if c.managers[callerID] {
		view.Permissions |= perms.ManageMessages
	}
	return view, nil
}

func (c *fakeChannels) MayWrite(ctx context.Context, channelID, callerID uuid.UUID) (channels.Channel, error) {
	view, err := c.Get(ctx, channelID, callerID)
	if err != nil {
		return channels.Channel{}, err
	}
	if !view.CanWrite {
		return channels.Channel{}, channels.ErrForbidden
	}
	return view.Channel, nil
}

type fakeMembers struct {
	members map[uuid.UUID]map[uuid.UUID]bool
}

func (m *fakeMembers) IsMember(_ context.Context, workspaceID, userID uuid.UUID) (bool, error) {
	return m.members[workspaceID][userID], nil
}

// recorder keeps what the service published, so a test can read it back.
type recorder struct {
	published []events.Event
	personal  []uuid.UUID
}

func (r *recorder) Publish(event events.Event) { r.published = append(r.published, event) }

func (r *recorder) PublishToUser(userID uuid.UUID, event events.Event) {
	r.personal = append(r.personal, userID)
	r.published = append(r.published, event)
}

// fakeConversations answers who is in a conversation, so messages can fan out
// to their own streams instead of a server's.
type fakeConversations struct {
	members map[uuid.UUID][]uuid.UUID
	kinds   map[uuid.UUID]conversations.Kind
}

func (c *fakeConversations) MemberIDs(_ context.Context, channelID uuid.UUID) ([]uuid.UUID, error) {
	return c.members[channelID], nil
}

func (c *fakeConversations) Kind(_ context.Context, channelID uuid.UUID) (conversations.Kind, error) {
	if kind, ok := c.kinds[channelID]; ok {
		return kind, nil
	}
	return conversations.KindDirect, nil
}

func (r *recorder) kinds() []events.Kind {
	out := make([]events.Kind, 0, len(r.published))
	for _, e := range r.published {
		out = append(out, e.Kind)
	}
	return out
}

type world struct {
	svc       *Service
	store     *fakeStore
	access    *fakeChannels
	workspace uuid.UUID
	channel   uuid.UUID
	readOnly  uuid.UUID
	direct    uuid.UUID
	anna      uuid.UUID
	bob       uuid.UUID
	admin     uuid.UUID
	outsider  uuid.UUID
	published *recorder
}

func newWorld(t *testing.T) world {
	t.Helper()
	w := world{
		workspace: uuid.New(), channel: uuid.New(), readOnly: uuid.New(), direct: uuid.New(),
		anna: uuid.New(), bob: uuid.New(), admin: uuid.New(), outsider: uuid.New(),
		store: newFakeStore(),
	}
	w.access = &fakeChannels{
		managers: map[uuid.UUID]bool{w.admin: true},
		channels: map[uuid.UUID]channels.Channel{
			w.channel:  {ID: w.channel, WorkspaceID: w.workspace, Name: "общий"},
			w.readOnly: {ID: w.readOnly, WorkspaceID: w.workspace, Name: "объявления", IsReadOnly: true},
			// A conversation is a channel without a server, and it is not secret.
			w.direct: {ID: w.direct, Kind: channels.KindText, Name: "личный"},
		},
		readers: map[uuid.UUID]map[uuid.UUID]bool{
			w.channel:  {w.anna: true, w.bob: true, w.admin: true},
			w.readOnly: {w.anna: true, w.bob: true, w.admin: true},
			w.direct:   {w.anna: true, w.bob: true},
		},
		writers: map[uuid.UUID]map[uuid.UUID]bool{
			w.channel:  {w.anna: true, w.bob: true, w.admin: true},
			w.readOnly: {w.admin: true},
			w.direct:   {w.anna: true, w.bob: true},
		},
	}
	members := &fakeMembers{members: map[uuid.UUID]map[uuid.UUID]bool{
		w.workspace: {w.anna: true, w.bob: true, w.admin: true},
	}}
	w.published = &recorder{}
	w.svc = NewService(w.store, w.access, members, nil, w.published)
	w.svc.SetConversations(&fakeConversations{
		members: map[uuid.UUID][]uuid.UUID{w.direct: {w.anna, w.bob}},
		kinds:   map[uuid.UUID]conversations.Kind{w.direct: conversations.KindDirect},
	})
	return w
}

func send(t *testing.T, w world, author uuid.UUID, clientID, text string) Message {
	t.Helper()
	m, _, err := w.svc.Send(t.Context(), author, NewMessage{
		ChannelID: w.channel, ClientMessageID: clientID, Content: text,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	return m
}

// A resend after a network failure must not leave two messages (FR-04).
func TestResendWithTheSameClientIdReturnsTheStoredMessage(t *testing.T) {
	w := newWorld(t)

	first, existed, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "c-1", Content: "привет",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if existed {
		t.Error("the first send reports the message as already stored")
	}

	second, existed, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "c-1", Content: "привет",
	})
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if !existed {
		t.Error("the resend reports a new message")
	}
	if second.ID != first.ID || second.Seq != first.Seq {
		t.Errorf("resend = %v/%d, want the stored %v/%d", second.ID, second.Seq, first.ID, first.Seq)
	}

	page, err := w.svc.History(t.Context(), w.channel, w.anna, nil, 50)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(page.Messages) != 1 {
		t.Errorf("messages = %d, want 1", len(page.Messages))
	}
}

// Another author with the same client id is a different message.
func TestSameClientIdFromAnotherAuthorIsANewMessage(t *testing.T) {
	w := newWorld(t)
	first := send(t, w, w.anna, "c-1", "от Анны")
	second := send(t, w, w.bob, "c-1", "от Боба")

	if first.ID == second.ID {
		t.Error("two authors with the same client id got one message")
	}
}

func TestSendRejectsEmptyAndTooLongText(t *testing.T) {
	w := newWorld(t)

	for _, tc := range []struct{ name, text string }{
		{"empty", "   "},
		{"too long", strings.Repeat("я", maxContentLength+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
				ChannelID: w.channel, ClientMessageID: "c-x", Content: tc.text,
			})
			if !errors.Is(err, ErrInvalidContent) {
				t.Errorf("err = %v, want ErrInvalidContent", err)
			}
		})
	}
}

// Writing follows the channel: a plain member gets nothing into an announcements channel.
func TestSendRespectsChannelWriteAccess(t *testing.T) {
	w := newWorld(t)

	_, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.readOnly, ClientMessageID: "c-1", Content: "можно?",
	})
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}

	if _, _, err := w.svc.Send(t.Context(), w.admin, NewMessage{
		ChannelID: w.readOnly, ClientMessageID: "c-2", Content: "объявление",
	}); err != nil {
		t.Errorf("the admin cannot write in a read-only channel: %v", err)
	}
}

func TestOutsiderSeesNothing(t *testing.T) {
	w := newWorld(t)
	message := send(t, w, w.anna, "c-1", "привет")

	if _, err := w.svc.History(t.Context(), w.channel, w.outsider, nil, 50); !errors.Is(err, ErrNotFound) {
		t.Errorf("History: err = %v, want ErrNotFound", err)
	}
	if _, _, err := w.svc.Send(t.Context(), w.outsider, NewMessage{
		ChannelID: w.channel, ClientMessageID: "c-2", Content: "я тут",
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Send: err = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.Edit(t.Context(), message.ID, w.outsider, "моё"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Edit: err = %v, want ErrNotFound", err)
	}
}

// History comes newest first, in pages, and the cursor walks the whole channel once.
func TestHistoryPagesByCursor(t *testing.T) {
	w := newWorld(t)
	for i := range 5 {
		send(t, w, w.anna, string(rune('a'+i)), "сообщение")
	}

	first, err := w.svc.History(t.Context(), w.channel, w.anna, nil, 2)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(first.Messages) != 2 || first.NextBefore == nil {
		t.Fatalf("page = %d messages, cursor %v; want 2 and a cursor", len(first.Messages), first.NextBefore)
	}
	if first.Messages[0].Seq <= first.Messages[1].Seq {
		t.Error("the page is not newest first")
	}

	seen := map[int64]bool{}
	cursor := first.NextBefore
	for _, m := range first.Messages {
		seen[m.Seq] = true
	}
	for cursor != nil {
		page, err := w.svc.History(t.Context(), w.channel, w.anna, cursor, 2)
		if err != nil {
			t.Fatalf("History: %v", err)
		}
		for _, m := range page.Messages {
			if seen[m.Seq] {
				t.Errorf("seq %d came twice", m.Seq)
			}
			seen[m.Seq] = true
		}
		cursor = page.NextBefore
	}
	if len(seen) != 5 {
		t.Errorf("walked %d messages, want 5", len(seen))
	}
}

func TestHistoryLimitIsCapped(t *testing.T) {
	w := newWorld(t)
	send(t, w, w.anna, "c-1", "привет")

	for _, limit := range []int{0, -3, maxPageSize + 100} {
		if _, err := w.svc.History(t.Context(), w.channel, w.anna, nil, limit); err != nil {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
}

func TestEditAndDeleteOnlyYourOwn(t *testing.T) {
	w := newWorld(t)
	message := send(t, w, w.anna, "c-1", "привет")

	t.Run("someone else cannot edit", func(t *testing.T) {
		if _, err := w.svc.Edit(t.Context(), message.ID, w.bob, "правлю чужое"); !errors.Is(err, ErrForbidden) {
			t.Errorf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("an admin cannot edit either", func(t *testing.T) {
		if _, err := w.svc.Edit(t.Context(), message.ID, w.admin, "правлю"); !errors.Is(err, ErrForbidden) {
			t.Errorf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("the author edits", func(t *testing.T) {
		edited, err := w.svc.Edit(t.Context(), message.ID, w.anna, "  привет всем  ")
		if err != nil {
			t.Fatalf("Edit: %v", err)
		}
		if edited.Content != "привет всем" {
			t.Errorf("content = %q, want it trimmed", edited.Content)
		}
		if edited.EditedAt == nil {
			t.Error("editedAt is not set")
		}
	})

	t.Run("someone else cannot delete", func(t *testing.T) {
		if _, err := w.svc.Delete(t.Context(), message.ID, w.bob); !errors.Is(err, ErrForbidden) {
			t.Errorf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("the author deletes", func(t *testing.T) {
		deleted, err := w.svc.Delete(t.Context(), message.ID, w.anna)
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if deleted.DeletedAt == nil || deleted.Content != "" {
			t.Errorf("message = %+v, want it deleted and empty", deleted)
		}
	})
}

// A deleted message stays in the history so replies and read cursors keep working.
func TestDeletedMessageStaysInHistory(t *testing.T) {
	w := newWorld(t)
	message := send(t, w, w.anna, "c-1", "привет")
	if _, err := w.svc.Delete(t.Context(), message.ID, w.anna); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	page, err := w.svc.History(t.Context(), w.channel, w.bob, nil, 50)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(page.Messages) != 1 || page.Messages[0].DeletedAt == nil {
		t.Errorf("history = %+v, want one deleted message", page.Messages)
	}
	if page.Messages[0].Content != "" {
		t.Errorf("content = %q, want it empty", page.Messages[0].Content)
	}
}

// manageMessages lets a moderator delete anyone's message in a server channel;
// without it only the author may. A conversation has no moderators.
func TestModeratorDeletesOthersMessages(t *testing.T) {
	w := newWorld(t)
	annas := send(t, w, w.anna, "c-1", "спам")
	if _, err := w.svc.Delete(t.Context(), annas.ID, w.bob); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member deleted another's message: err = %v", err)
	}
	deleted, err := w.svc.Delete(t.Context(), annas.ID, w.admin)
	if err != nil {
		t.Fatalf("moderator Delete: %v", err)
	}
	if deleted.DeletedAt == nil {
		t.Error("the message is not marked deleted")
	}

	direct, _, err := w.svc.Send(t.Context(), w.bob, NewMessage{ChannelID: w.direct, ClientMessageID: "d-1", Content: "лично"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := w.svc.Delete(t.Context(), direct.ID, w.anna); !errors.Is(err, ErrForbidden) {
		t.Errorf("a participant deleted the other's message in a dialogue: err = %v", err)
	}
}

func TestReplyMustBeInTheSameChannel(t *testing.T) {
	w := newWorld(t)
	target := send(t, w, w.anna, "c-1", "вопрос")
	stranger := uuid.New()

	t.Run("same channel", func(t *testing.T) {
		m, _, err := w.svc.Send(t.Context(), w.bob, NewMessage{
			ChannelID: w.channel, ClientMessageID: "c-2", Content: "ответ", ReplyToID: &target.ID,
		})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if m.ReplyToID == nil || *m.ReplyToID != target.ID {
			t.Errorf("replyToId = %v, want %v", m.ReplyToID, target.ID)
		}
	})

	t.Run("unknown message", func(t *testing.T) {
		_, _, err := w.svc.Send(t.Context(), w.bob, NewMessage{
			ChannelID: w.channel, ClientMessageID: "c-3", Content: "ответ", ReplyToID: &stranger,
		})
		if !errors.Is(err, ErrInvalidReply) {
			t.Errorf("err = %v, want ErrInvalidReply", err)
		}
	})
}

// Mentions are stored for the people who can actually read the channel.
func TestMentionsKeepOnlyMembers(t *testing.T) {
	w := newWorld(t)

	message, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "c-1", Content: "@bob @outsider",
		Mentions: []uuid.UUID{w.bob, w.outsider, w.bob},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(message.Mentions) != 1 || message.Mentions[0] != w.bob {
		t.Errorf("mentions = %v, want only %v", message.Mentions, w.bob)
	}
}

func TestReactions(t *testing.T) {
	w := newWorld(t)
	message := send(t, w, w.anna, "c-1", "привет")

	reacted, err := w.svc.React(t.Context(), message.ID, w.bob, "👍")
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if len(reacted.Reactions) != 1 || reacted.Reactions[0].Count != 1 {
		t.Fatalf("reactions = %+v, want one with count 1", reacted.Reactions)
	}
	// The count belongs to everyone; "reacted" is about the caller.
	if !reacted.Reactions[0].Reacted {
		t.Error("the caller who just reacted is not marked")
	}

	asAnna, err := w.svc.React(t.Context(), message.ID, w.anna, "👍")
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if asAnna.Reactions[0].Count != 2 {
		t.Errorf("count = %d, want 2", asAnna.Reactions[0].Count)
	}

	// Reacting twice with the same emoji changes nothing.
	again, err := w.svc.React(t.Context(), message.ID, w.anna, "👍")
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if again.Reactions[0].Count != 2 {
		t.Errorf("count after a repeat = %d, want 2", again.Reactions[0].Count)
	}

	left, err := w.svc.Unreact(t.Context(), message.ID, w.anna, "👍")
	if err != nil {
		t.Fatalf("Unreact: %v", err)
	}
	if left.Reactions[0].Count != 1 || left.Reactions[0].Reacted {
		t.Errorf("reactions = %+v, want count 1 and not reacted by the caller", left.Reactions)
	}
}

// Reacting is writing: a read-only channel takes no reaction from a plain member.
func TestReactionRespectsWriteAccess(t *testing.T) {
	w := newWorld(t)
	announcement, _, err := w.svc.Send(t.Context(), w.admin, NewMessage{
		ChannelID: w.readOnly, ClientMessageID: "c-1", Content: "объявление",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if _, err := w.svc.React(t.Context(), announcement.ID, w.anna, "👍"); !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
	if _, err := w.svc.React(t.Context(), announcement.ID, w.admin, "👍"); err != nil {
		t.Errorf("the admin cannot react in their own announcement: %v", err)
	}
}

func TestReactionRejectsEmptyEmojiAndDeletedMessage(t *testing.T) {
	w := newWorld(t)
	message := send(t, w, w.anna, "c-1", "привет")

	if _, err := w.svc.React(t.Context(), message.ID, w.bob, "  "); !errors.Is(err, ErrInvalidEmoji) {
		t.Errorf("err = %v, want ErrInvalidEmoji", err)
	}

	if _, err := w.svc.Delete(t.Context(), message.ID, w.anna); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := w.svc.React(t.Context(), message.ID, w.bob, "👍"); !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden on a deleted message", err)
	}
}

// Losing the right to write still lets someone take their own reaction back.
func TestUnreactNeedsOnlyReadAccess(t *testing.T) {
	w := newWorld(t)
	message := send(t, w, w.anna, "c-1", "привет")
	if _, err := w.svc.React(t.Context(), message.ID, w.bob, "👍"); err != nil {
		t.Fatalf("React: %v", err)
	}

	w.access.writers[w.channel][w.bob] = false

	left, err := w.svc.Unreact(t.Context(), message.ID, w.bob, "👍")
	if err != nil {
		t.Fatalf("Unreact: %v", err)
	}
	if len(left.Reactions) != 0 {
		t.Errorf("reactions = %+v, want none", left.Reactions)
	}
}

// Everything that changes a message is announced to the watchers; a resend is not.
func TestEventsArePublished(t *testing.T) {
	w := newWorld(t)

	message := send(t, w, w.anna, "c-1", "привет")
	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "c-1", Content: "привет",
	}); err != nil {
		t.Fatalf("resend: %v", err)
	}
	if _, err := w.svc.Edit(t.Context(), message.ID, w.anna, "привет всем"); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if _, err := w.svc.React(t.Context(), message.ID, w.bob, "👍"); err != nil {
		t.Fatalf("React: %v", err)
	}
	if _, err := w.svc.Delete(t.Context(), message.ID, w.anna); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	want := []events.Kind{
		events.KindMessageCreated,
		events.KindMessageUpdated,
		events.KindReactionChanged,
		events.KindMessageDeleted,
	}
	got := w.published.kinds()
	if len(got) != len(want) {
		t.Fatalf("published %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, got[i], want[i])
		}
	}

	first := w.published.published[0]
	if first.WorkspaceID != w.workspace || first.ChannelID != w.channel || first.Seq != message.Seq {
		t.Errorf("event = %+v, want it addressed to the channel with the message seq", first)
	}
}

// What a reconnecting client missed comes back oldest first, without what it has.
func TestSinceEventsReturnsOnlyWhatIsMissing(t *testing.T) {
	w := newWorld(t)
	first := send(t, w, w.anna, "c-1", "первое")
	second := send(t, w, w.anna, "c-2", "второе")
	third := send(t, w, w.anna, "c-3", "третье")

	missed, err := w.svc.SinceEvents(t.Context(), w.channel, w.bob, first.Seq, 100)
	if err != nil {
		t.Fatalf("SinceEvents: %v", err)
	}
	if len(missed) != 2 {
		t.Fatalf("missed %d events, want 2", len(missed))
	}
	if missed[0].Seq != second.Seq || missed[1].Seq != third.Seq {
		t.Errorf("seqs = %d, %d; want %d, %d", missed[0].Seq, missed[1].Seq, second.Seq, third.Seq)
	}

	none, err := w.svc.SinceEvents(t.Context(), w.channel, w.bob, third.Seq, 100)
	if err != nil {
		t.Fatalf("SinceEvents: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("missed %d events after the last seq, want 0", len(none))
	}
}

func TestSinceEventsHidesChannelsTheCallerCannotRead(t *testing.T) {
	w := newWorld(t)
	send(t, w, w.anna, "c-1", "привет")

	if _, err := w.svc.SinceEvents(t.Context(), w.channel, w.outsider, 0, 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// A message in a conversation goes to each participant's own stream: there is
// no server to publish to, and an outsider must not receive it.
func TestConversationMessageIsPublishedPersonally(t *testing.T) {
	w := newWorld(t)

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.direct, ClientMessageID: "c-1", Content: "привет",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// One event per participant, because each has its own stream.
	if len(w.published.published) != 2 {
		t.Fatalf("published %d events, want one per participant", len(w.published.published))
	}
	for _, event := range w.published.published {
		if event.WorkspaceID != uuid.Nil {
			t.Errorf("a conversation event named a workspace: %+v", event)
		}
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range w.published.personal {
		seen[id] = true
	}
	if len(seen) != 2 || !seen[w.anna] || !seen[w.bob] || seen[w.outsider] {
		t.Errorf("published to %v, want both participants and nobody else", w.published.personal)
	}
}

// A secret chat carries ciphertext envelopes, not messages: the messages API
// refuses it, so plaintext can never be stored there by accident.
func TestSecretChannelRefusesMessages(t *testing.T) {
	w := newWorld(t)
	secret := uuid.New()
	w.access.channels[secret] = channels.Channel{ID: secret, Kind: channels.KindSecret}
	w.access.readers[secret] = map[uuid.UUID]bool{w.anna: true, w.bob: true}
	w.access.writers[secret] = map[uuid.UUID]bool{w.anna: true, w.bob: true}

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: secret, ClientMessageID: "c-1", Content: "не должно сохраниться",
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Send: err = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.History(t.Context(), secret, w.anna, nil, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("History: err = %v, want ErrNotFound", err)
	}
}
