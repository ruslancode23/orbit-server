package invites

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

// Making and revoking a link leaves one row each. The code never appears in
// the journal: it is a password to the server.
func TestInviteChangesAreRecorded(t *testing.T) {
	w := newWorld(t)
	journal := &captureAuditor{}
	w.svc.SetAuditor(journal)

	created, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.svc.Revoke(t.Context(), created.ID, w.owner); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	wanted := []audit.Action{audit.ActionInviteCreate, audit.ActionInviteRevoke}
	if len(journal.entries) != len(wanted) {
		t.Fatalf("entries = %d, want %d: %+v", len(journal.entries), len(wanted), journal.entries)
	}
	for i, action := range wanted {
		entry := journal.entries[i]
		if entry.Action != action || entry.TargetType != audit.TargetInvite {
			t.Errorf("entry %d = %s/%s, want %s/invite", i, entry.Action, entry.TargetType, action)
		}
		if entry.WorkspaceID != w.members.workspace.ID || entry.ActorID == nil || *entry.ActorID != w.owner {
			t.Errorf("entry %d = %+v, want the workspace and the owner", i, entry)
		}
		if entry.TargetID == nil || *entry.TargetID != created.ID {
			t.Errorf("entry %d target = %v, want the invite", i, entry.TargetID)
		}
		if len(entry.Metadata) != 0 {
			t.Errorf("entry %d metadata = %v, want none (no code)", i, entry.Metadata)
		}
	}
}

// A failing journal must not leave a link alive after a successful revoke.
func TestBrokenJournalDoesNotUndoRevoke(t *testing.T) {
	w := newWorld(t)
	w.svc.SetAuditor(&captureAuditor{err: errors.New("journal is down")})

	created, err := w.svc.Create(t.Context(), w.members.workspace.ID, w.owner, New{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.svc.Revoke(t.Context(), created.ID, w.owner); err != nil {
		t.Fatalf("Revoke = %v, want success despite the journal", err)
	}
	if _, err := w.store.Live(t.Context(), created.Code); !errors.Is(err, ErrNotFound) {
		t.Fatalf("link is still live (err %v): the action was rolled back", err)
	}
}
