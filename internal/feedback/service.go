// Package feedback owns bug reports sent from inside the app. A report is what
// the person wrote, an optional screenshot and the technical context the client
// attaches: the build, the browser and the route. The account comes from the
// session. Message text, tokens and secret chat content never belong here, so
// the route loses its query and fragment before it is stored.
package feedback

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"

	// PNG and JPEG register themselves with image; WebP through x/image.
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	_ "golang.org/x/image/webp"

	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

var (
	// ErrScreenshotTooLarge means the image is over this instance's byte limit.
	ErrScreenshotTooLarge = errors.New("feedback: the screenshot is too large")
	// ErrScreenshotType means the bytes are not a PNG, JPEG or WebP image.
	ErrScreenshotType = errors.New("feedback: the screenshot must be a PNG, JPEG or WebP image")
)

// Field limits, the same as in api/openapi.yaml and the bug_reports checks.
// The schema already refuses longer input; the service repeats the rule so it
// holds for every caller, not only for the HTTP one.
const (
	maxDescription = 4000
	maxSteps       = 4000
	maxAppVersion  = 64
	maxUserAgent   = 512
	maxRoute       = 512
	// sniffBytes is what http.DetectContentType reads.
	sniffBytes = 512
)

// screenshotTypes are what a screenshot may be, by what the bytes say, with
// the extension its object gets.
var screenshotTypes = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
}

// FieldError is a refusal about one field of the report. The API puts it in a
// 400 whose message starts with the field's name, so a client can show it
// next to that field.
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Reason }

// LimitedError means the account sent too many reports; RetryAfter is how long
// to wait.
type LimitedError struct {
	RetryAfter time.Duration
}

func (e *LimitedError) Error() string {
	return fmt.Sprintf("feedback: too many reports, retry in %s", e.RetryAfter)
}

// Draft is what the client sends, before the server checks it.
type Draft struct {
	Description string
	Steps       string
	AppVersion  string
	UserAgent   string
	Route       string
}

// Screenshot is a stored image: where it lies in the object store and what it is.
type Screenshot struct {
	Key       string
	MimeType  string
	SizeBytes int64
}

// Report is a stored bug report.
type Report struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Description string
	Steps       string
	AppVersion  string
	UserAgent   string
	Route       string
	Screenshot  *Screenshot
	CreatedAt   time.Time
}

type Store interface {
	Create(ctx context.Context, r Report) (Report, error)
}

// Storage is the object store the screenshots go to.
type Storage interface {
	Put(ctx context.Context, key, mimeType string, body io.Reader, size int64) error
	Delete(ctx context.Context, key string) error
}

type Service struct {
	store              Store
	storage            Storage
	maxScreenshotBytes int64
	limiter            *ratelimit.Limiter
}

func NewService(store Store, storage Storage, maxScreenshotBytes int64) *Service {
	return &Service{store: store, storage: storage, maxScreenshotBytes: maxScreenshotBytes}
}

// SetLimiter caps how many reports one account may send; nil allows any number.
func (s *Service) SetLimiter(limiter *ratelimit.Limiter) { s.limiter = limiter }

// MaxScreenshotBytes is this instance's screenshot limit, so the API can say it plainly.
func (s *Service) MaxScreenshotBytes() int64 { return s.maxScreenshotBytes }

// CheckLimit says whether the account may send a report now without spending
// anything: the handler asks before it reads the form, and Submit spends.
func (s *Service) CheckLimit(userID uuid.UUID) error {
	if ok, wait := s.limiter.Check(userID.String()); !ok {
		return &LimitedError{RetryAfter: wait}
	}
	return nil
}

// ReadScreenshot reads an uploaded image up to the limit. One byte over is
// enough to refuse: it reads no further.
func (s *Service) ReadScreenshot(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, s.maxScreenshotBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read screenshot: %w", err)
	}
	if int64(len(data)) > s.maxScreenshotBytes {
		return nil, ErrScreenshotTooLarge
	}
	return data, nil
}

// Submit checks a draft and stores it with its screenshot, if there is one.
// The image goes to the object store first and the row second: a row must not
// point at nothing, and an object without a row is taken back.
func (s *Service) Submit(ctx context.Context, userID uuid.UUID, draft Draft, screenshot []byte) (Report, error) {
	report, err := clean(draft)
	if err != nil {
		return Report{}, err
	}
	report.ID = uuid.New()
	report.UserID = userID

	var mimeType string
	if screenshot != nil {
		mimeType, err = s.screenshotType(screenshot)
		if err != nil {
			return Report{}, err
		}
	}

	// The token is spent only on a report that is about to be stored: a typo in
	// the form costs nothing, and parallel attempts still meet the limit here.
	if ok, wait := s.limiter.Allow(userID); !ok {
		return Report{}, &LimitedError{RetryAfter: wait}
	}

	if screenshot != nil {
		key := "bug-reports/" + report.ID.String() + screenshotTypes[mimeType]
		if err := s.storage.Put(ctx, key, mimeType, bytes.NewReader(screenshot), int64(len(screenshot))); err != nil {
			return Report{}, fmt.Errorf("store screenshot: %w", err)
		}
		report.Screenshot = &Screenshot{Key: key, MimeType: mimeType, SizeBytes: int64(len(screenshot))}
	}

	stored, err := s.store.Create(ctx, report)
	if err != nil {
		if report.Screenshot != nil {
			_ = s.storage.Delete(ctx, report.Screenshot.Key)
		}
		return Report{}, err
	}
	return stored, nil
}

// screenshotType reads the type from the bytes, never from the name, and
// checks that the header really is an image of that kind.
func (s *Service) screenshotType(data []byte) (string, error) {
	if len(data) == 0 {
		return "", &FieldError{Field: "screenshot", Reason: "the file is empty"}
	}
	if int64(len(data)) > s.maxScreenshotBytes {
		return "", ErrScreenshotTooLarge
	}
	mimeType := http.DetectContentType(data[:min(len(data), sniffBytes)])
	if _, ok := screenshotTypes[mimeType]; !ok {
		return "", ErrScreenshotType
	}
	// A PNG signature in front of garbage passes the sniffer; the header does not.
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != mimeType {
		return "", ErrScreenshotType
	}
	return mimeType, nil
}

// clean trims the text, checks every field and cuts the route down to its path.
func clean(draft Draft) (Report, error) {
	report := Report{
		Description: strings.TrimSpace(draft.Description),
		Steps:       strings.TrimSpace(draft.Steps),
		AppVersion:  strings.TrimSpace(draft.AppVersion),
		UserAgent:   strings.TrimSpace(draft.UserAgent),
		Route:       pathOnly(strings.TrimSpace(draft.Route)),
	}
	switch {
	case report.Description == "":
		return Report{}, &FieldError{Field: "description", Reason: "describe what went wrong"}
	case utf8.RuneCountInString(report.Description) > maxDescription:
		return Report{}, &FieldError{Field: "description", Reason: fmt.Sprintf("must not exceed %d characters", maxDescription)}
	case utf8.RuneCountInString(report.Steps) > maxSteps:
		return Report{}, &FieldError{Field: "steps", Reason: fmt.Sprintf("must not exceed %d characters", maxSteps)}
	case report.AppVersion == "" || utf8.RuneCountInString(report.AppVersion) > maxAppVersion:
		return Report{}, &FieldError{Field: "appVersion", Reason: fmt.Sprintf("must be 1 to %d characters", maxAppVersion)}
	case utf8.RuneCountInString(report.UserAgent) > maxUserAgent:
		return Report{}, &FieldError{Field: "userAgent", Reason: fmt.Sprintf("must not exceed %d characters", maxUserAgent)}
	case !strings.HasPrefix(report.Route, "/") || utf8.RuneCountInString(report.Route) > maxRoute:
		return Report{}, &FieldError{Field: "route", Reason: fmt.Sprintf("must be a path of at most %d characters", maxRoute)}
	}
	return report, nil
}

// pathOnly drops the query and the fragment: an invite code or a token there
// has no business in a report.
func pathOnly(route string) string {
	if i := strings.IndexAny(route, "?#"); i >= 0 {
		return route[:i]
	}
	return route
}
