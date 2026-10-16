package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. OWASP's baseline for interactive logins: 64 MiB, two passes.
// They are stored inside every hash, so raising them here keeps old hashes verifiable.
const (
	argonMemoryKiB = 64 * 1024
	argonTime      = 2
	argonKeyLen    = 32
	argonSaltLen   = 16
)

// ErrPasswordMismatch means the password does not match the hash. It never says more:
// the caller answers "wrong login or password" either way.
var ErrPasswordMismatch = errors.New("auth: password does not match")

// HashPassword returns the PHC string of the password: $argon2id$v=19$m=...,t=...,p=...$salt$hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	parallelism := argonParallelism()
	sum := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, parallelism, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum),
	), nil
}

// VerifyPassword reports whether the password matches the stored hash. It returns
// ErrPasswordMismatch for a wrong password and another error for an unreadable hash.
func VerifyPassword(encoded, password string) error {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash
	if len(parts) != 6 || parts[1] != "argon2id" {
		return errors.New("auth: hash is not argon2id")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return fmt.Errorf("auth: unsupported argon2 version %q", parts[2])
	}

	var memory, time uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &parallelism); err != nil {
		return fmt.Errorf("auth: unreadable argon2 parameters %q", parts[3])
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return fmt.Errorf("auth: unreadable salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return fmt.Errorf("auth: unreadable hash: %w", err)
	}

	if len(want) == 0 || len(want) > 1024 {
		return fmt.Errorf("auth: hash length %d is out of range", len(want))
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, parallelism, uint32(len(want))) //nolint:gosec // range checked above
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

func argonParallelism() uint8 {
	if n := runtime.NumCPU(); n < 4 {
		return uint8(n) //nolint:gosec // NumCPU is at least 1 and below 4 here
	}
	return 4
}
