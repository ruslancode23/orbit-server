package conversations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore keeps conversations in memory with the rule the database keeps:
// one direct conversation per pair.
type fakeStore struct {
	conversations map[uuid.UUID]*Conversation
	directKeys    map[string]uuid.UUID
	known         map[uuid.UUID]bool
	creates       int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		conversations: map[uuid.UUID]*Conversation{},
		directKeys:    map[string]uuid.UUID{},
		known:         map[uuid.UUID]bool{},
	}
}

func (s *fakeStore) Create(_ context.Context, kind Kind, directKey string, memberIDs []uuid.UUID) (Conversation, error) {
	s.creates++
	conversation := Conversation{ID: uuid.New(), Kind: kind, CreatedAt: time.Now()}
	for _, id := range memberIDs {
		conversation.Members = append(conversation.Members, Member{ID: id, JoinedAt: time.Now()})
	}
	s.conversations[conversation.ID] = &conversation
	if directKey != "" {
		s.directKeys[directKey] = conversation.ID
	}
	return conversation, nil
}

func (s *fakeStore) Direct(_ context.Context, directKey string) (Conversation, error) {
	id, ok := s.directKeys[directKey]
	if !ok {
		return Conversation{}, ErrNotFound
	}
	return *s.conversations[id], nil
}

func (s *fakeStore) Conversation(_ context.Context, id uuid.UUID) (Conversation, error) {
	conversation, ok := s.conversations[id]
	if !ok {
		return Conversation{}, ErrNotFound
	}
	return *conversation, nil
}

func (s *fakeStore) List(_ context.Context, userID uuid.UUID) ([]Conversation, error) {
	var out []Conversation
	for _, conversation := range s.conversations {
		for _, member := range conversation.Members {
			if member.ID == userID {
				out = append(out, *conversation)
				break
			}
		}
	}
	return out, nil
}

func (s *fakeStore) IsParticipant(_ context.Context, channelID, userID uuid.UUID) (bool, error) {
	conversation, ok := s.conversations[channelID]
	if !ok {
		return false, nil
	}
	for _, member := range conversation.Members {
		if member.ID == userID {
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeStore) KnownUsers(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		out[id] = s.known[id]
	}
	return out, nil
}

type world struct {
	svc      *Service
	store    *fakeStore
	anna     uuid.UUID
	bob      uuid.UUID
	vika     uuid.UUID
	outsider uuid.UUID
}

const maxMembers = 4

func newWorld(t *testing.T) world {
	t.Helper()
	w := world{
		store: newFakeStore(),
		anna:  uuid.New(), bob: uuid.New(), vika: uuid.New(), outsider: uuid.New(),
	}
	for _, id := range []uuid.UUID{w.anna, w.bob, w.vika, w.outsider} {
		w.store.known[id] = true
	}
	w.svc = NewService(w.store, maxMembers)
	return w
}

// A pair has one dialogue, whoever opens it and in whichever order.
func TestDirectConversationIsOpenedOnce(t *testing.T) {
	w := newWorld(t)

	first, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if first.Kind != KindDirect || len(first.Members) != 2 {
		t.Errorf("conversation = %+v, want a direct one with two people", first)
	}

	again, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("second attempt = %v, want the same %v", again.ID, first.ID)
	}

	// The other way round names the same conversation.
	fromBob, err := w.svc.Start(t.Context(), w.bob, []uuid.UUID{w.anna})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if fromBob.ID != first.ID {
		t.Errorf("from the other side = %v, want %v", fromBob.ID, first.ID)
	}
	if w.store.creates != 1 {
		t.Errorf("created %d conversations, want 1", w.store.creates)
	}
}

func TestGroupConversation(t *testing.T) {
	w := newWorld(t)

	group, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob, w.vika})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if group.Kind != KindGroup || len(group.Members) != 3 {
		t.Errorf("conversation = %+v, want a group of three", group)
	}

	// A group is not deduplicated: the same people may have several.
	another, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob, w.vika})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if another.ID == group.ID {
		t.Error("a second group with the same people reused the first")
	}
}

func TestStartRejectsNobodyUnknownAndTooMany(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.Start(t.Context(), w.anna, nil); !errors.Is(err, ErrNoMembers) {
		t.Errorf("nobody: err = %v, want ErrNoMembers", err)
	}
	// Talking to yourself is not a conversation either.
	if _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.anna}); !errors.Is(err, ErrNoMembers) {
		t.Errorf("only myself: err = %v, want ErrNoMembers", err)
	}
	if _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{uuid.New()}); !errors.Is(err, ErrUnknownMember) {
		t.Errorf("unknown account: err = %v, want ErrUnknownMember", err)
	}

	many := make([]uuid.UUID, 0, maxMembers)
	for range maxMembers {
		id := uuid.New()
		w.store.known[id] = true
		many = append(many, id)
	}
	if _, err := w.svc.Start(t.Context(), w.anna, many); !errors.Is(err, ErrTooManyMembers) {
		t.Errorf("too many: err = %v, want ErrTooManyMembers", err)
	}
}

// A conversation is for the people in it, and nobody else (FR-04).
func TestOnlyParticipantsSeeAConversation(t *testing.T) {
	w := newWorld(t)
	conversation, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	for _, id := range []uuid.UUID{w.anna, w.bob} {
		if _, err := w.svc.Get(t.Context(), conversation.ID, id); err != nil {
			t.Errorf("a participant cannot read it: %v", err)
		}
	}
	if _, err := w.svc.Get(t.Context(), conversation.ID, w.outsider); !errors.Is(err, ErrNotFound) {
		t.Errorf("a third person: err = %v, want ErrNotFound", err)
	}

	// The same answer feeds internal/channels, which guards messages and files.
	inside, err := w.svc.IsParticipant(t.Context(), conversation.ID, w.outsider)
	if err != nil {
		t.Fatalf("IsParticipant: %v", err)
	}
	if inside {
		t.Error("a third person counts as a participant")
	}
}

func TestListShowsOnlyYourConversations(t *testing.T) {
	w := newWorld(t)
	mine, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := w.svc.Start(t.Context(), w.bob, []uuid.UUID{w.vika}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	list, err := w.svc.List(t.Context(), w.anna)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != mine.ID {
		t.Errorf("conversations = %+v, want only Anna's own", list)
	}

	none, err := w.svc.List(t.Context(), w.outsider)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an outsider sees %d conversations, want 0", len(none))
	}
}

func TestDuplicateNamesAreCollapsed(t *testing.T) {
	w := newWorld(t)

	conversation, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob, w.bob, w.anna})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if conversation.Kind != KindDirect || len(conversation.Members) != 2 {
		t.Errorf("conversation = %+v, want a direct one with two people", conversation)
	}
}
