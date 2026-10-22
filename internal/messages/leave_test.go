package messages

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/conversations"
)

// conversationChannelStore serves the one channel a membership test needs.
type conversationChannelStore struct {
	channel channels.Channel
}

func (s *conversationChannelStore) Create(context.Context, channels.NewChannel) (channels.Channel, error) {
	return channels.Channel{}, errors.New("not implemented")
}

func (s *conversationChannelStore) Channel(_ context.Context, id uuid.UUID) (channels.Channel, error) {
	if id != s.channel.ID {
		return channels.Channel{}, channels.ErrNotFound
	}
	return s.channel, nil
}

func (s *conversationChannelStore) List(context.Context, uuid.UUID) ([]channels.Channel, error) {
	return []channels.Channel{s.channel}, nil
}

func (s *conversationChannelStore) Update(context.Context, uuid.UUID, channels.Update) (channels.Channel, error) {
	return s.channel, nil
}

func (s *conversationChannelStore) Delete(context.Context, uuid.UUID) error { return nil }

func (s *conversationChannelStore) SetReadOnly(context.Context, channels.Channel, bool) error {
	return nil
}

func (s *conversationChannelStore) Reorder(context.Context, uuid.UUID, []uuid.UUID) error { return nil }

// participants is the join table the conversations module owns. Removing a row
// is what leaving a group does.
type participants map[uuid.UUID]bool

func (p participants) IsParticipant(_ context.Context, _, userID uuid.UUID) (bool, error) {
	return p[userID], nil
}

// History and the reconnect replay both go through internal/channels, so a
// former member gets the same not-found answer as a stranger.
func TestLeavingAConversationRevokesHistoryAndReplay(t *testing.T) {
	w := newWorld(t)
	members := participants{w.anna: true, w.bob: true}
	access := channels.NewService(&conversationChannelStore{channel: channels.Channel{
		ID: w.direct, Kind: channels.KindText,
	}}, nil)
	access.SetParticipants(members)
	svc := NewService(w.store, access, nil, nil, nil)

	if _, err := svc.History(t.Context(), w.direct, w.bob, nil, 10); err != nil {
		t.Fatalf("history before leaving: %v", err)
	}
	if _, err := svc.SinceEvents(t.Context(), w.direct, w.bob, 0, 10); err != nil {
		t.Fatalf("replay before leaving: %v", err)
	}

	delete(members, w.bob)

	if _, err := svc.History(t.Context(), w.direct, w.bob, nil, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("history after leaving: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.SinceEvents(t.Context(), w.direct, w.bob, 0, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("replay after leaving: err = %v, want ErrNotFound", err)
	}
	// The remaining member is unaffected.
	if _, err := svc.History(t.Context(), w.direct, w.anna, nil, 10); err != nil {
		t.Errorf("history for the remaining member: %v", err)
	}
}

// A live message goes to the current participants only: the list the fan-out
// reads is the same one leaving removes the person from.
func TestConversationFanOutFollowsCurrentMembers(t *testing.T) {
	w := newWorld(t)
	conversation := &fakeConversations{
		members: map[uuid.UUID][]uuid.UUID{w.direct: {w.anna, w.bob}},
		kinds:   map[uuid.UUID]conversations.Kind{w.direct: conversations.KindDirect},
	}
	w.svc.SetConversations(conversation)

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.direct, ClientMessageID: "before-leave", Content: "hi",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range w.published.personal {
		seen[id] = true
	}
	if !seen[w.anna] || !seen[w.bob] {
		t.Fatalf("message published to %v, want both participants", w.published.personal)
	}

	// Bob leaves: the member list shrinks, and so does the audience.
	conversation.members[w.direct] = []uuid.UUID{w.anna}
	w.published.personal = nil
	w.published.published = nil
	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.direct, ClientMessageID: "after-leave", Content: "hi again",
	}); err != nil {
		t.Fatalf("Send after leaving: %v", err)
	}
	for _, id := range w.published.personal {
		if id != w.anna {
			t.Errorf("message published to %v after Bob left, want only Anna", id)
		}
	}
	if len(w.published.personal) == 0 {
		t.Error("message was not published to the remaining member")
	}
}
