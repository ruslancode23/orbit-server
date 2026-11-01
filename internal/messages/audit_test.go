package messages

import (
	"context"
	"errors"
	"testing"

	"github.com/ruslancode23/orbit-server/internal/audit"
)

type captureAuditor struct {
	entries []audit.Entry
	err     error
}

func (a *captureAuditor) Record(_ context.Context, entry audit.Entry) error {
	a.entries = append(a.entries, entry)
	return a.err
}

// Only a moderator's delete is a moderation action: an author removing their
// own message leaves no row, while manageMessages leaves one naming the
// moderator, the message, its channel and its author.
func TestModeratorDeleteIsRecorded(t *testing.T) {
	w := newWorld(t)
	journal := &captureAuditor{}
	w.svc.SetAuditor(journal)

	own := send(t, w, w.anna, "c-own", "своё сообщение")
	if _, err := w.svc.Delete(t.Context(), own.ID, w.anna); err != nil {
		t.Fatalf("delete own: %v", err)
	}
	if len(journal.entries) != 0 {
		t.Fatalf("entries = %+v, want none for an author's own delete", journal.entries)
	}

	other := send(t, w, w.bob, "c-other", "чужое сообщение")
	if _, err := w.svc.Delete(t.Context(), other.ID, w.admin); err != nil {
		t.Fatalf("delete as moderator: %v", err)
	}
	if len(journal.entries) != 1 {
		t.Fatalf("entries = %d, want the moderator's one", len(journal.entries))
	}
	entry := journal.entries[0]
	if entry.Action != audit.ActionMessageDelete || entry.TargetType != audit.TargetMessage {
		t.Errorf("entry = %s/%s, want message.delete/message", entry.Action, entry.TargetType)
	}
	if entry.WorkspaceID != w.workspace || entry.ActorID == nil || *entry.ActorID != w.admin {
		t.Errorf("entry = %+v, want the workspace and the admin as actor", entry)
	}
	if entry.TargetID == nil || *entry.TargetID != other.ID {
		t.Errorf("target = %v, want the message", entry.TargetID)
	}
	if entry.Metadata["channelId"] != w.channel || entry.Metadata["authorId"] != w.bob {
		t.Errorf("metadata = %v, want the channel and the author", entry.Metadata)
	}
}

// A failing journal must not make a moderator's delete fail after the message
// is already gone.
func TestBrokenJournalDoesNotUndoDelete(t *testing.T) {
	w := newWorld(t)
	w.svc.SetAuditor(&captureAuditor{err: errors.New("journal is down")})
	message := send(t, w, w.anna, "c-1", "привет")

	if _, err := w.svc.Delete(t.Context(), message.ID, w.admin); err != nil {
		t.Fatalf("Delete = %v, want success despite the journal", err)
	}
	stored, err := w.store.Message(t.Context(), message.ID)
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if stored.DeletedAt == nil {
		t.Fatal("message is not deleted: the action was rolled back")
	}
}
