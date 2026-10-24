package files

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/perms"
)

// conversationChannelStore is the channel a conversation leaves behind: no
// server, so internal/channels asks the conversations module who is in it.
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

func (s *conversationChannelStore) SetOverride(context.Context, uuid.UUID, perms.Override) error {
	return nil
}

func (s *conversationChannelStore) SetReadOnly(context.Context, channels.Channel, bool) error {
	return nil
}

func (s *conversationChannelStore) Reorder(context.Context, uuid.UUID, []uuid.UUID) error { return nil }

func (s *conversationChannelStore) Channels(_ context.Context, ids []uuid.UUID) ([]channels.Channel, error) {
	out := make([]channels.Channel, 0, len(ids))
	for _, id := range ids {
		if id == s.channel.ID {
			out = append(out, s.channel)
		}
	}
	return out, nil
}

type participantSet map[uuid.UUID]bool

func (p participantSet) IsParticipant(_ context.Context, _, userID uuid.UUID) (bool, error) {
	return p[userID], nil
}

// A download link is issued only to someone who may read the channel of the
// message; a participant who left a group is no longer that.
func TestLeavingAConversationRevokesAttachmentLinks(t *testing.T) {
	store := newFakeStore()
	channelID := uuid.New()
	messageID := uuid.New()
	anna, bob := uuid.New(), uuid.New()

	attachment, err := store.Create(t.Context(), Attachment{
		UploaderID: anna, StorageKey: "files/one", FileName: "one.png",
		MimeType: "image/png", SizeBytes: 3,
	})
	if err != nil {
		t.Fatalf("Create attachment: %v", err)
	}
	if _, err := store.Attach(t.Context(), attachment.ID, messageID, anna); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	members := participantSet{anna: true, bob: true}
	access := channels.NewService(&conversationChannelStore{channel: channels.Channel{
		ID: channelID, Kind: channels.KindText,
	}}, nil)
	access.SetParticipants(members)
	svc := NewService(store, newFakeStorage(), access,
		&fakeMessages{channelOf: map[uuid.UUID]uuid.UUID{messageID: channelID}},
		nil, 1<<20, time.Hour)

	if _, err := svc.Link(t.Context(), attachment.ID, bob); err != nil {
		t.Fatalf("link before leaving: %v", err)
	}

	delete(members, bob)

	if _, err := svc.Link(t.Context(), attachment.ID, bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("link after leaving: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.Link(t.Context(), attachment.ID, anna); err != nil {
		t.Errorf("link for the remaining member: %v", err)
	}
}
