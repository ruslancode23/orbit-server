package servers

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/audit"
	"github.com/ruslancode23/orbit-server/internal/perms"
	"github.com/ruslancode23/orbit-server/internal/profile"
)

// AuditLog is the read side of the moderation journal (internal/audit). The
// servers module owns the HTTP endpoint because the access rule and the actor
// profiles are already here: the journal itself stays storage-only.
type AuditLog interface {
	List(ctx context.Context, workspaceID uuid.UUID, params audit.ListParams) (audit.Page, error)
}

// AuditEntry is one journal row with the public profile of the moderator who
// took the action. Actor is nil when the account has been deleted, and
// ActorDeleted then says so explicitly — the row itself is kept.
type AuditEntry struct {
	audit.Entry
	Actor        *profile.Public
	ActorDeleted bool
}

// AuditPage is one page of the journal and the cursor of the next, older page.
type AuditPage struct {
	Entries []AuditEntry
	Next    *audit.Cursor
}

// SetAuditLog wires the journal's read side in after construction.
func (s *Service) SetAuditLog(log AuditLog) { s.auditLog = log }

// ListAudit returns one page of the moderation journal of a server.
//
// Reading it takes manageServer: the journal names who banned whom, which
// roles exist and which channels were deleted, so it belongs to whoever runs
// the server — an administrator role grants it too, and the owner has
// everything. A caller from outside the server gets ErrNotMember and the API
// answers 404, exactly as for a server that does not exist. A plain member
// without the permission gets ErrForbidden: they already know the server
// exists, and only the permission is missing.
func (s *Service) ListAudit(ctx context.Context, workspaceID, callerID uuid.UUID, params audit.ListParams) (AuditPage, error) {
	if _, err := s.Require(ctx, workspaceID, callerID, perms.ManageServer); err != nil {
		return AuditPage{}, err
	}
	if s.auditLog == nil {
		return AuditPage{}, fmt.Errorf("audit log is not wired")
	}
	page, err := s.auditLog.List(ctx, workspaceID, params)
	if err != nil {
		return AuditPage{}, err
	}
	entries := make([]AuditEntry, len(page.Entries))
	actors := make([]uuid.UUID, 0, len(page.Entries))
	for i, entry := range page.Entries {
		entries[i] = AuditEntry{Entry: entry, ActorDeleted: entry.ActorID == nil}
		if entry.ActorID != nil {
			actors = append(actors, *entry.ActorID)
		}
	}
	if s.profiles != nil && len(actors) > 0 {
		// One batch for the whole page, never one query per row.
		loaded, err := s.profiles.Profiles(ctx, actors)
		if err != nil {
			return AuditPage{}, fmt.Errorf("load audit actors: %w", err)
		}
		for i := range entries {
			if entries[i].ActorID == nil {
				continue
			}
			if actor, ok := loaded[*entries[i].ActorID]; ok {
				entries[i].Actor = &actor
				continue
			}
			// The foreign key would have made the id NULL, so this is a race
			// at most; «deleted» is the honest answer either way.
			entries[i].ActorDeleted = true
		}
	}
	return AuditPage{Entries: entries, Next: page.Next}, nil
}
