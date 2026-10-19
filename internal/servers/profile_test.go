package servers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/events"
)

// fakeStorage keeps icon objects in memory and signs links that name the key.
type fakeStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{objects: map[string][]byte{}, types: map[string]string{}}
}

func (s *fakeStorage) Put(_ context.Context, key, mimeType string, body io.Reader, _ int64) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
	s.types[key] = mimeType
	return nil
}

func (s *fakeStorage) SignedURL(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return "https://store.test/" + key + "?signature=x", nil
}

func (s *fakeStorage) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func (s *fakeStorage) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

// fakeWatchers records what the service announced.
type fakeWatchers struct {
	mu        sync.Mutex
	published []events.Event
	revoked   []uuid.UUID
}

func (f *fakeWatchers) Publish(event events.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, event)
}

func (f *fakeWatchers) Revoke(_, userID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, userID)
}

// updates are the workspace.updated events, the ones these tests look at.
func (f *fakeWatchers) updates() []events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []events.Event
	for _, e := range f.published {
		if e.Kind == events.KindWorkspaceUpdated {
			out = append(out, e)
		}
	}
	return out
}

const iconLimit = 4 << 10

// pngBytes is enough of a PNG for http.DetectContentType, padded to size.
func pngBytes(size int) []byte {
	head := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	if size < len(head) {
		size = len(head)
	}
	return append(head, bytes.Repeat([]byte{0}, size-len(head))...)
}

// profileWorld is newWorld with icons and watchers wired in.
func newProfileWorld(t *testing.T) (world, *fakeStorage, *fakeWatchers) {
	t.Helper()
	w := newWorld(t)
	storage := newFakeStorage()
	watchers := &fakeWatchers{}
	w.svc = NewService(w.store, watchers, Icons{Storage: storage, MaxBytes: iconLimit, LinkTTL: time.Minute})
	return w, storage, watchers
}

func TestCreateTakesADescription(t *testing.T) {
	svc := NewService(newFakeStore(), nil, Icons{})

	created, err := svc.Create(t.Context(), NewWorkspace{Name: "Команда", Description: "  про всё  "}, uuid.New())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Workspace.Description != "про всё" {
		t.Errorf("description = %q, want it trimmed", created.Workspace.Description)
	}

	tooLong := strings.Repeat("я", maxDescriptionLength+1)
	if _, err := svc.Create(t.Context(), NewWorkspace{Name: "Команда", Description: tooLong}, uuid.New()); !errors.Is(err, ErrInvalidDescription) {
		t.Errorf("err = %v, want ErrInvalidDescription", err)
	}
	// The limit counts characters, not bytes.
	if _, err := svc.Create(t.Context(), NewWorkspace{Name: "Команда", Description: tooLong[2:]}, uuid.New()); err != nil {
		t.Errorf("500 Cyrillic characters: %v", err)
	}
}

func TestUpdateChangesOnlyWhatWasSent(t *testing.T) {
	w, _, watchers := newProfileWorld(t)
	description := "О нас"

	updated, err := w.svc.Update(t.Context(), w.workspace, w.admin, WorkspaceUpdate{Description: &description})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Workspace.Name != "Команда Orbit" || updated.Workspace.Description != "О нас" {
		t.Errorf("workspace = %+v, want the old name and the new description", updated.Workspace)
	}

	empty := "   "
	cleared, err := w.svc.Update(t.Context(), w.workspace, w.owner, WorkspaceUpdate{Description: &empty})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if cleared.Workspace.Description != "" {
		t.Errorf("description = %q, want it cleared", cleared.Workspace.Description)
	}
	if got := APIWorkspace(cleared.Workspace).Description; got != nil {
		t.Errorf("API description = %q, want null", *got)
	}

	if _, err := w.svc.Update(t.Context(), w.workspace, w.owner, WorkspaceUpdate{}); !errors.Is(err, ErrNothingToUpdate) {
		t.Errorf("empty update: err = %v, want ErrNothingToUpdate", err)
	}

	// Both changes reached the watchers, in the API shape.
	published := watchers.updates()
	if len(published) != 2 {
		t.Fatalf("workspace.updated events = %d, want 2", len(published))
	}
	payload, ok := published[0].Payload.(api.Workspace)
	if !ok || published[0].WorkspaceID != w.workspace || payload.Description == nil || *payload.Description != "О нас" {
		t.Errorf("event = %+v, want the server with its new description", published[0])
	}
}

func TestProfileRules(t *testing.T) {
	description := "Описание"
	actions := map[string]func(w world, caller uuid.UUID) error{
		"change the description": func(w world, caller uuid.UUID) error {
			_, err := w.svc.Update(context.Background(), w.workspace, caller, WorkspaceUpdate{Description: &description})
			return err
		},
		"set the icon": func(w world, caller uuid.UUID) error {
			_, err := w.svc.SetIcon(context.Background(), w.workspace, caller, bytes.NewReader(pngBytes(100)))
			return err
		},
		"remove the icon": func(w world, caller uuid.UUID) error {
			_, err := w.svc.RemoveIcon(context.Background(), w.workspace, caller)
			return err
		},
	}
	want := map[string]error{"owner": nil, "admin": nil, "member": ErrForbidden, "outsider": ErrNotMember}

	for name, run := range actions {
		for role, expected := range want {
			t.Run(name+" as "+role, func(t *testing.T) {
				w, _, _ := newProfileWorld(t)
				caller := map[string]uuid.UUID{
					"owner": w.owner, "admin": w.admin, "member": w.member, "outsider": w.outsider,
				}[role]
				err := run(w, caller)
				switch {
				case expected == nil && err != nil:
					t.Errorf("err = %v, want it allowed", err)
				case expected != nil && !errors.Is(err, expected):
					t.Errorf("err = %v, want %v", err, expected)
				}
			})
		}
	}
}

func TestIconTypeComesFromTheBytes(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want error
	}{
		{"png", pngBytes(200), nil},
		{"jpeg", append([]byte("\xff\xd8\xff\xe0"), bytes.Repeat([]byte{1}, 100)...), nil},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{1}, 100)...), nil},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), ErrIconType},
		{"html", []byte("<!DOCTYPE html><html><script>alert(1)</script></html>"), ErrIconType},
		{"gif", []byte("GIF89a" + strings.Repeat("\x00", 50)), ErrIconType},
		{"plain text", []byte("not an image at all"), ErrIconType},
		{"empty", nil, ErrIconEmpty},
		{"over the limit", pngBytes(iconLimit + 1), ErrIconTooLarge},
		{"exactly the limit", pngBytes(iconLimit), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, storage, _ := newProfileWorld(t)
			_, err := w.svc.SetIcon(t.Context(), w.workspace, w.owner, bytes.NewReader(tc.body))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			wantObjects := 0
			if tc.want == nil {
				wantObjects = 1
			}
			if storage.count() != wantObjects {
				t.Errorf("objects stored = %d, want %d", storage.count(), wantObjects)
			}
		})
	}
}

func TestIconLifecycle(t *testing.T) {
	w, storage, watchers := newProfileWorld(t)

	first, err := w.svc.SetIcon(t.Context(), w.workspace, w.admin, bytes.NewReader(pngBytes(100)))
	if err != nil {
		t.Fatalf("SetIcon: %v", err)
	}
	if first.Workspace.IconURL == "" || !strings.HasSuffix(first.Workspace.IconKey, ".png") {
		t.Fatalf("workspace = %+v, want a signed link to a .png", first.Workspace)
	}

	// A member sees the link; an outsider sees nothing at all.
	seen, err := w.svc.Get(t.Context(), w.workspace, w.member)
	if err != nil || seen.Workspace.IconURL == "" {
		t.Errorf("member: url = %q, err = %v, want a link", seen.Workspace.IconURL, err)
	}
	if _, err := w.svc.Get(t.Context(), w.workspace, w.outsider); !errors.Is(err, ErrNotMember) {
		t.Errorf("outsider: err = %v, want ErrNotMember", err)
	}
	listed, err := w.svc.List(t.Context(), w.member)
	if err != nil || len(listed) != 1 || listed[0].Workspace.IconURL == "" {
		t.Errorf("list = %+v, err = %v, want the link there too", listed, err)
	}

	// A new icon replaces the old object rather than piling up.
	second, err := w.svc.SetIcon(t.Context(), w.workspace, w.owner, bytes.NewReader(pngBytes(120)))
	if err != nil {
		t.Fatalf("SetIcon again: %v", err)
	}
	if second.Workspace.IconKey == first.Workspace.IconKey || storage.count() != 1 {
		t.Errorf("key = %q (was %q), objects = %d, want a fresh key and one object",
			second.Workspace.IconKey, first.Workspace.IconKey, storage.count())
	}

	removed, err := w.svc.RemoveIcon(t.Context(), w.workspace, w.owner)
	if err != nil {
		t.Fatalf("RemoveIcon: %v", err)
	}
	if removed.Workspace.IconURL != "" || storage.count() != 0 {
		t.Errorf("url = %q, objects = %d, want no icon left", removed.Workspace.IconURL, storage.count())
	}
	// Removing again is fine and tells nobody.
	if _, err := w.svc.RemoveIcon(t.Context(), w.workspace, w.owner); err != nil {
		t.Errorf("RemoveIcon twice: %v", err)
	}

	if got := len(watchers.updates()); got != 3 {
		t.Errorf("workspace.updated events = %d, want 3 (two uploads, one removal)", got)
	}
}

func TestDeletingTheServerDropsItsIcon(t *testing.T) {
	w, storage, _ := newProfileWorld(t)
	if _, err := w.svc.SetIcon(t.Context(), w.workspace, w.owner, bytes.NewReader(pngBytes(100))); err != nil {
		t.Fatalf("SetIcon: %v", err)
	}
	if err := w.svc.Delete(t.Context(), w.workspace, w.owner); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if storage.count() != 0 {
		t.Errorf("objects = %d, want the icon gone with the server", storage.count())
	}
}

// The icon and the description of one server never show up on another.
func TestProfileIsPerServer(t *testing.T) {
	w, _, watchers := newProfileWorld(t)
	other, err := w.svc.Create(t.Context(), NewWorkspace{Name: "Другая"}, w.outsider)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.svc.SetIcon(t.Context(), w.workspace, w.owner, bytes.NewReader(pngBytes(100))); err != nil {
		t.Fatalf("SetIcon: %v", err)
	}

	// The owner here is nobody there.
	if _, err := w.svc.SetIcon(t.Context(), other.Workspace.ID, w.owner, bytes.NewReader(pngBytes(100))); !errors.Is(err, ErrNotMember) {
		t.Errorf("icon on a foreign server: err = %v, want ErrNotMember", err)
	}
	seen, err := w.svc.Get(t.Context(), other.Workspace.ID, w.outsider)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if seen.Workspace.IconURL != "" {
		t.Errorf("the other server shows an icon: %q", seen.Workspace.IconURL)
	}
	for _, event := range watchers.updates() {
		if event.WorkspaceID != w.workspace {
			t.Errorf("event went to server %v, want only %v", event.WorkspaceID, w.workspace)
		}
	}
}

func TestIconsOffWithoutStorage(t *testing.T) {
	w := newWorld(t)
	if _, err := w.svc.SetIcon(t.Context(), w.workspace, w.owner, bytes.NewReader(pngBytes(10))); !errors.Is(err, ErrIconsUnavailable) {
		t.Errorf("err = %v, want ErrIconsUnavailable", err)
	}
}
