package files

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/messages"
)

// ForMessages adapts this module to what internal/messages needs: the messages
// module owns the message, this one owns the files on it.
type ForMessages struct{ svc *Service }

func NewForMessages(svc *Service) *ForMessages { return &ForMessages{svc: svc} }

var _ messages.Files = (*ForMessages)(nil)

func (f *ForMessages) MayAttach(ctx context.Context, channel channels.Channel, callerID uuid.UUID) error {
	err := f.svc.MayAttach(ctx, channel, callerID)
	switch {
	case errors.Is(err, ErrAttachNotAllowed):
		return messages.ErrAttachNotAllowed
	case errors.Is(err, ErrNotFound):
		return messages.ErrNotFound
	}
	return err
}

func (f *ForMessages) AttachAll(ctx context.Context, messageID, uploaderID uuid.UUID, ids []uuid.UUID) ([]messages.Attachment, error) {
	attached, err := f.svc.AttachAll(ctx, messageID, uploaderID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]messages.Attachment, 0, len(attached))
	for _, a := range attached {
		out = append(out, toMessageAttachment(a))
	}
	return out, nil
}

func (f *ForMessages) ByMessages(ctx context.Context, messageIDs []uuid.UUID) (map[uuid.UUID][]messages.Attachment, error) {
	list, err := f.svc.ByMessages(ctx, messageIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]messages.Attachment, len(messageIDs))
	for _, a := range list {
		if a.MessageID == nil {
			continue
		}
		out[*a.MessageID] = append(out[*a.MessageID], toMessageAttachment(a))
	}
	return out, nil
}

func toMessageAttachment(a Attachment) messages.Attachment {
	return messages.Attachment{
		ID:        a.ID,
		FileName:  a.FileName,
		MimeType:  a.MimeType,
		SizeBytes: a.SizeBytes,
		Width:     a.Width,
		Height:    a.Height,
		CreatedAt: a.CreatedAt,
	}
}
