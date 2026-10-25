// Package linkpreview fetches the OpenGraph card of the first link in a message.
// The server makes the request, never the reader's browser: every address is
// checked at dial time (loopback, private, link-local, CGNAT, multicast, IPv6
// ULA), only ports 80 and 443 are allowed and redirects are bounded. Results
// are cached by normalized URL for a day, failures included.
package linkpreview

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

var (
	// ErrNoPreview means the page has no card to show, or its cached fetch failed.
	ErrNoPreview = errors.New("linkpreview: no preview for this page")
	// ErrBlocked means the address is not a public one, or the scheme/port is not allowed.
	ErrBlocked = errors.New("linkpreview: the address is not allowed")
	// ErrNotHTML means the response is not a web page.
	ErrNotHTML = errors.New("linkpreview: the response is not HTML")
	// ErrTooLarge means the page (or image) is over the limit.
	ErrTooLarge = errors.New("linkpreview: the response is too large")
	// ErrRateLimited means the account asked for too many previews lately.
	ErrRateLimited = errors.New("linkpreview: too many previews")
	// ErrNotFound means there is no such cached preview or image.
	ErrNotFound = errors.New("linkpreview: not found")
)

const (
	// fetchTimeout bounds one fetch, redirects included.
	fetchTimeout = 5 * time.Second
	// maxBody is how much of a page is read: enough for the head and a little more.
	maxBody = 512 << 10
	// maxImage is how much of an image the proxy passes through.
	maxImage = 5 << 20
	// cacheTTL is how long a card is trusted, failures included.
	cacheTTL = 24 * time.Hour
	// maxRedirects is how many hops the fetch follows.
	maxRedirects = 3
)

// Preview is one card: what the page says about itself.
type Preview struct {
	ID          uuid.UUID
	URL         string
	Title       string
	Description string
	SiteName    string
	// ImageURL is the page's own image address; the API serves it through
	// /link-previews/{id}/image, so a reader's browser never calls it directly.
	ImageURL string
}

// Store is the cache table this package owns.
type Store interface {
	Preview(ctx context.Context, normalizedURL string) (cachedPreview, error)
	Save(ctx context.Context, p cachedPreview) (cachedPreview, error)
	Attach(ctx context.Context, messageID, previewID uuid.UUID) error
	Detach(ctx context.Context, messageID uuid.UUID) error
	ForMessages(ctx context.Context, messageIDs []uuid.UUID) (map[uuid.UUID]Preview, error)
	ByID(ctx context.Context, id uuid.UUID) (cachedPreview, error)
}

type cachedPreview struct {
	Preview
	Status    string
	FetchedAt time.Time
}

// Service fetches, caches and attaches previews. It knows nothing about HTTP
// responses; internal/messages owns the message and publishes the event.
type Service struct {
	store   Store
	client  *http.Client
	limiter *ratelimit.Limiter
	now     func() time.Time
}

// NewService returns a service with an SSRF-safe HTTP client. allowPrivate is
// for local end-to-end tests only; production must leave it off.
func NewService(store Store, limiter *ratelimit.Limiter, allowPrivate bool) *Service {
	return &Service{store: store, client: safeClient(allowPrivate), limiter: limiter, now: time.Now}
}

// Attach fetches the card of one URL (or takes it from the cache) and binds it
// to the message. A failure is cached too, so a broken link is not fetched again
// for a day.
func (s *Service) Attach(ctx context.Context, userID, messageID uuid.UUID, rawURL string) (Preview, error) {
	if s.limiter != nil {
		if allowed, _ := s.limiter.Allow(userID); !allowed {
			return Preview{}, ErrRateLimited
		}
	}
	normalized, err := Normalize(rawURL)
	if err != nil {
		return Preview{}, err
	}
	preview, err := s.previewFor(ctx, normalized)
	if err != nil {
		return Preview{}, err
	}
	if err := s.store.Attach(ctx, messageID, preview.ID); err != nil {
		return Preview{}, fmt.Errorf("attach preview: %w", err)
	}
	return preview.Preview, nil
}

// Detach removes the preview of a message. A message without one is not an error.
func (s *Service) Detach(ctx context.Context, messageID uuid.UUID) error {
	if err := s.store.Detach(ctx, messageID); err != nil {
		return fmt.Errorf("detach preview: %w", err)
	}
	return nil
}

// ForMessages is the previews of a page in one query.
func (s *Service) ForMessages(ctx context.Context, messageIDs []uuid.UUID) (map[uuid.UUID]Preview, error) {
	if len(messageIDs) == 0 {
		return map[uuid.UUID]Preview{}, nil
	}
	out, err := s.store.ForMessages(ctx, messageIDs)
	if err != nil {
		return nil, fmt.Errorf("previews of messages: %w", err)
	}
	return out, nil
}

// previewFor returns a fresh cached row, refetching after the TTL. A cached
// failure is returned as ErrNoPreview.
func (s *Service) previewFor(ctx context.Context, normalized string) (cachedPreview, error) {
	cached, err := s.store.Preview(ctx, normalized)
	if err == nil && s.now().Sub(cached.FetchedAt) < cacheTTL {
		if cached.Status != "ok" {
			return cachedPreview{}, ErrNoPreview
		}
		return cached, nil
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return cachedPreview{}, fmt.Errorf("read preview cache: %w", err)
	}

	preview, fetchErr := s.fetch(ctx, normalized)
	saved := cachedPreview{Preview: preview, Status: "ok", FetchedAt: s.now()}
	if fetchErr != nil {
		saved = cachedPreview{
			Preview:   Preview{ID: uuid.New(), URL: normalized},
			Status:    "error",
			FetchedAt: s.now(),
		}
	}
	stored, saveErr := s.store.Save(ctx, saved)
	if saveErr != nil {
		return cachedPreview{}, fmt.Errorf("save preview: %w", saveErr)
	}
	if fetchErr != nil {
		return cachedPreview{}, fetchErr
	}
	return stored, nil
}

// Image reads the image of a cached preview through the same address checks.
func (s *Service) Image(ctx context.Context, id uuid.UUID) ([]byte, string, error) {
	cached, err := s.store.ByID(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if cached.ImageURL == "" {
		return nil, "", ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cached.ImageURL, nil)
	if err != nil {
		return nil, "", ErrBlocked
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch preview image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", ErrNotFound
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if !allowedImageType(contentType) {
		return nil, "", ErrNotHTML
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImage+1))
	if err != nil {
		return nil, "", fmt.Errorf("read preview image: %w", err)
	}
	if len(body) > maxImage {
		return nil, "", ErrTooLarge
	}
	return body, contentType, nil
}

func allowedImageType(contentType string) bool {
	switch contentType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	}
	return false
}

// fetch reads one page and parses its OpenGraph tags, with <title> as a fallback.
func (s *Service) fetch(ctx context.Context, rawURL string) (Preview, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Preview{}, ErrBlocked
	}
	req.Header.Set("User-Agent", "OrbitBot/1.0 (+https://orbit.chat)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := s.client.Do(req)
	if err != nil {
		return Preview{}, fmt.Errorf("fetch page: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Preview{}, ErrNoPreview
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if contentType != "text/html" && contentType != "application/xhtml+xml" {
		return Preview{}, ErrNotHTML
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return Preview{}, fmt.Errorf("read page: %w", err)
	}
	if len(body) > maxBody {
		return Preview{}, ErrTooLarge
	}

	page := parseOpenGraph(string(body))
	preview := Preview{
		URL:         rawURL,
		Title:       page.title,
		Description: page.description,
		SiteName:    page.siteName,
	}
	if page.image != "" {
		// The image address may be relative; resolve it against the final page.
		base := resp.Request.URL
		if resolved, err := base.Parse(page.image); err == nil && (resolved.Scheme == "http" || resolved.Scheme == "https") {
			preview.ImageURL = resolved.String()
		}
	}
	if preview.Title == "" && preview.Description == "" && preview.ImageURL == "" {
		return Preview{}, ErrNoPreview
	}
	if preview.Title == "" {
		preview.Title = preview.URL
	}
	return preview, nil
}

// Normalize is the cache key: scheme and host lowered, the fragment dropped and
// the default port removed; the path and query stay as written.
func Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", ErrBlocked
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else {
		parsed.Host = host
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}

// FirstURL is the first http(s) link in a message, if it has one.
func FirstURL(text string) string {
	match := linkPattern.FindString(text)
	if match == "" {
		return ""
	}
	return strings.TrimRight(match, ".,!?;:»«\"')]}")
}

var linkPattern = regexp.MustCompile(`https?://[^\s<>"']+`)

// safeClient is the only client this package uses for untrusted addresses.
func safeClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   fetchTimeout,
		KeepAlive: -1,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return ErrBlocked
			}
			// The browser never fetches the preview; the server does, and only
			// on the two web ports. The local end-to-end switch lifts the port
			// rule together with the address rule.
			if !allowPrivate && port != "80" && port != "443" {
				return ErrBlocked
			}
			ip := net.ParseIP(host)
			if ip == nil || (!allowPrivate && blockedIP(ip)) {
				return ErrBlocked
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: fetchTimeout,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return ErrBlocked
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return ErrBlocked
			}
			// The dial-time check also covers a redirect to a private address.
			return nil
		},
	}
}

// blockedIP lists everything that is not a public unicast address: loopback,
// private, link-local (cloud metadata lives there), CGNAT, multicast, the
// unspecified address, benchmarking and reserved ranges and IPv6 ULA.
func blockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		for _, network := range blockedIPv4 {
			if network.Contains(v4) {
				return true
			}
		}
		return false
	}
	for _, network := range blockedIPv6 {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

var blockedIPv4 = mustPrefixes(
	"0.0.0.0/8",
	"100.64.0.0/10",  // CGNAT and some cloud metadata (100.100.100.200)
	"169.254.0.0/16", // link-local, includes 169.254.169.254
	"192.0.0.0/24",
	"192.0.2.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"240.0.0.0/4",
)

var blockedIPv6 = mustPrefixes(
	"fc00::/7", // unique local
	"fe80::/10",
	"ff00::/8",
	"2001:db8::/32",
)

func mustPrefixes(values ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			panic(err)
		}
		out = append(out, network)
	}
	return out
}

type ogPage struct {
	title       string
	description string
	siteName    string
	image       string
}

var (
	metaTagPattern  = regexp.MustCompile(`(?is)<meta\s+[^>]*>`)
	titleTagPattern = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	attributeName   = regexp.MustCompile(`(?is)([a-z_:][-a-z_:0-9]*)\s*=\s*("([^"]*)"|'([^']*)'|([^\s"'>]+))`)
)

// parseOpenGraph reads the meta tags of a page without an HTML parser: a card is all it needs.
func parseOpenGraph(page string) ogPage {
	out := ogPage{}
	for _, tag := range metaTagPattern.FindAllString(page, 64) {
		attrs := map[string]string{}
		for _, match := range attributeName.FindAllStringSubmatch(tag, -1) {
			value := match[3]
			if value == "" {
				value = match[4]
			}
			if value == "" {
				value = match[5]
			}
			attrs[strings.ToLower(match[1])] = html.UnescapeString(value)
		}
		key := attrs["property"]
		if key == "" {
			key = attrs["name"]
		}
		content := strings.TrimSpace(attrs["content"])
		if content == "" {
			continue
		}
		switch strings.ToLower(key) {
		case "og:title":
			out.title = content
		case "og:description":
			out.description = content
		case "og:site_name":
			out.siteName = content
		case "og:image", "og:image:url":
			if out.image == "" {
				out.image = content
			}
		}
	}
	if out.title == "" {
		if match := titleTagPattern.FindStringSubmatch(page); match != nil {
			out.title = strings.TrimSpace(html.UnescapeString(match[1]))
		}
	}
	return out
}
