package voice

import (
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
)

const (
	testKey    = "devkey"
	testSecret = "a-secret-that-is-long-enough-for-hs256"
	testURL    = "ws://localhost:7880"
)

func newTestIssuer(now time.Time) *TokenIssuer {
	i := NewTokenIssuer(testKey, testSecret, testURL)
	i.now = func() time.Time { return now }
	return i
}

func verify(t *testing.T, jwt string) *auth.ClaimGrants {
	t.Helper()
	v, err := auth.ParseAPIToken(jwt)
	if err != nil {
		t.Fatalf("ParseAPIToken() error = %v", err)
	}
	if v.APIKey() != testKey {
		t.Errorf("api key = %q, want %q", v.APIKey(), testKey)
	}
	_, grants, err := v.Verify(testSecret)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	return grants
}

func TestIssueGrantsOneRoom(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tok, err := newTestIssuer(now).Issue("channel-id", "account-id", "  Руслан  ", true)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	g := verify(t, tok.Value)
	if g.Video == nil {
		t.Fatal("token has no video grant")
	}
	if !g.Video.RoomJoin || g.Video.Room != "channel-id" {
		t.Errorf("grant = join:%v room:%q, want join to channel-id", g.Video.RoomJoin, g.Video.Room)
	}
	if g.Video.RoomCreate || g.Video.RoomAdmin || g.Video.RoomList || g.Video.RoomRecord {
		t.Error("token must not carry admin grants")
	}
	if g.Video.CanPublish == nil || !*g.Video.CanPublish || g.Video.CanSubscribe == nil || !*g.Video.CanSubscribe {
		t.Error("a speaker must be allowed to publish and subscribe")
	}
	if g.Video.CanUpdateOwnMetadata == nil || !*g.Video.CanUpdateOwnMetadata {
		t.Error("token must allow updating own attributes (voice state)")
	}
	if g.Name != "Руслан" {
		t.Errorf("name = %q, want trimmed %q", g.Name, "Руслан")
	}
	if g.Identity != "account-id" || tok.Identity != "account-id" {
		t.Errorf("identity = %q (claims %q), want the account id", tok.Identity, g.Identity)
	}
	if want := now.Add(TokenTTL); !tok.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", tok.ExpiresAt, want)
	}
	if tok.URL != testURL {
		t.Errorf("URL = %q, want %q", tok.URL, testURL)
	}
}

// A read-only voice channel is a broadcast: the listener joins and subscribes,
// but the token carries no right to publish.
func TestIssueListenerCannotPublish(t *testing.T) {
	tok, err := NewTokenIssuer(testKey, testSecret, testURL).Issue("channel-id", "account-id", "Anna", false)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	g := verify(t, tok.Value)
	if g.Video.CanPublish == nil || *g.Video.CanPublish {
		t.Error("a listener must not be allowed to publish")
	}
	if g.Video.CanSubscribe == nil || !*g.Video.CanSubscribe {
		t.Error("a listener must still be allowed to subscribe")
	}
}

func TestIssueKeepsOneIdentityPerAccount(t *testing.T) {
	i := NewTokenIssuer(testKey, testSecret, testURL)
	a, errA := i.Issue("room", "account-id", "Anna", true)
	b, errB := i.Issue("room", "account-id", "Anna", true)
	if errA != nil || errB != nil {
		t.Fatalf("Issue() errors = %v, %v", errA, errB)
	}
	if a.Identity != b.Identity {
		t.Errorf("two tokens for one account have identities %q and %q", a.Identity, b.Identity)
	}
}

func TestIssueValidation(t *testing.T) {
	tests := []struct {
		name, room, identity, displayName, wantMsg string
	}{
		{"empty room", "", "account", "Anna", "room must be"},
		{"room with slash", "a/b", "account", "Anna", "room must be"},
		{"room starts with dash", "-room", "account", "Anna", "room must be"},
		{"room too long", strings.Repeat("a", 65), "account", "Anna", "room must be"},
		{"empty identity", "room", "", "Anna", "identity must be"},
		{"identity too long", "room", strings.Repeat("a", 65), "Anna", "identity must be"},
		{"blank name", "room", "account", "   ", "displayName is required"},
		{"name too long", "room", "account", strings.Repeat("я", 33), "must not exceed 32"},
		{"control characters", "room", "account", "An\u0000na", "control characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewTokenIssuer(testKey, testSecret, testURL).Issue(tt.room, tt.identity, tt.displayName, true)
			if !IsValidationError(err) {
				t.Fatalf("Issue() error = %v, want validation error", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not contain %q", err, tt.wantMsg)
			}
		})
	}
}

func TestIssueAcceptsLimits(t *testing.T) {
	_, err := NewTokenIssuer(testKey, testSecret, testURL).Issue(
		strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("я", 32), true)
	if err != nil {
		t.Errorf("Issue() at limits error = %v", err)
	}
}
