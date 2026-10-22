package feedback

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/HugoSmits86/nativewebp"
	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

type fakeStore struct {
	reports []Report
	err     error
}

func (s *fakeStore) Create(_ context.Context, r Report) (Report, error) {
	if s.err != nil {
		return Report{}, s.err
	}
	r.CreatedAt = time.Now()
	s.reports = append(s.reports, r)
	return r, nil
}

type fakeStorage struct {
	objects map[string][]byte
	types   map[string]string
	err     error
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{objects: map[string][]byte{}, types: map[string]string{}}
}

func (s *fakeStorage) Put(_ context.Context, key, mimeType string, body io.Reader, _ int64) error {
	if s.err != nil {
		return s.err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.objects[key] = data
	s.types[key] = mimeType
	return nil
}

func (s *fakeStorage) Delete(_ context.Context, key string) error {
	delete(s.objects, key)
	return nil
}

func testImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	return img
}

func encodePNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, testImage()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeWebP(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := nativewebp.Encode(&buf, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func validDraft() Draft {
	return Draft{
		Description: "  Не отправляется сообщение  ",
		Steps:       "1. Открыть канал\n2. Нажать Enter",
		AppVersion:  "0.1.0+abc123",
		UserAgent:   "Mozilla/5.0 (Macintosh) Safari/605.1.15",
		Route:       "/s/0d3c/7a1b",
	}
}

func TestSubmitStoresTheReport(t *testing.T) {
	store, storage := &fakeStore{}, newFakeStorage()
	svc := NewService(store, storage, 1<<20)
	userID := uuid.New()

	report, err := svc.Submit(t.Context(), userID, validDraft(), nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if report.ID == uuid.Nil || report.UserID != userID {
		t.Errorf("report ids = %v / %v, want a fresh id and the caller", report.ID, report.UserID)
	}
	if report.Description != "Не отправляется сообщение" {
		t.Errorf("description = %q, want it trimmed", report.Description)
	}
	if report.Screenshot != nil || len(storage.objects) != 0 {
		t.Errorf("a report without a screenshot stored an object: %+v", storage.objects)
	}
	if len(store.reports) != 1 {
		t.Errorf("stored %d reports, want 1", len(store.reports))
	}
}

func TestSubmitChecksFields(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Draft)
		field  string
	}{
		{"blank description", func(d *Draft) { d.Description = " \n\t " }, "description"},
		{"description too long", func(d *Draft) { d.Description = strings.Repeat("я", maxDescription+1) }, "description"},
		{"steps too long", func(d *Draft) { d.Steps = strings.Repeat("ш", maxSteps+1) }, "steps"},
		{"no app version", func(d *Draft) { d.AppVersion = "" }, "appVersion"},
		{"app version too long", func(d *Draft) { d.AppVersion = strings.Repeat("1", maxAppVersion+1) }, "appVersion"},
		{"user agent too long", func(d *Draft) { d.UserAgent = strings.Repeat("a", maxUserAgent+1) }, "userAgent"},
		{"route is not a path", func(d *Draft) { d.Route = "https://example.test/s/1" }, "route"},
		{"route is only a query", func(d *Draft) { d.Route = "?token=1" }, "route"},
		{"route too long", func(d *Draft) { d.Route = "/" + strings.Repeat("a", maxRoute) }, "route"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}
			svc := NewService(store, newFakeStorage(), 1<<20)
			draft := validDraft()
			tt.change(&draft)

			_, err := svc.Submit(t.Context(), uuid.New(), draft, nil)
			var field *FieldError
			if !errors.As(err, &field) || field.Field != tt.field {
				t.Fatalf("Submit error = %v, want a FieldError about %q", err, tt.field)
			}
			if !strings.HasPrefix(field.Error(), tt.field+": ") {
				t.Errorf("message %q does not start with the field name", field.Error())
			}
			if len(store.reports) != 0 {
				t.Error("a refused report was stored")
			}
		})
	}
}

func TestSubmitLimitsTextByCharactersNotBytes(t *testing.T) {
	svc := NewService(&fakeStore{}, newFakeStorage(), 1<<20)
	draft := validDraft()
	// 4000 Cyrillic letters are 8000 bytes and still fit.
	draft.Description = strings.Repeat("я", maxDescription)
	if _, err := svc.Submit(t.Context(), uuid.New(), draft, nil); err != nil {
		t.Errorf("Submit of exactly %d characters: %v", maxDescription, err)
	}
}

func TestSubmitDropsQueryAndFragmentFromRoute(t *testing.T) {
	tests := []struct {
		route string
		want  string
	}{
		{"/invite/abc?code=secret", "/invite/abc"},
		{"/s/1/2#token=secret", "/s/1/2"},
		{"/s/1/2?x=1#y", "/s/1/2"},
		{"/settings", "/settings"},
	}
	for _, tt := range tests {
		t.Run(tt.route, func(t *testing.T) {
			svc := NewService(&fakeStore{}, newFakeStorage(), 1<<20)
			draft := validDraft()
			draft.Route = tt.route
			report, err := svc.Submit(t.Context(), uuid.New(), draft, nil)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if report.Route != tt.want {
				t.Errorf("route = %q, want %q", report.Route, tt.want)
			}
		})
	}
}

func TestSubmitScreenshot(t *testing.T) {
	tests := []struct {
		name     string
		data     func(*testing.T) []byte
		wantType string
		wantExt  string
		wantErr  error
		field    bool
	}{
		{name: "png", data: encodePNG, wantType: "image/png", wantExt: ".png"},
		{name: "jpeg", data: encodeJPEG, wantType: "image/jpeg", wantExt: ".jpg"},
		{name: "webp", data: encodeWebP, wantType: "image/webp", wantExt: ".webp"},
		{name: "gif", data: func(*testing.T) []byte { return []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;") }, wantErr: ErrScreenshotType},
		{name: "svg", data: func(*testing.T) []byte { return []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`) }, wantErr: ErrScreenshotType},
		{name: "html named png", data: func(*testing.T) []byte { return []byte("<html><script>alert(1)</script></html>") }, wantErr: ErrScreenshotType},
		{name: "png signature over garbage", data: func(*testing.T) []byte { return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 64)...) }, wantErr: ErrScreenshotType},
		{name: "empty", data: func(*testing.T) []byte { return []byte{} }, field: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, storage := &fakeStore{}, newFakeStorage()
			svc := NewService(store, storage, 1<<20)

			report, err := svc.Submit(t.Context(), uuid.New(), validDraft(), tt.data(t))
			if tt.field {
				var field *FieldError
				if !errors.As(err, &field) || field.Field != "screenshot" {
					t.Fatalf("Submit error = %v, want a FieldError about the screenshot", err)
				}
				return
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Submit error = %v, want %v", err, tt.wantErr)
				}
				if len(storage.objects) != 0 || len(store.reports) != 0 {
					t.Error("a refused screenshot left something behind")
				}
				return
			}
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			shot := report.Screenshot
			if shot == nil {
				t.Fatal("the report has no screenshot")
			}
			if shot.MimeType != tt.wantType || !strings.HasSuffix(shot.Key, tt.wantExt) ||
				!strings.HasPrefix(shot.Key, "bug-reports/"+report.ID.String()) {
				t.Errorf("screenshot = %+v, want %s under bug-reports/<id>%s", shot, tt.wantType, tt.wantExt)
			}
			if storage.types[shot.Key] != tt.wantType || int64(len(storage.objects[shot.Key])) != shot.SizeBytes {
				t.Errorf("stored object = %q, %d bytes; want %s, %d bytes",
					storage.types[shot.Key], len(storage.objects[shot.Key]), tt.wantType, shot.SizeBytes)
			}
		})
	}
}

func TestReadScreenshotStopsAtTheLimit(t *testing.T) {
	svc := NewService(&fakeStore{}, newFakeStorage(), 10)
	if _, err := svc.ReadScreenshot(bytes.NewReader(make([]byte, 11))); !errors.Is(err, ErrScreenshotTooLarge) {
		t.Errorf("11 bytes over a 10-byte limit: %v, want ErrScreenshotTooLarge", err)
	}
	data, err := svc.ReadScreenshot(bytes.NewReader(make([]byte, 10)))
	if err != nil || len(data) != 10 {
		t.Errorf("exactly the limit: %d bytes, %v", len(data), err)
	}
}

func TestSubmitTakesTheObjectBackWhenTheRowFails(t *testing.T) {
	storage := newFakeStorage()
	svc := NewService(&fakeStore{err: errors.New("db down")}, storage, 1<<20)

	if _, err := svc.Submit(t.Context(), uuid.New(), validDraft(), encodePNG(t)); err == nil {
		t.Fatal("Submit succeeded with a failing store")
	}
	if len(storage.objects) != 0 {
		t.Errorf("objects left behind: %d", len(storage.objects))
	}
}

func TestSubmitStorageFailureStoresNoRow(t *testing.T) {
	store, storage := &fakeStore{}, newFakeStorage()
	storage.err = errors.New("s3 down")
	svc := NewService(store, storage, 1<<20)

	if _, err := svc.Submit(t.Context(), uuid.New(), validDraft(), encodePNG(t)); err == nil {
		t.Fatal("Submit succeeded with a failing object store")
	}
	if len(store.reports) != 0 {
		t.Error("a row was stored without its screenshot")
	}
}

func TestSubmitLimit(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store, newFakeStorage(), 1<<20)
	svc.SetLimiter(ratelimit.NewHourly(2))
	anna, bob := uuid.New(), uuid.New()

	// A refused draft spends nothing.
	bad := validDraft()
	bad.Description = ""
	if _, err := svc.Submit(t.Context(), anna, bad, nil); err == nil {
		t.Fatal("blank description accepted")
	}
	for i := range 2 {
		if _, err := svc.Submit(t.Context(), anna, validDraft(), nil); err != nil {
			t.Fatalf("report %d: %v", i+1, err)
		}
	}

	if err := svc.CheckLimit(anna); err == nil {
		t.Error("CheckLimit allowed a third report")
	}
	_, err := svc.Submit(t.Context(), anna, validDraft(), nil)
	var limited *LimitedError
	if !errors.As(err, &limited) || limited.RetryAfter <= 0 {
		t.Fatalf("third report: %v, want a LimitedError with a wait", err)
	}
	if _, err := svc.Submit(t.Context(), bob, validDraft(), nil); err != nil {
		t.Errorf("another account shares the budget: %v", err)
	}
	if len(store.reports) != 3 {
		t.Errorf("stored %d reports, want 3", len(store.reports))
	}
}
