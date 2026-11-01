package audit

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore keeps entries in memory and applies the same filter and order the
// SQL does, so the service's paging can be tested without a database.
type fakeStore struct {
	entries   []Entry
	insertErr error
	listErr   error
	lastLimit int
	lastWhere uuid.UUID
	lastQuery Filter
	// insertCanceled records whether Insert got a live context.
	insertCanceled bool
}

func (s *fakeStore) Insert(ctx context.Context, e Entry) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.insertCanceled = ctx.Err() != nil
	s.entries = append(s.entries, e)
	return nil
}

func (s *fakeStore) List(_ context.Context, workspaceID uuid.UUID, f Filter, limit int) ([]Entry, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.lastWhere, s.lastQuery, s.lastLimit = workspaceID, f, limit
	out := make([]Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		if entry.WorkspaceID != workspaceID {
			continue
		}
		if f.Action != nil && entry.Action != *f.Action {
			continue
		}
		if f.ActorID != nil && (entry.ActorID == nil || *entry.ActorID != *f.ActorID) {
			continue
		}
		if f.Before != nil {
			if entry.CreatedAt.After(f.Before.CreatedAt) {
				continue
			}
			if entry.CreatedAt.Equal(f.Before.CreatedAt) && entry.ID.String() >= f.Before.ID.String() {
				continue
			}
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func filled(workspaceID, actorID uuid.UUID, action Action, at time.Time) Entry {
	return Entry{
		ID:          uuid.New(),
		WorkspaceID: workspaceID,
		ActorID:     &actorID,
		Action:      action,
		TargetType:  TargetUser,
		TargetID:    Target(uuid.New()),
		CreatedAt:   at,
	}
}

func TestRecord(t *testing.T) {
	ctx := context.Background()
	workspaceID, actorID := uuid.New(), uuid.New()

	t.Run("appends a valid entry", func(t *testing.T) {
		store := &fakeStore{}
		svc := NewService(store, nil)
		if err := svc.Record(ctx, filled(workspaceID, actorID, ActionMemberBan, time.Now())); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if len(store.entries) != 1 || store.entries[0].Action != ActionMemberBan {
			t.Fatalf("entries = %+v, want the ban", store.entries)
		}
	})

	t.Run("rejects an entry without a server", func(t *testing.T) {
		svc := NewService(&fakeStore{}, nil)
		err := svc.Record(ctx, Entry{ActorID: &actorID, Action: ActionMemberBan, TargetType: TargetUser})
		if !errors.Is(err, ErrInvalidEntry) {
			t.Fatalf("Record = %v, want ErrInvalidEntry", err)
		}
	})

	t.Run("returns the store error instead of hiding it", func(t *testing.T) {
		boom := errors.New("database is down")
		svc := NewService(&fakeStore{insertErr: boom}, nil)
		err := svc.Record(ctx, filled(workspaceID, actorID, ActionMemberBan, time.Now()))
		if !errors.Is(err, boom) {
			t.Fatalf("Record = %v, want the wrapped store error", err)
		}
	})

	t.Run("writes even when the caller's context was cancelled", func(t *testing.T) {
		store := &fakeStore{}
		svc := NewService(store, nil)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if err := svc.Record(cancelled, filled(workspaceID, actorID, ActionMemberKick, time.Now())); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if store.insertCanceled {
			t.Fatal("Insert got a cancelled context: a disconnect must not drop the row")
		}
	})

	t.Run("allows a missing actor", func(t *testing.T) {
		store := &fakeStore{}
		svc := NewService(store, nil)
		entry := filled(workspaceID, actorID, ActionChannelDelete, time.Now())
		entry.ActorID = nil // a system action, or a row whose actor is already gone
		if err := svc.Record(ctx, entry); err != nil {
			t.Fatalf("Record: %v", err)
		}
	})
}

func TestListPages(t *testing.T) {
	ctx := context.Background()
	workspaceID, actorID := uuid.New(), uuid.New()
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	store := &fakeStore{}
	svc := NewService(store, nil)
	for i := range 60 {
		entry := filled(workspaceID, actorID, ActionMemberBan, start.Add(time.Duration(i)*time.Second))
		entry.ID = uuid.New()
		store.entries = append(store.entries, entry)
	}
	// Another server's entries must never leak into the page.
	store.entries = append(store.entries, filled(uuid.New(), actorID, ActionMemberBan, start))

	first, err := svc.List(ctx, workspaceID, ListParams{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(first.Entries) != DefaultLimit {
		t.Fatalf("first page = %d entries, want %d", len(first.Entries), DefaultLimit)
	}
	if first.Next == nil {
		t.Fatal("first page has no cursor although the journal continues")
	}
	if store.lastLimit != DefaultLimit+1 {
		t.Errorf("store limit = %d, want %d: one extra row detects the next page", store.lastLimit, DefaultLimit+1)
	}

	second, err := svc.List(ctx, workspaceID, ListParams{Filter: Filter{Before: first.Next}})
	if err != nil {
		t.Fatalf("second List: %v", err)
	}
	if len(second.Entries) != 10 || second.Next != nil {
		t.Fatalf("second page = %d entries, next %v; want the last 10 and no cursor", len(second.Entries), second.Next)
	}
	seen := map[uuid.UUID]bool{}
	for _, entry := range slices.Concat(first.Entries, second.Entries) {
		if seen[entry.ID] {
			t.Fatalf("entry %s appears on both pages", entry.ID)
		}
		seen[entry.ID] = true
	}
	if len(seen) != 60 {
		t.Fatalf("pages cover %d entries, want 60", len(seen))
	}

	// Exactly one full page: no cursor, because there is nothing after it.
	store.entries = store.entries[:50]
	only, err := svc.List(ctx, workspaceID, ListParams{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(only.Entries) != 50 || only.Next != nil {
		t.Fatalf("full single page = %d entries, next %v; want 50 and no cursor", len(only.Entries), only.Next)
	}
}

func TestListFiltersAndLimit(t *testing.T) {
	ctx := context.Background()
	workspaceID, anna, boris := uuid.New(), uuid.New(), uuid.New()
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	store := &fakeStore{}
	svc := NewService(store, nil)
	for i := range 5 {
		store.entries = append(store.entries, filled(workspaceID, anna, ActionMemberBan, start.Add(time.Duration(i)*time.Second)))
		store.entries = append(store.entries, filled(workspaceID, boris, ActionMemberKick, start.Add(time.Duration(i)*time.Second)))
	}

	action := ActionMemberKick
	page, err := svc.List(ctx, workspaceID, ListParams{Filter: Filter{Action: &action}, Limit: 3})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Entries) != 3 || page.Next == nil {
		t.Fatalf("filtered page = %d entries, next %v; want 3 and a cursor", len(page.Entries), page.Next)
	}
	for _, entry := range page.Entries {
		if entry.Action != ActionMemberKick || entry.ActorID == nil || *entry.ActorID != boris {
			t.Errorf("entry %s does not match the filter", entry.Action)
		}
	}
	if store.lastQuery.Action == nil || *store.lastQuery.Action != action {
		t.Errorf("action filter did not reach the store: %+v", store.lastQuery)
	}

	store.lastQuery = Filter{}
	page, err = svc.List(ctx, workspaceID, ListParams{Filter: Filter{ActorID: &anna}, Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Entries) != 2 || store.lastQuery.ActorID == nil || *store.lastQuery.ActorID != anna {
		t.Errorf("actor filter = %+v (%d entries), want Anna's", store.lastQuery, len(page.Entries))
	}

	// A limit above the maximum is clamped; a zero one takes the default.
	if _, err := svc.List(ctx, workspaceID, ListParams{Limit: 500}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if store.lastLimit != MaxLimit+1 {
		t.Errorf("store limit = %d, want the clamped %d", store.lastLimit, MaxLimit+1)
	}
	if _, err := svc.List(ctx, workspaceID, ListParams{Limit: -7}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if store.lastLimit != DefaultLimit+1 {
		t.Errorf("store limit = %d, want the default %d", store.lastLimit, DefaultLimit+1)
	}

	// A filter nobody matches is an empty page, not an error.
	unknown := Action("nothing.like.this")
	empty, err := svc.List(ctx, workspaceID, ListParams{Filter: Filter{Action: &unknown}})
	if err != nil || len(empty.Entries) != 0 || empty.Next != nil {
		t.Errorf("unknown action = %d entries, next %v, err %v; want an empty page", len(empty.Entries), empty.Next, err)
	}
}

func TestListStoreError(t *testing.T) {
	boom := errors.New("connection reset")
	svc := NewService(&fakeStore{listErr: boom}, nil)
	_, err := svc.List(context.Background(), uuid.New(), ListParams{})
	if !errors.Is(err, boom) {
		t.Fatalf("List = %v, want the wrapped store error", err)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	original := Cursor{
		CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 123456000, time.UTC),
		ID:        uuid.MustParse("6f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"),
	}
	parsed, err := ParseCursor(original.String())
	if err != nil {
		t.Fatalf("ParseCursor: %v", err)
	}
	if !parsed.CreatedAt.Equal(original.CreatedAt) || parsed.ID != original.ID {
		t.Fatalf("parsed = %+v, want %+v", parsed, original)
	}

	for _, raw := range []string{
		"",
		"not-base64",
		"bm90LWEtY3Vyc29y", // "not-a-cursor"
		"MTIz",             // no id
		"MTIzOg",           // empty id
		"MTIzOm5vdC1hLXV1aWQ",
		base64.RawURLEncoding.EncodeToString([]byte("123:" + uuid.Nil.String())), // nil uuid
	} {
		if _, err := ParseCursor(raw); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("ParseCursor(%q) = %v, want ErrInvalidCursor", raw, err)
		}
	}
}
