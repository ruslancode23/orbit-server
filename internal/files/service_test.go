package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

type fakeStore struct {
	attachments map[uuid.UUID]*Attachment
}

func newFakeStore() *fakeStore { return &fakeStore{attachments: map[uuid.UUID]*Attachment{}} }

func (s *fakeStore) Create(_ context.Context, a Attachment) (Attachment, error) {
	a.ID = uuid.New()
	a.CreatedAt = time.Now()
	s.attachments[a.ID] = &a
	return a, nil
}

func (s *fakeStore) Attachment(_ context.Context, id uuid.UUID) (Attachment, error) {
	a, ok := s.attachments[id]
	if !ok {
		return Attachment{}, ErrNotFound
	}
	return *a, nil
}

func (s *fakeStore) Attach(_ context.Context, id, messageID, uploaderID uuid.UUID) (Attachment, error) {
	a, ok := s.attachments[id]
	if !ok || a.UploaderID != uploaderID || a.MessageID != nil {
		return Attachment{}, ErrNotFound
	}
	a.MessageID = &messageID
	return *a, nil
}

func (s *fakeStore) ByMessages(_ context.Context, messageIDs []uuid.UUID) ([]Attachment, error) {
	wanted := make(map[uuid.UUID]bool, len(messageIDs))
	for _, id := range messageIDs {
		wanted[id] = true
	}
	var out []Attachment
	for _, a := range s.attachments {
		if a.MessageID != nil && wanted[*a.MessageID] {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (s *fakeStore) Orphans(_ context.Context, olderThan time.Time, limit int) ([]Attachment, error) {
	var out []Attachment
	for _, a := range s.attachments {
		if a.MessageID == nil && a.CreatedAt.Before(olderThan) {
			out = append(out, *a)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *fakeStore) Delete(_ context.Context, id uuid.UUID) error {
	if a, ok := s.attachments[id]; ok && a.MessageID == nil {
		delete(s.attachments, id)
	}
	return nil
}

// fakeStorage keeps the bytes in memory and records what was asked of it.
type fakeStorage struct {
	objects map[string][]byte
	deleted []string
	putErr  error
}

func newFakeStorage() *fakeStorage { return &fakeStorage{objects: map[string][]byte{}} }

func (s *fakeStorage) Put(_ context.Context, key, _ string, body io.Reader, _ int64) error {
	if s.putErr != nil {
		return s.putErr
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.objects[key] = data
	return nil
}

func (s *fakeStorage) SignedURL(_ context.Context, key, _ string, ttl time.Duration) (string, error) {
	return "https://store.example/" + key + "?expires=" + ttl.String(), nil
}

func (s *fakeStorage) Delete(_ context.Context, key string) error {
	s.deleted = append(s.deleted, key)
	delete(s.objects, key)
	return nil
}

type fakeChannels struct {
	channel channels.Channel
	readers map[uuid.UUID]bool
	writers map[uuid.UUID]bool
}

func (c *fakeChannels) Get(_ context.Context, channelID, callerID uuid.UUID) (channels.View, error) {
	if channelID != c.channel.ID || !c.readers[callerID] {
		return channels.View{}, channels.ErrNotFound
	}
	return channels.View{Channel: c.channel, CanWrite: c.writers[callerID]}, nil
}

func (c *fakeChannels) MayWrite(ctx context.Context, channelID, callerID uuid.UUID) (channels.Channel, error) {
	view, err := c.Get(ctx, channelID, callerID)
	if err != nil {
		return channels.Channel{}, err
	}
	if !view.CanWrite {
		return channels.Channel{}, channels.ErrForbidden
	}
	return view.Channel, nil
}

type fakeMessages struct{ channelOf map[uuid.UUID]uuid.UUID }

func (m *fakeMessages) ChannelOf(_ context.Context, messageID uuid.UUID) (uuid.UUID, error) {
	channelID, ok := m.channelOf[messageID]
	if !ok {
		return uuid.Nil, errors.New("no such message")
	}
	return channelID, nil
}

// fakePermissions answers like internal/servers: a role per (server, account)
// and the server's own member permissions.
type fakePermissions struct {
	roles map[uuid.UUID]map[uuid.UUID]servers.Role
	perms map[uuid.UUID]servers.Permissions
}

func (p *fakePermissions) RequirePermission(_ context.Context, workspaceID, userID uuid.UUID, permission servers.Permission) (servers.Role, error) {
	role, ok := p.roles[workspaceID][userID]
	if !ok {
		return "", servers.ErrNotMember
	}
	if !p.perms[workspaceID].Allows(role, permission) {
		return role, servers.ErrForbidden
	}
	return role, nil
}

type world struct {
	svc      *Service
	access   *fakeChannels
	perms    *fakePermissions
	store    *fakeStore
	storage  *fakeStorage
	messages *fakeMessages
	channel  uuid.UUID
	anna     uuid.UUID
	reader   uuid.UUID
	outsider uuid.UUID
}

const maxBytes = 1024

func newWorld(t *testing.T) world {
	t.Helper()
	w := world{
		store: newFakeStore(), storage: newFakeStorage(),
		messages: &fakeMessages{channelOf: map[uuid.UUID]uuid.UUID{}},
		channel:  uuid.New(), anna: uuid.New(), reader: uuid.New(), outsider: uuid.New(),
	}
	workspaceID := uuid.New()
	access := &fakeChannels{
		channel: channels.Channel{ID: w.channel, WorkspaceID: workspaceID, Name: "общий"},
		readers: map[uuid.UUID]bool{w.anna: true, w.reader: true},
		writers: map[uuid.UUID]bool{w.anna: true},
	}
	w.access = access
	w.perms = &fakePermissions{
		roles: map[uuid.UUID]map[uuid.UUID]servers.Role{
			workspaceID: {w.anna: servers.RoleMember, w.reader: servers.RoleMember},
		},
		perms: map[uuid.UUID]servers.Permissions{workspaceID: servers.DefaultPermissions()},
	}
	w.svc = NewService(w.store, w.storage, access, w.messages, w.perms, maxBytes, time.Minute)
	return w
}

// A PNG header: enough for http.DetectContentType to recognise the kind.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)

func TestUploadStoresTheFileAndReadsItsRealType(t *testing.T) {
	w := newWorld(t)

	// The name says .txt, the bytes say PNG. The bytes win.
	attachment, err := w.svc.Upload(t.Context(), w.channel, w.anna, "картинка.txt", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if attachment.MimeType != "image/png" {
		t.Errorf("mimeType = %q, want image/png: the type must come from the content", attachment.MimeType)
	}
	if attachment.SizeBytes != int64(len(pngBytes)) {
		t.Errorf("size = %d, want %d", attachment.SizeBytes, len(pngBytes))
	}
	if attachment.MessageID != nil {
		t.Error("a fresh upload already belongs to a message")
	}
	if !strings.Contains(attachment.StorageKey, w.channel.String()) {
		t.Errorf("storageKey = %q, want it under the channel", attachment.StorageKey)
	}
	if string(w.storage.objects[attachment.StorageKey]) != string(pngBytes) {
		t.Error("the stored bytes differ from what was uploaded")
	}
}

func TestUploadRefusesWhatABrowserWouldRun(t *testing.T) {
	w := newWorld(t)

	for _, tc := range []struct{ name, body string }{
		{"html", "<!DOCTYPE html><html><body>привет</body></html>"},
		{"svg", `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.svc.Upload(t.Context(), w.channel, w.anna, "harmless.png", strings.NewReader(tc.body))
			if !errors.Is(err, ErrUnsupportedType) {
				t.Errorf("err = %v, want ErrUnsupportedType", err)
			}
			if len(w.storage.objects) != 0 {
				t.Error("the refused file was stored anyway")
			}
		})
	}
}

func TestUploadRefusesTooLargeAndEmpty(t *testing.T) {
	w := newWorld(t)

	t.Run("too large", func(t *testing.T) {
		big := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{1}, maxBytes)...)
		if _, err := w.svc.Upload(t.Context(), w.channel, w.anna, "big.png", bytes.NewReader(big)); !errors.Is(err, ErrTooLarge) {
			t.Errorf("err = %v, want ErrTooLarge", err)
		}
		if len(w.storage.objects) != 0 {
			t.Error("the oversized file was stored")
		}
	})

	t.Run("empty", func(t *testing.T) {
		if _, err := w.svc.Upload(t.Context(), w.channel, w.anna, "empty.txt", strings.NewReader("")); !errors.Is(err, ErrEmpty) {
			t.Errorf("err = %v, want ErrEmpty", err)
		}
	})
}

func TestUploadNeedsTheRightToWrite(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.Upload(t.Context(), w.channel, w.reader, "f.png", bytes.NewReader(pngBytes)); !errors.Is(err, ErrForbidden) {
		t.Errorf("as a reader: err = %v, want ErrForbidden", err)
	}
	if _, err := w.svc.Upload(t.Context(), w.channel, w.outsider, "f.png", bytes.NewReader(pngBytes)); !errors.Is(err, ErrNotFound) {
		t.Errorf("as an outsider: err = %v, want ErrNotFound", err)
	}
}

// A failed write to the store must not leave a row pointing at nothing, and a
// failed row must not leave bytes nobody can reach.
func TestUploadCleansUpAfterItself(t *testing.T) {
	w := newWorld(t)
	w.storage.putErr = errors.New("store is down")

	if _, err := w.svc.Upload(t.Context(), w.channel, w.anna, "f.png", bytes.NewReader(pngBytes)); err == nil {
		t.Fatal("err = nil, want the store's error")
	}
	if len(w.store.attachments) != 0 {
		t.Error("a row was written although the bytes were not")
	}
}

// Only someone who may read the channel gets a link, and only for a while.
func TestLinkIsIssuedOnlyToTheChannelsReaders(t *testing.T) {
	w := newWorld(t)
	attachment, err := w.svc.Upload(t.Context(), w.channel, w.anna, "f.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	messageID := uuid.New()
	w.messages.channelOf[messageID] = w.channel
	if _, err := w.svc.AttachAll(t.Context(), messageID, w.anna, []uuid.UUID{attachment.ID}); err != nil {
		t.Fatalf("AttachAll: %v", err)
	}

	link, err := w.svc.Link(t.Context(), attachment.ID, w.reader)
	if err != nil {
		t.Fatalf("Link for a reader: %v", err)
	}
	if link.URL == "" || !link.ExpiresAt.After(time.Now()) {
		t.Errorf("link = %+v, want a URL that expires later", link)
	}

	if _, err := w.svc.Link(t.Context(), attachment.ID, w.outsider); !errors.Is(err, ErrNotFound) {
		t.Errorf("for an outsider: err = %v, want ErrNotFound", err)
	}
}

// A file not yet on a message is nobody's business but its uploader's.
func TestUnattachedFileIsVisibleOnlyToItsUploader(t *testing.T) {
	w := newWorld(t)
	attachment, err := w.svc.Upload(t.Context(), w.channel, w.anna, "f.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if _, err := w.svc.Link(t.Context(), attachment.ID, w.anna); err != nil {
		t.Errorf("the uploader cannot reach their own file: %v", err)
	}
	if _, err := w.svc.Link(t.Context(), attachment.ID, w.reader); !errors.Is(err, ErrNotFound) {
		t.Errorf("another member of the channel: err = %v, want ErrNotFound", err)
	}
}

func TestAttachAllTakesOnlyYourOwnFreeFiles(t *testing.T) {
	w := newWorld(t)
	mine, err := w.svc.Upload(t.Context(), w.channel, w.anna, "mine.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// A file of somebody else, and one already used by another message.
	foreign, err := w.store.Create(t.Context(), Attachment{UploaderID: w.reader, StorageKey: "k2", FileName: "theirs.png"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	used, err := w.svc.Upload(t.Context(), w.channel, w.anna, "used.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := w.svc.AttachAll(t.Context(), uuid.New(), w.anna, []uuid.UUID{used.ID}); err != nil {
		t.Fatalf("AttachAll: %v", err)
	}

	attached, err := w.svc.AttachAll(t.Context(), uuid.New(), w.anna, []uuid.UUID{mine.ID, foreign.ID, used.ID})
	if err != nil {
		t.Fatalf("AttachAll: %v", err)
	}
	if len(attached) != 1 || attached[0].ID != mine.ID {
		t.Errorf("attached = %+v, want only the uploader's free file", attached)
	}
}

// Files whose message never arrived go away, bytes first, row second.
func TestSweepRemovesOrphansOnly(t *testing.T) {
	w := newWorld(t)
	orphan, err := w.svc.Upload(t.Context(), w.channel, w.anna, "orphan.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	kept, err := w.svc.Upload(t.Context(), w.channel, w.anna, "kept.png", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	messageID := uuid.New()
	if _, err := w.svc.AttachAll(t.Context(), messageID, w.anna, []uuid.UUID{kept.ID}); err != nil {
		t.Fatalf("AttachAll: %v", err)
	}
	// Both were made now; age them so the sweep sees the orphan.
	w.store.attachments[orphan.ID].CreatedAt = time.Now().Add(-48 * time.Hour)
	w.store.attachments[kept.ID].CreatedAt = time.Now().Add(-48 * time.Hour)

	removed, err := w.svc.SweepOrphans(t.Context(), 24*time.Hour, 100)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := w.store.Attachment(t.Context(), orphan.ID); !errors.Is(err, ErrNotFound) {
		t.Error("the orphan row is still there")
	}
	if _, ok := w.storage.objects[orphan.StorageKey]; ok {
		t.Error("the orphan bytes are still there")
	}
	if _, err := w.store.Attachment(t.Context(), kept.ID); err != nil {
		t.Errorf("the attached file was swept: %v", err)
	}
}

// A file uploaded a minute ago is not swept: its message may still be coming.
func TestSweepLeavesFreshUploadsAlone(t *testing.T) {
	w := newWorld(t)
	if _, err := w.svc.Upload(t.Context(), w.channel, w.anna, "fresh.png", bytes.NewReader(pngBytes)); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	removed, err := w.svc.SweepOrphans(t.Context(), 24*time.Hour, 100)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

func TestFileNameIsKeptSaneAndTheKeyIsNot(t *testing.T) {
	w := newWorld(t)

	attachment, err := w.svc.Upload(t.Context(), w.channel, w.anna, "../../etc/passwd", bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if attachment.FileName != "passwd" {
		t.Errorf("fileName = %q, want the path stripped", attachment.FileName)
	}
	if strings.Contains(attachment.StorageKey, "..") {
		t.Errorf("storageKey = %q, want no path tricks", attachment.StorageKey)
	}
}
