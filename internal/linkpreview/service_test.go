package linkpreview

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore keeps the cache in memory, like the table.
type fakeStore struct {
	previews map[string]cachedPreview
	links    map[uuid.UUID]uuid.UUID
}

func newFakeStore() *fakeStore {
	return &fakeStore{previews: map[string]cachedPreview{}, links: map[uuid.UUID]uuid.UUID{}}
}

func (s *fakeStore) Preview(_ context.Context, normalized string) (cachedPreview, error) {
	preview, ok := s.previews[normalized]
	if !ok {
		return cachedPreview{}, ErrNotFound
	}
	return preview, nil
}

func (s *fakeStore) Save(_ context.Context, preview cachedPreview) (cachedPreview, error) {
	if preview.ID == uuid.Nil {
		preview.ID = uuid.New()
	}
	s.previews[preview.URL] = preview
	return preview, nil
}

func (s *fakeStore) Attach(_ context.Context, messageID, previewID uuid.UUID) error {
	s.links[messageID] = previewID
	return nil
}

func (s *fakeStore) Detach(_ context.Context, messageID uuid.UUID) error {
	delete(s.links, messageID)
	return nil
}

func (s *fakeStore) ForMessages(_ context.Context, messageIDs []uuid.UUID) (map[uuid.UUID]Preview, error) {
	out := map[uuid.UUID]Preview{}
	for _, messageID := range messageIDs {
		if id, ok := s.links[messageID]; ok {
			for _, preview := range s.previews {
				if preview.ID == id {
					out[messageID] = preview.Preview
				}
			}
		}
	}
	return out, nil
}

func (s *fakeStore) ByID(_ context.Context, id uuid.UUID) (cachedPreview, error) {
	for _, preview := range s.previews {
		if preview.ID == id {
			return preview, nil
		}
	}
	return cachedPreview{}, ErrNotFound
}

func TestBlockedAddresses(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "100.100.100.200", "0.0.0.0", "::",
		"224.0.0.1", "ff02::1", "fc00::1", "fd00::1", "fe80::1",
		"198.18.0.1", "240.0.0.1",
	}
	for _, value := range blocked {
		ip := net.ParseIP(value)
		if ip == nil {
			t.Fatalf("bad test address %q", value)
		}
		if !blockedIP(ip) {
			t.Errorf("%s is allowed, want blocked", value)
		}
	}
	public := []string{"8.8.8.8", "1.1.1.1", "2001:4860:4860::8888", "2606:4700:4700::1111"}
	for _, value := range public {
		if blockedIP(net.ParseIP(value)) {
			t.Errorf("%s is blocked, want allowed", value)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct{ input, want string }{
		{"HTTPS://Example.COM:443/Path?x=1#frag", "https://example.com/Path?x=1"},
		{"http://example.com:80/a", "http://example.com/a"},
		{"http://example.com:8080/a", "http://example.com:8080/a"},
		{" https://Example.com/a ", "https://example.com/a"},
	}
	for _, tc := range cases {
		got, err := Normalize(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	for _, bad := range []string{"ftp://example.com", "javascript:alert(1)", "", "http:///x"} {
		if _, err := Normalize(bad); !errors.Is(err, ErrBlocked) {
			t.Errorf("Normalize(%q) = %v, want ErrBlocked", bad, err)
		}
	}
}

func TestFirstURL(t *testing.T) {
	cases := map[string]string{
		"смотри https://orbit.chat/docs.":      "https://orbit.chat/docs",
		"нет ссылок":                           "",
		"http://a.example и https://b.example": "http://a.example",
		"<https://orbit.chat>":                 "https://orbit.chat",
	}
	for input, want := range cases {
		if got := FirstURL(input); got != want {
			t.Errorf("FirstURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseOpenGraph(t *testing.T) {
	page := `<html><head>
		<meta property="og:title" content="Orbit &amp; Co">
		<meta property="og:description" content="Голос для команд">
		<meta property="og:site_name" content="Orbit">
		<meta property="og:image" content="/cover.png">
		</head></html>`
	got := parseOpenGraph(page)
	if got.title != "Orbit & Co" || got.description != "Голос для команд" || got.siteName != "Orbit" || got.image != "/cover.png" {
		t.Fatalf("parseOpenGraph = %+v", got)
	}

	fallback := parseOpenGraph(`<html><head><title>Just a page</title></head></html>`)
	if fallback.title != "Just a page" {
		t.Errorf("title fallback = %q", fallback.title)
	}
}

func TestAttachFetchesAndCaches(t *testing.T) {
	requests := 0
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<meta property="og:title" content="Заголовок"><meta property="og:image" content="/i.png">`))
	}))
	defer page.Close()

	store := newFakeStore()
	svc := NewService(store, nil, false)
	// The httptest server listens on 127.0.0.1, which the safe client refuses by
	// design; the test replaces only the client, not the rules.
	svc.client = page.Client()
	messageID := uuid.New()
	preview, err := svc.Attach(t.Context(), uuid.New(), messageID, page.URL+"/page")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if preview.Title != "Заголовок" || preview.ImageURL != page.URL+"/i.png" {
		t.Fatalf("preview = %+v", preview)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want one", requests)
	}
	// The cache answers the second message without another request.
	if _, err := svc.Attach(t.Context(), uuid.New(), uuid.New(), page.URL+"/page"); err != nil {
		t.Fatalf("cached Attach: %v", err)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want the cache to answer", requests)
	}
}

func TestAttachCachesFailures(t *testing.T) {
	requests := 0
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer page.Close()
	svc := NewService(newFakeStore(), nil, false)
	svc.client = page.Client()
	if _, err := svc.Attach(t.Context(), uuid.New(), uuid.New(), page.URL); !errors.Is(err, ErrNoPreview) {
		t.Fatalf("Attach = %v, want ErrNoPreview", err)
	}
	if _, err := svc.Attach(t.Context(), uuid.New(), uuid.New(), page.URL); !errors.Is(err, ErrNoPreview) {
		t.Fatalf("cached Attach = %v, want ErrNoPreview", err)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want the failure cached", requests)
	}
}

func TestFetchRejectsInternalAddress(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<title>secret</title>`))
	}))
	defer page.Close()
	svc := NewService(newFakeStore(), nil, false)
	// The real safe client must refuse the loopback address the test server has.
	_, err := svc.Attach(context.Background(), uuid.New(), uuid.New(), page.URL)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("Attach to loopback = %v, want ErrBlocked", err)
	}
}

func TestImageOnlyServesImages(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/svg" {
			w.Header().Set("Content-Type", "image/svg+xml")
			_, _ = w.Write([]byte(`<svg/>`))
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png"))
	}))
	defer page.Close()

	store := newFakeStore()
	svc := NewService(store, nil, false)
	svc.client = page.Client()
	preview, err := store.Save(t.Context(), cachedPreview{
		Preview: Preview{ID: uuid.New(), URL: "https://example.com", Title: "t", ImageURL: page.URL + "/img"},
		Status:  "ok", FetchedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, contentType, err := svc.Image(t.Context(), preview.ID)
	if err != nil || contentType != "image/png" || string(body) != "png" {
		t.Fatalf("Image = %q %q %v", body, contentType, err)
	}

	svg, err := store.Save(t.Context(), cachedPreview{
		Preview: Preview{ID: uuid.New(), URL: "https://example.com/s", Title: "t", ImageURL: page.URL + "/svg"},
		Status:  "ok", FetchedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Image(t.Context(), svg.ID); !errors.Is(err, ErrNotHTML) {
		t.Errorf("svg image = %v, want ErrNotHTML", err)
	}
}
