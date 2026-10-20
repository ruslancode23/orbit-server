package profile

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
)

// fakeAvatarStorage keeps the stored objects in memory and records deletions,
// so a test can assert that the old sizes are gone and the new ones decode.
type fakeAvatarStorage struct {
	mu      sync.Mutex
	puts    map[string][]byte
	deleted []string
	putErr  error
}

func newFakeAvatarStorage() *fakeAvatarStorage {
	return &fakeAvatarStorage{puts: map[string][]byte{}}
}

func (s *fakeAvatarStorage) Put(_ context.Context, key, _ string, body io.Reader, _ int64) error {
	if s.putErr != nil {
		return s.putErr
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts[key] = data
	return nil
}

func (s *fakeAvatarStorage) SignedInlineURL(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return "inline://" + key, nil
}

func (s *fakeAvatarStorage) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, key)
	delete(s.puts, key)
	return nil
}

func (s *fakeAvatarStorage) putKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.puts))
	for key := range s.puts {
		out = append(out, key)
	}
	return out
}

func (s *fakeAvatarStorage) deletedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

// avatarHarness is a service with an avatar store and an object store, sharing
// the in-memory profile rows of the ordinary harness.
func newAvatarHarness(t *testing.T, maxBytes int64) (*harness, *fakeAvatarStorage) {
	t.Helper()
	h := newHarness(t)
	storage := newFakeAvatarStorage()
	h.svc.SetAvatars(Avatars{Storage: storage, MaxBytes: maxBytes, LinkTTL: time.Hour})
	return h, storage
}

func pngAvatar(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x % 256), G: uint8(y % 256), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return buf.Bytes()
}

// pngBomb is a valid PNG header that claims 50000x50000 pixels and carries no
// image data: a few dozen bytes that a decoder would expand into gigabytes.
func pngBomb(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("\x89PNG\r\n\x1a\n")

	body := make([]byte, 13)
	binary.BigEndian.PutUint32(body[0:4], 50000)
	binary.BigEndian.PutUint32(body[4:8], 50000)
	body[8] = 8 // bit depth
	body[9] = 6 // truecolour with alpha
	chunk := append([]byte("IHDR"), body...)

	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(body)))
	buf.Write(length[:])
	buf.Write(chunk)
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.ChecksumIEEE(chunk))
	buf.Write(crc[:])
	return buf.Bytes()
}

func TestAvatarRejectsAMisleadingType(t *testing.T) {
	h, _ := newAvatarHarness(t, 8<<20)
	user := uuid.New()
	h.seed(user, "anna")

	// The bytes say HTML whatever extension or content type the client sent.
	_, err := h.svc.SetAvatar(t.Context(), user, strings.NewReader("<html><script>alert(1)</script>"))
	if !errors.Is(err, ErrAvatarType) {
		t.Fatalf("err = %v, want ErrAvatarType", err)
	}
}

func TestAvatarRejectsADecompressionBombBeforeDecoding(t *testing.T) {
	h, storage := newAvatarHarness(t, 8<<20)
	user := uuid.New()
	h.seed(user, "anna")

	_, err := h.svc.SetAvatar(t.Context(), user, bytes.NewReader(pngBomb(t)))
	if !errors.Is(err, ErrAvatarDimensions) {
		t.Fatalf("err = %v, want ErrAvatarDimensions", err)
	}
	if len(storage.putKeys()) != 0 {
		t.Error("an object was stored for a rejected image")
	}
}

func TestAvatarRejectsAFileOverTheLimit(t *testing.T) {
	h, _ := newAvatarHarness(t, 1024)
	user := uuid.New()
	h.seed(user, "anna")

	data := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 2048)...)
	_, err := h.svc.SetAvatar(t.Context(), user, bytes.NewReader(data))
	if !errors.Is(err, ErrAvatarTooLarge) {
		t.Fatalf("err = %v, want ErrAvatarTooLarge", err)
	}
}

func TestAvatarRejectsAnEmptyFile(t *testing.T) {
	h, _ := newAvatarHarness(t, 8<<20)
	user := uuid.New()
	h.seed(user, "anna")

	if _, err := h.svc.SetAvatar(t.Context(), user, strings.NewReader("")); !errors.Is(err, ErrAvatarEmpty) {
		t.Fatalf("err = %v, want ErrAvatarEmpty", err)
	}
}

func TestAvatarReplacementStoresBothSizesAndDropsTheOldOne(t *testing.T) {
	h, storage := newAvatarHarness(t, 8<<20)
	user := uuid.New()
	seed := h.seed(user, "anna")
	seed.AvatarKey = "avatars/old"
	h.store.profiles[user] = seed

	updated, err := h.svc.SetAvatar(t.Context(), user, bytes.NewReader(pngAvatar(t, 400, 200)))
	if err != nil {
		t.Fatalf("SetAvatar: %v", err)
	}
	stored, _ := h.store.Profile(t.Context(), user)
	if stored.AvatarKey == "" || stored.AvatarKey == seed.AvatarKey {
		t.Fatalf("avatar key = %q, want a fresh key", stored.AvatarKey)
	}
	if updated.AvatarURL != "inline://"+avatarObjectKey(stored.AvatarKey, avatarLarge) {
		t.Errorf("avatarUrl = %q, want the large signed link", updated.AvatarURL)
	}

	// Both sizes are WebP of the promised dimensions.
	for _, size := range []int{avatarLarge, avatarSmall} {
		data, ok := storage.puts[avatarObjectKey(stored.AvatarKey, size)]
		if !ok {
			t.Fatalf("no object for size %d", size)
		}
		img, format, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decode size %d: %v", size, err)
		}
		if format != "webp" {
			t.Errorf("format = %q, want webp", format)
		}
		if got := img.Bounds(); got.Dx() != size || got.Dy() != size {
			t.Errorf("size %d image = %v, want %dx%d", size, got, size, size)
		}
	}

	// The old objects are gone, both sizes.
	deleted := storage.deletedKeys()
	for _, size := range []int{avatarLarge, avatarSmall} {
		key := avatarObjectKey(seed.AvatarKey, size)
		if !contains(deleted, key) {
			t.Errorf("old object %q was not deleted; deleted = %v", key, deleted)
		}
	}
}

func TestAvatarFailureKeepsTheOldOne(t *testing.T) {
	h, storage := newAvatarHarness(t, 8<<20)
	user := uuid.New()
	seed := h.seed(user, "anna")
	seed.AvatarKey = "avatars/old"
	h.store.profiles[user] = seed
	storage.putErr = errors.New("object store is down")

	if _, err := h.svc.SetAvatar(t.Context(), user, bytes.NewReader(pngAvatar(t, 20, 20))); err == nil {
		t.Fatal("the upload succeeded with a broken object store")
	}
	stored, _ := h.store.Profile(t.Context(), user)
	if stored.AvatarKey != seed.AvatarKey {
		t.Errorf("avatar key = %q, want the old one", stored.AvatarKey)
	}
	if contains(storage.deletedKeys(), avatarObjectKey(seed.AvatarKey, avatarLarge)) {
		t.Error("the old avatar was deleted after a failed upload")
	}
}

func TestClearAvatarRemovesBothObjects(t *testing.T) {
	h, storage := newAvatarHarness(t, 8<<20)
	user := uuid.New()
	seed := h.seed(user, "anna")
	seed.AvatarKey = "avatars/old"
	h.store.profiles[user] = seed

	updated, err := h.svc.ClearAvatar(t.Context(), user)
	if err != nil {
		t.Fatalf("ClearAvatar: %v", err)
	}
	if updated.AvatarURL != "" {
		t.Errorf("avatarUrl = %q, want none", updated.AvatarURL)
	}
	stored, _ := h.store.Profile(t.Context(), user)
	if stored.AvatarKey != "" {
		t.Errorf("avatar key = %q, want none", stored.AvatarKey)
	}
	for _, size := range []int{avatarLarge, avatarSmall} {
		if !contains(storage.deletedKeys(), avatarObjectKey(seed.AvatarKey, size)) {
			t.Errorf("object %q was not deleted", avatarObjectKey(seed.AvatarKey, size))
		}
	}
}

func TestAvatarChangeIsAnnounced(t *testing.T) {
	h, _ := newAvatarHarness(t, 8<<20)
	user := uuid.New()
	h.seed(user, "anna")

	if _, err := h.svc.SetAvatar(t.Context(), user, bytes.NewReader(pngAvatar(t, 20, 20))); err != nil {
		t.Fatalf("SetAvatar: %v", err)
	}
	h.svc.flush(t.Context())

	events := h.hub.personalEvents()
	if len(events) != 1 || events[0].userID != user {
		t.Fatalf("events = %+v, want one user.updated for the account", events)
	}
	if payload, ok := events[0].event.Payload.(api.UserProfile); !ok || payload.AvatarUrl == nil {
		t.Fatalf("payload = %+v, want a profile with an avatar link", events[0].event.Payload)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
