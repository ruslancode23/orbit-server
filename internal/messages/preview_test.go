package messages

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/linkpreview"
)

type fakePreviews struct {
	attached []string
	detached []uuid.UUID
}

func (f *fakePreviews) Attach(_ context.Context, _, messageID uuid.UUID, rawURL string) (linkpreview.Preview, error) {
	f.attached = append(f.attached, rawURL)
	return linkpreview.Preview{ID: uuid.New(), URL: rawURL, Title: "Заголовок"}, nil
}

func (f *fakePreviews) Detach(_ context.Context, messageID uuid.UUID) error {
	f.detached = append(f.detached, messageID)
	return nil
}

func (f *fakePreviews) ForMessages(context.Context, []uuid.UUID) (map[uuid.UUID]linkpreview.Preview, error) {
	return map[uuid.UUID]linkpreview.Preview{}, nil
}

func TestSendFetchesTheFirstLinkWithoutHoldingTheSend(t *testing.T) {
	w := newWorld(t)
	previews := &fakePreviews{}
	w.svc.SetLinkPreviews(previews)

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "link-1",
		Content: "смотри https://orbit.chat/docs и https://orbit.chat/other",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// The send returned before the fetch; the test waits for the round like a
	// shutdown does.
	w.svc.Wait()

	if !slices.Equal(previews.attached, []string{"https://orbit.chat/docs"}) {
		t.Errorf("attached = %v, want only the first link", previews.attached)
	}
	if !slices.Contains(w.published.kinds(), events.KindMessageUpdated) {
		t.Errorf("published = %v, want message.updated when the card is ready", w.published.kinds())
	}
}

func TestSendWithoutALinkFetchesNothing(t *testing.T) {
	w := newWorld(t)
	previews := &fakePreviews{}
	w.svc.SetLinkPreviews(previews)
	send(t, w, w.anna, "no-link", "просто текст")
	w.svc.Wait()
	if len(previews.attached) != 0 {
		t.Errorf("attached = %v, want none", previews.attached)
	}
}

func TestEditingRecalculatesThePreview(t *testing.T) {
	w := newWorld(t)
	previews := &fakePreviews{}
	w.svc.SetLinkPreviews(previews)
	message := send(t, w, w.anna, "edit-1", "старый текст")

	if _, err := w.svc.Edit(t.Context(), message.ID, w.anna, "новая ссылка https://orbit.chat/new"); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	w.svc.Wait()
	if !slices.Equal(previews.attached, []string{"https://orbit.chat/new"}) {
		t.Errorf("attached = %v, want the new link", previews.attached)
	}

	if _, err := w.svc.Edit(t.Context(), message.ID, w.anna, "без ссылки"); err != nil {
		t.Fatalf("second Edit: %v", err)
	}
	if !slices.Contains(previews.detached, message.ID) {
		t.Errorf("detached = %v, want the edited message", previews.detached)
	}
}

func TestOnlyTheAuthorRemovesThePreview(t *testing.T) {
	w := newWorld(t)
	message := send(t, w, w.anna, "remove-1", "текст https://orbit.chat")
	previews := &fakePreviews{}
	w.svc.SetLinkPreviews(previews)

	if err := w.svc.RemovePreview(t.Context(), message.ID, w.bob); !errors.Is(err, ErrForbidden) {
		t.Errorf("Bob removing Anna's preview = %v, want ErrForbidden", err)
	}
	if err := w.svc.RemovePreview(t.Context(), message.ID, w.anna); err != nil {
		t.Fatalf("Anna removing her preview: %v", err)
	}
	if !slices.Contains(previews.detached, message.ID) {
		t.Errorf("detached = %v, want the message", previews.detached)
	}
}
