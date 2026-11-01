package channels

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

// Creating, renaming and deleting a channel each leave one row with the channel
// as the target and its name in the metadata.
func TestChannelChangesAreRecorded(t *testing.T) {
	w := newWorld(t)
	journal := &captureAuditor{}
	w.svc.SetAuditor(journal)

	created, err := w.svc.Create(t.Context(), w.workspace, w.owner, NewChannel{Name: "голос", Kind: KindVoice})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	name := "переименован"
	if _, err := w.svc.Update(t.Context(), created.Channel.ID, w.owner, Update{Name: &name}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := w.svc.Delete(t.Context(), created.Channel.ID, w.owner); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	wanted := []audit.Action{audit.ActionChannelCreate, audit.ActionChannelUpdate, audit.ActionChannelDelete}
	if len(journal.entries) != len(wanted) {
		t.Fatalf("entries = %d, want %d: %+v", len(journal.entries), len(wanted), journal.entries)
	}
	for i, action := range wanted {
		entry := journal.entries[i]
		if entry.Action != action {
			t.Errorf("entry %d = %s, want %s", i, entry.Action, action)
		}
		if entry.WorkspaceID != w.workspace || entry.TargetType != audit.TargetChannel {
			t.Errorf("entry %d = %+v, want the workspace and a channel target", i, entry)
		}
		if entry.TargetID == nil || *entry.TargetID != created.Channel.ID {
			t.Errorf("entry %d target = %v, want the channel", i, entry.TargetID)
		}
		if entry.ActorID == nil || *entry.ActorID != w.owner {
			t.Errorf("entry %d actor = %v, want the owner", i, entry.ActorID)
		}
	}
	if journal.entries[0].Metadata["name"] != "голос" || journal.entries[0].Metadata["kind"] != string(KindVoice) {
		t.Errorf("create metadata = %v, want the name and kind", journal.entries[0].Metadata)
	}
	if journal.entries[1].Metadata["name"] != name {
		t.Errorf("update metadata = %v, want the new name", journal.entries[1].Metadata)
	}
}

// An unavailable journal must not undo a channel that has already been deleted.
func TestBrokenJournalDoesNotUndoChannelDelete(t *testing.T) {
	w := newWorld(t)
	w.svc.SetAuditor(&captureAuditor{err: errors.New("journal is down")})

	if err := w.svc.Delete(t.Context(), w.general.ID, w.owner); err != nil {
		t.Fatalf("Delete = %v, want success despite the journal", err)
	}
	if _, err := w.store.Channel(t.Context(), w.general.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("channel is still there (err %v): the action was rolled back", err)
	}
}
