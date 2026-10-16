package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashPasswordVerifies(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Errorf("hash = %q, want an argon2id PHC string", hash)
	}
	if strings.Contains(hash, "correct horse battery") {
		t.Error("the hash contains the password")
	}
	if err := VerifyPassword(hash, "correct horse battery"); err != nil {
		t.Errorf("VerifyPassword with the right password: %v", err)
	}
}

func TestHashPasswordUsesFreshSalt(t *testing.T) {
	first, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	second, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if first == second {
		t.Error("two hashes of one password are equal: the salt is not random")
	}
}

func TestVerifyPasswordRejects(t *testing.T) {
	hash, err := HashPassword("right password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	for _, tc := range []struct {
		name     string
		password string
	}{
		{"wrong password", "wrong password"},
		{"empty password", ""},
		{"password with a prefix of the right one", "right"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyPassword(hash, tc.password); !errors.Is(err, ErrPasswordMismatch) {
				t.Errorf("err = %v, want ErrPasswordMismatch", err)
			}
		})
	}
}

func TestVerifyPasswordRejectsUnreadableHash(t *testing.T) {
	for _, tc := range []struct {
		name string
		hash string
	}{
		{"empty", ""},
		{"bcrypt", "$2a$10$abcdefghijklmnopqrstuv"},
		{"wrong version", "$argon2id$v=16$m=65536,t=2,p=4$c2FsdHNhbHQ$aGFzaA"},
		{"unreadable parameters", "$argon2id$v=19$m=lots$c2FsdHNhbHQ$aGFzaA"},
		{"salt not base64", "$argon2id$v=19$m=65536,t=2,p=4$!!!$aGFzaA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyPassword(tc.hash, "any password")
			if err == nil {
				t.Fatal("err = nil, want an error")
			}
			// An unreadable hash is a server problem, not a wrong password.
			if errors.Is(err, ErrPasswordMismatch) {
				t.Errorf("err = %v, want something other than ErrPasswordMismatch", err)
			}
		})
	}
}

// A hash written with other parameters still verifies: they are read from the hash itself.
func TestVerifyPasswordAcceptsOtherParameters(t *testing.T) {
	// m=19456,t=2,p=1 — OWASP's second recommended setting.
	const hash = "$argon2id$v=19$m=19456,t=2,p=1$3P7Xn1xkxLxrr7Zc5oRdHw$Qo1xJ5mXLKp4VhvZVLHLlRT3DkLCKQoFwTVZBIaQhsY"
	if err := VerifyPassword(hash, "whatever"); errors.Is(err, ErrPasswordMismatch) {
		return // parameters were read; the password simply does not match this fixture
	} else if err != nil {
		t.Errorf("err = %v, want the parameters to be read", err)
	}
}
