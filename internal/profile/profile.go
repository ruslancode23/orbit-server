// Package profile owns what a person shows about themselves: the display name,
// bio, accent colour, avatar key and custom status (SCRUM-103). It owns those
// columns of users; the rest of the row belongs to internal/auth. Other modules
// never read the columns themselves: they ask this package for the public part
// in one batch, so a member list or a page of messages stays one query.
//
// A profile is visible to people who already know the account — a common server,
// a common conversation, or a friendship. Anyone else gets the same not-found
// answer as an unknown id, so the API never reveals which accounts exist.
package profile

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Limits are the lengths the API schema also states. The database repeats them
// as CHECK constraints, so a bug in this package cannot write an unbounded row.
const (
	MaxDisplayNameLength = 32
	MaxBioLength         = 190
	MaxStatusEmojiLength = 32
	MaxStatusTextLength  = 128
)

var (
	// ErrNotFound covers an unknown account and one the caller may not see: the
	// same answer on purpose, or the API tells strangers which accounts exist.
	ErrNotFound = errors.New("profile: not found")
	// ErrInvalidDisplayName means the name is longer than the limit.
	ErrInvalidDisplayName = errors.New("profile: a display name must be at most 32 characters")
	// ErrInvalidDisplayNameChars means the name holds a control, zero-width or
	// bidi character: it could disguise itself as another name.
	ErrInvalidDisplayNameChars = errors.New("profile: a display name cannot contain control, zero-width or bidi characters")
	// ErrInvalidBio means the bio is longer than the limit.
	ErrInvalidBio = errors.New("profile: a bio must be at most 190 characters")
	// ErrInvalidBioChars means the bio holds a control or bidi character.
	ErrInvalidBioChars = errors.New("profile: a bio cannot contain control or bidi characters")
	// ErrInvalidAccent means the colour is not one of the named palette tokens.
	ErrInvalidAccent = errors.New("profile: unknown accent color")
	// ErrInvalidStatusEmoji means the emoji is longer than the limit or holds a
	// forbidden character.
	ErrInvalidStatusEmoji = errors.New("profile: a custom status emoji must be at most 32 characters and printable")
	// ErrInvalidStatusText means the text is longer than the limit.
	ErrInvalidStatusText = errors.New("profile: a custom status text must be at most 128 characters")
	// ErrInvalidStatusChars means the status holds a control or bidi character.
	ErrInvalidStatusChars = errors.New("profile: a custom status cannot contain control or bidi characters")
	// ErrEmptyStatus means neither an emoji nor a text was given.
	ErrEmptyStatus = errors.New("profile: a custom status needs an emoji or a text")
	// ErrInvalidExpiry means the requested lifetime is not one of the options.
	ErrInvalidExpiry = errors.New("profile: unknown status expiry")
	// ErrNothingToUpdate means a profile update named no field.
	ErrNothingToUpdate = errors.New("profile: nothing to update")
	// ErrSelf means the caller asked for a mutual list with their own account:
	// a mutual list needs two different accounts.
	ErrSelf = errors.New("profile: a mutual list needs two different accounts")
)

// AccentColor is one of the product's named palette tokens. A raw hex colour is
// not accepted: the client maps the token to the palette of its current theme,
// so a profile looks right everywhere instead of fighting a dark background.
type AccentColor string

const (
	AccentSlate  AccentColor = "slate"
	AccentRed    AccentColor = "red"
	AccentAmber  AccentColor = "amber"
	AccentGreen  AccentColor = "green"
	AccentTeal   AccentColor = "teal"
	AccentSky    AccentColor = "sky"
	AccentIndigo AccentColor = "indigo"
	AccentViolet AccentColor = "violet"
	AccentPink   AccentColor = "pink"
)

// DefaultAccent is what an account has until it picks another.
const DefaultAccent = AccentSlate

// Valid reports whether the colour is one this server knows.
func (c AccentColor) Valid() bool {
	switch c {
	case AccentSlate, AccentRed, AccentAmber, AccentGreen, AccentTeal,
		AccentSky, AccentIndigo, AccentViolet, AccentPink:
		return true
	}
	return false
}

// CustomStatus is a short line next to the name. An expired status is never
// handed out: the service drops it as it reads, so no background job is needed.
type CustomStatus struct {
	Emoji string
	Text  string
	// ExpiresAt is nil when the status lasts until it is replaced.
	ExpiresAt *time.Time
}

// Public is the part of an account other people see. AvatarURL is a signed
// link; the key behind it never leaves this package.
type Public struct {
	ID           uuid.UUID
	Nickname     string
	DisplayName  string
	AvatarURL    string
	AccentColor  AccentColor
	Bio          string
	CustomStatus *CustomStatus
	IsGuest      bool
	CreatedAt    time.Time
}

// Stored is a profile as the database keeps it: the public part plus the avatar
// key, which the service signs into a link but never returns as a value.
type Stored struct {
	Public
	AvatarKey string
}

// MutualServer is one server two accounts are on, as the caller sees it. The
// servers module fills it: this package owns the pair question, that module owns
// the server, its icon key and the caller's role there.
type MutualServer struct {
	ID      uuid.UUID
	Name    string
	IconURL string
	// Role is the caller's role on this server, as the servers module names it.
	Role string
}

// Mutual is what two accounts share: the servers both are on and the friends
// both have, each list an intersection.
type Mutual struct {
	Servers []MutualServer
	Friends []Public
}

// Update carries only what the caller asked to change. A pointer to an empty
// string clears a text field; a nil pointer leaves it alone.
type Update struct {
	DisplayName *string
	Bio         *string
	AccentColor *AccentColor
}

func (u Update) empty() bool {
	return u.DisplayName == nil && u.Bio == nil && u.AccentColor == nil
}

// Expiry is how long a custom status lives. The server turns it into a deadline
// when the status is set, so a client clock cannot stretch it.
type Expiry string

const (
	Expiry30Minutes Expiry = "30m"
	Expiry4Hours    Expiry = "4h"
	// ExpiryEndOfDay is the end of the current UTC day: 00:00 UTC next day. The
	// server has no per-account time zone, and guessing one would make the same
	// request mean different things in different deployments.
	ExpiryEndOfDay Expiry = "end_of_day"
	ExpiryNever    Expiry = "never"
)

func (e Expiry) valid() bool {
	switch e {
	case Expiry30Minutes, Expiry4Hours, ExpiryEndOfDay, ExpiryNever:
		return true
	}
	return false
}

// deadline is when a status set at now stops showing. nil means no deadline.
func (e Expiry) deadline(now time.Time) *time.Time {
	var at time.Time
	switch e {
	case Expiry30Minutes:
		at = now.Add(30 * time.Minute)
	case Expiry4Hours:
		at = now.Add(4 * time.Hour)
	case ExpiryEndOfDay:
		year, month, day := now.UTC().Date()
		at = time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC)
	case ExpiryNever:
		return nil
	default:
		return nil
	}
	return &at
}

// StatusUpdate is one replacement of the custom status.
type StatusUpdate struct {
	Emoji string
	Text  string
	// ExpiresIn is empty when the caller did not ask for a deadline: never.
	ExpiresIn Expiry
}

// cleaned validates the status and fills in the default lifetime.
func (u StatusUpdate) cleaned() (StatusUpdate, error) {
	emoji, err := cleanStatusEmoji(u.Emoji)
	if err != nil {
		return StatusUpdate{}, err
	}
	text, err := cleanStatusText(u.Text)
	if err != nil {
		return StatusUpdate{}, err
	}
	expiry := u.ExpiresIn
	if expiry == "" {
		expiry = ExpiryNever
	}
	if !expiry.valid() {
		return StatusUpdate{}, ErrInvalidExpiry
	}
	if emoji == "" && text == "" {
		return StatusUpdate{}, ErrEmptyStatus
	}
	return StatusUpdate{Emoji: emoji, Text: text, ExpiresIn: expiry}, nil
}

// cleanDisplayName trims a name and rejects the invisible ways to forge another
// one. An empty result is not an error: on a partial update it means "show the
// nickname again", and the service decides that before calling this.
func cleanDisplayName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if utf8.RuneCountInString(value) > MaxDisplayNameLength {
		return "", ErrInvalidDisplayName
	}
	// Every format character is out: zero-width joiners, bidi overrides and the
	// rest can visually reorder or hide a name, which is impersonation.
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", ErrInvalidDisplayNameChars
		}
	}
	return value, nil
}

// cleanBio truncates nothing and rejects control characters. An empty result
// clears the bio.
func cleanBio(value string) (string, error) {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > MaxBioLength {
		return "", ErrInvalidBio
	}
	for _, r := range value {
		if unicode.IsControl(r) || isBidiControl(r) {
			return "", ErrInvalidBioChars
		}
	}
	return value, nil
}

// cleanStatusEmoji keeps a composed emoji sequence intact: the zero-width
// joiner is a format character, and family emoji need it.
func cleanStatusEmoji(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if utf8.RuneCountInString(value) > MaxStatusEmojiLength {
		return "", ErrInvalidStatusEmoji
	}
	for _, r := range value {
		if unicode.IsControl(r) || isBidiControl(r) {
			return "", ErrInvalidStatusEmoji
		}
	}
	return value, nil
}

func cleanStatusText(value string) (string, error) {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > MaxStatusTextLength {
		return "", ErrInvalidStatusText
	}
	for _, r := range value {
		if unicode.IsControl(r) || isBidiControl(r) {
			return "", ErrInvalidStatusChars
		}
	}
	return value, nil
}

// isBidiControl reports the characters that reorder text around themselves.
// They stay out of every free-text field; other format characters are allowed
// where a composed emoji needs them.
func isBidiControl(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, // LRE, RLE, PDF, LRO, RLO
		r >= 0x2066 && r <= 0x2069, // LRI, RLI, FSI, PDI
		r == 0x200E, r == 0x200F:   // LRM, RLM
		return true
	}
	return false
}

// hideExpired returns the profile as others see it at now: an expired status is
// simply absent.
func hideExpired(p Public, now time.Time) Public {
	if p.CustomStatus == nil || p.CustomStatus.ExpiresAt == nil {
		return p
	}
	if !p.CustomStatus.ExpiresAt.After(now) {
		p.CustomStatus = nil
	}
	return p
}
