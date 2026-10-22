package feedback

import (
	"bytes"
	"context"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

type formPart struct {
	name, fileName string
	data           []byte
}

func formRequest(t *testing.T, parts []formPart) api.CreateBugReportRequestObject {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, part := range parts {
		var err error
		if part.fileName != "" {
			w, createErr := form.CreateFormFile(part.name, part.fileName)
			if createErr != nil {
				t.Fatal(createErr)
			}
			_, err = w.Write(part.data)
		} else {
			err = form.WriteField(part.name, string(part.data))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	return api.CreateBugReportRequestObject{Body: multipart.NewReader(&body, form.Boundary())}
}

func textParts() []formPart {
	return []formPart{
		{name: "description", data: []byte("Секретный текст жалобы")},
		{name: "steps", data: []byte("Открыть настройки")},
		{name: "appVersion", data: []byte("0.1.0")},
		{name: "userAgent", data: []byte("Mozilla/5.0")},
		{name: "route", data: []byte("/settings?token=abc")},
	}
}

func signedIn(ctx context.Context, userID uuid.UUID) context.Context {
	return auth.WithSession(ctx, auth.Session{ID: uuid.New(), User: auth.User{ID: userID}})
}

// status runs the response through its writer, the way the router would.
func status(t *testing.T, resp api.CreateBugReportResponseObject) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := resp.VisitCreateBugReportResponse(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestCreateBugReportHandler(t *testing.T) {
	png := encodePNG(t)
	tests := []struct {
		name    string
		parts   func() []formPart
		maxShot int64
		status  int
		message string
	}{
		{
			name:   "text only",
			parts:  textParts,
			status: http.StatusCreated,
		},
		{
			name: "screenshot first, then the text",
			parts: func() []formPart {
				return append([]formPart{{name: "screenshot", fileName: "shot.png", data: png}}, textParts()...)
			},
			status: http.StatusCreated,
		},
		{
			name: "no description",
			parts: func() []formPart {
				return textParts()[1:]
			},
			status:  http.StatusBadRequest,
			message: "description: ",
		},
		{
			name: "unknown field",
			parts: func() []formPart {
				return append(textParts(), formPart{name: "messages", data: []byte("чужая переписка")})
			},
			status:  http.StatusBadRequest,
			message: "messages: ",
		},
		{
			name: "screenshot over the limit",
			parts: func() []formPart {
				return append(textParts(), formPart{name: "screenshot", fileName: "big.png", data: png})
			},
			maxShot: int64(len(png) - 1),
			status:  http.StatusRequestEntityTooLarge,
			message: "screenshot: ",
		},
		{
			name: "screenshot that is not an image",
			parts: func() []formPart {
				return append(textParts(), formPart{name: "screenshot", fileName: "shot.png", data: []byte("<html></html>")})
			},
			status:  http.StatusUnsupportedMediaType,
			message: "screenshot: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maxShot := tt.maxShot
			if maxShot == 0 {
				maxShot = 1 << 20
			}
			store := &fakeStore{}
			h := NewHandler(NewService(store, newFakeStorage(), maxShot), slog.New(slog.DiscardHandler))

			resp, err := h.CreateBugReport(signedIn(t.Context(), uuid.New()), formRequest(t, tt.parts()))
			if err != nil {
				t.Fatal(err)
			}
			rec := status(t, resp)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
			}
			if tt.message != "" && !strings.Contains(rec.Body.String(), `"message":"`+tt.message) {
				t.Errorf("body %s, want a message starting with %q", rec.Body, tt.message)
			}
			if tt.status == http.StatusCreated && len(store.reports) != 1 {
				t.Errorf("stored %d reports, want 1", len(store.reports))
			}
		})
	}
}

func TestCreateBugReportNeedsASession(t *testing.T) {
	store := &fakeStore{}
	h := NewHandler(NewService(store, newFakeStorage(), 1<<20), slog.New(slog.DiscardHandler))

	resp, err := h.CreateBugReport(t.Context(), formRequest(t, textParts()))
	if err != nil {
		t.Fatal(err)
	}
	if rec := status(t, resp); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if len(store.reports) != 0 {
		t.Error("a report was stored without a session")
	}
}

func TestCreateBugReportOverTheLimit(t *testing.T) {
	svc := NewService(&fakeStore{}, newFakeStorage(), 1<<20)
	svc.SetLimiter(ratelimit.NewHourly(1))
	h := NewHandler(svc, slog.New(slog.DiscardHandler))
	ctx := signedIn(t.Context(), uuid.New())

	if _, err := h.CreateBugReport(ctx, formRequest(t, textParts())); err != nil {
		t.Fatal(err)
	}
	resp, err := h.CreateBugReport(ctx, formRequest(t, textParts()))
	if err != nil {
		t.Fatal(err)
	}
	rec := status(t, resp)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}

// The log line is for finding the row, not for reading the report: neither the
// text nor the query of the route may end up there.
func TestCreateBugReportLogsNoText(t *testing.T) {
	var logs bytes.Buffer
	store := &fakeStore{}
	h := NewHandler(NewService(store, newFakeStorage(), 1<<20), slog.New(slog.NewTextHandler(&logs, nil)))

	resp, err := h.CreateBugReport(signedIn(t.Context(), uuid.New()), formRequest(t, textParts()))
	if err != nil {
		t.Fatal(err)
	}
	if rec := status(t, resp); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	for _, secret := range []string{"Секретный", "Открыть настройки", "token=abc"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("log contains %q: %s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), store.reports[0].ID.String()) {
		t.Errorf("log does not name the report: %s", logs.String())
	}
	if store.reports[0].Route != "/settings" {
		t.Errorf("stored route = %q, want the path only", store.reports[0].Route)
	}
}
