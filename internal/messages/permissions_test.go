package messages

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
)

// fakeFiles says whether attaching is allowed and records what was attached.
type fakeFiles struct {
	refuse   map[uuid.UUID]bool // by caller
	attached []uuid.UUID
}

func (f *fakeFiles) MayAttach(_ context.Context, _ channels.Channel, callerID uuid.UUID) error {
	if f.refuse[callerID] {
		return ErrAttachNotAllowed
	}
	return nil
}

func (f *fakeFiles) AttachAll(_ context.Context, _, _ uuid.UUID, ids []uuid.UUID) ([]Attachment, error) {
	f.attached = append(f.attached, ids...)
	return nil, nil
}

func (f *fakeFiles) ByMessages(_ context.Context, _ []uuid.UUID) (map[uuid.UUID][]Attachment, error) {
	return map[uuid.UUID][]Attachment{}, nil
}

// Sending files needs the right to attach them; the message is refused whole,
// not stored without its files.
func TestSendWithFilesNeedsTheRightToAttach(t *testing.T) {
	w := newWorld(t)
	files := &fakeFiles{refuse: map[uuid.UUID]bool{w.anna: true}}
	w.svc.SetFiles(files)
	file := uuid.New()

	_, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "f-1", Content: "смотри", AttachmentIDs: []uuid.UUID{file},
	})
	if !errors.Is(err, ErrAttachNotAllowed) || !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrAttachNotAllowed", err)
	}
	if len(w.store.messages) != 0 || len(files.attached) != 0 || len(w.published.published) != 0 {
		t.Errorf("messages = %d, attached = %d, events = %d, want nothing at all",
			len(w.store.messages), len(files.attached), len(w.published.published))
	}

	// Text alone is still fine, and someone allowed sends files.
	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "f-2", Content: "просто текст",
	}); err != nil {
		t.Errorf("text only: %v", err)
	}
	if _, _, err := w.svc.Send(t.Context(), w.bob, NewMessage{
		ChannelID: w.channel, ClientMessageID: "f-3", AttachmentIDs: []uuid.UUID{file},
	}); err != nil {
		t.Errorf("allowed caller: %v", err)
	}
	if len(files.attached) != 1 {
		t.Errorf("attached = %d, want the allowed file only", len(files.attached))
	}
}
