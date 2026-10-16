package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// SessionCookieName holds the session token in the browser. HttpOnly, so a script
// cannot read it, and the browser sends it on a WebSocket upgrade too.
const SessionCookieName = "orbit_session"

// sessionTokenBytes is the entropy of a session token: 256 bits, so guessing one is hopeless.
const sessionTokenBytes = 32

// newSessionToken returns the token handed to the client and the hash stored for it.
// Only the hash is stored: a leaked sessions table does not hand out live sessions.
func newSessionToken() (token string, hash []byte, err error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("read token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, hashToken(token), nil
}

// hashToken is SHA-256 and not a password hash on purpose: the token is random,
// 256 bits long, so there is nothing to brute-force, and every request hashes it.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
