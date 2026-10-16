// Package voice issues access tokens for LiveKit voice rooms.
package voice

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/livekit/protocol/auth"
)

// TokenTTL is how long a token can be used to join a room. LiveKit checks it
// only on connect, so an established session outlives the token.
const TokenTTL = 10 * time.Minute

const maxDisplayNameLen = 32

var roomNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidationError reports invalid user input; its message is safe to return to the client.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

type Token struct {
	Value     string
	URL       string // LiveKit URL the client connects to
	Identity  string
	ExpiresAt time.Time
}

type TokenIssuer struct {
	apiKey    string
	apiSecret string
	publicURL string
	now       func() time.Time
	newID     func() (string, error)
}

func NewTokenIssuer(apiKey, apiSecret, publicURL string) *TokenIssuer {
	return &TokenIssuer{
		apiKey:    apiKey,
		apiSecret: apiSecret,
		publicURL: publicURL,
		now:       time.Now,
		newID:     randomIdentity,
	}
}

// Issue returns a token that lets one new participant join exactly one room,
// publish and subscribe. Every call gets a fresh identity, so the same person
// can join from two tabs without kicking themselves out.
func (i *TokenIssuer) Issue(room, displayName string) (Token, error) {
	displayName = strings.TrimSpace(displayName)
	if err := validate(room, displayName); err != nil {
		return Token{}, err
	}

	identity, err := i.newID()
	if err != nil {
		return Token{}, fmt.Errorf("generate identity: %w", err)
	}

	allow := true
	at := auth.NewAccessToken(i.apiKey, i.apiSecret).
		SetIdentity(identity).
		SetName(displayName).
		SetValidFor(TokenTTL).
		SetVideoGrant(&auth.VideoGrant{
			RoomJoin:       true,
			Room:           room,
			CanPublish:     &allow,
			CanSubscribe:   &allow,
			CanPublishData: &allow,
			// Clients publish voice state (deafened) as participant attributes.
			CanUpdateOwnMetadata: &allow,
		})
	jwt, err := at.ToJWT()
	if err != nil {
		return Token{}, fmt.Errorf("sign token: %w", err)
	}

	return Token{
		Value:     jwt,
		URL:       i.publicURL,
		Identity:  identity,
		ExpiresAt: i.now().Add(TokenTTL).UTC(),
	}, nil
}

func validate(room, displayName string) error {
	var errs []error
	if !roomNamePattern.MatchString(room) {
		errs = append(errs, &ValidationError{"room must be 1-64 characters: latin letters, digits, '-' or '_', starting with a letter or digit"})
	}
	switch n := utf8.RuneCountInString(displayName); {
	case n == 0:
		errs = append(errs, &ValidationError{"displayName is required"})
	case n > maxDisplayNameLen:
		errs = append(errs, &ValidationError{fmt.Sprintf("displayName must not exceed %d characters", maxDisplayNameLen)})
	case strings.IndexFunc(displayName, unicode.IsControl) >= 0:
		errs = append(errs, &ValidationError{"displayName must not contain control characters"})
	}
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	return &ValidationError{strings.Join(msgs, "; ")}
}

// IsValidationError reports whether err is caused by invalid input.
func IsValidationError(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

func randomIdentity() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return "guest-" + hex.EncodeToString(b), nil
}
