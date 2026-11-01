package audit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// recordTimeout bounds one insert. The journal is written after the action has
// already happened, so a slow database must not hold the request forever; the
// write is still allowed to outlive a client that disconnected in the same
// moment (context.WithoutCancel below).
const recordTimeout = 3 * time.Second

// Store is the persistence this package needs; store_postgres.go implements it.
type Store interface {
	// Insert writes one row. The database fills the id and the timestamp.
	Insert(ctx context.Context, e Entry) error
	// List reads at most limit rows of one server, newest first, with the
	// filter applied. The caller asks for one row more than the page it wants.
	List(ctx context.Context, workspaceID uuid.UUID, f Filter, limit int) ([]Entry, error)
}

// Service writes and reads the journal. It knows nothing about HTTP or about
// the modules that call it.
type Service struct {
	store Store
	log   *slog.Logger
}

// NewService returns a service over the store. The logger is where a failed
// insert is reported: Record returns the error too, but its callers must not
// undo the action they already took.
func NewService(store Store, log *slog.Logger) *Service {
	return &Service{store: store, log: log}
}

// Record appends one row.
//
// It is synchronous on purpose. An insert of one indexed row is a single
// round trip, far cheaper than the ban, role or channel change it describes,
// and a background channel would buy little: it would need its own queue,
// backpressure, shutdown and retries, and it would lose entries on a crash —
// exactly the entries an incident review would ask for. The write therefore
// happens before the request returns, with a short timeout, and a failure is
// logged and returned rather than raised into the action: an unavailable
// journal must not make a ban, a kick or a role change impossible. That is
// acceptable because the action's own row in its own table remains the source
// of truth; the journal is a second record of it, and a gap is visible in the
// log as an error.
//
// The context is detached from cancellation: a client that disconnects right
// after banning someone must not take the audit row with it. The timeout keeps
// that detached write bounded.
func (s *Service) Record(ctx context.Context, e Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := s.store.Insert(writeCtx, e); err != nil {
		if s.log != nil {
			s.log.ErrorContext(writeCtx, "record audit entry",
				slog.String("action", string(e.Action)),
				slog.Any("workspace_id", e.WorkspaceID),
				slog.Any("error", err))
		}
		return fmt.Errorf("record %s: %w", e.Action, err)
	}
	return nil
}

// List returns one page of a server's journal, newest first, and the cursor of
// the next page when there is one. An unknown action or actor is simply an
// empty page: the filter names a value, it does not assert that it exists.
func (s *Service) List(ctx context.Context, workspaceID uuid.UUID, params ListParams) (Page, error) {
	if workspaceID == uuid.Nil {
		return Page{}, ErrInvalidEntry
	}
	limit := newPageLimit(params.Limit)
	entries, err := s.store.List(ctx, workspaceID, params.Filter, limit+1)
	if err != nil {
		return Page{}, wrap("list", err)
	}
	return pageOf(entries, limit), nil
}
