package invites

import (
	"io"
	"log/slog"
	"testing"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/clientip"
	"github.com/ruslancode23/orbit-server/internal/ratelimit"
)

type plainCookies struct{}

func (plainCookies) SessionCookie(auth.Credentials) string { return "orbit_session=test" }

// The guest entrance creates an account, so it spends the same per-address
// budget as registration; another address keeps its own.
func TestJoinAsGuestPerAddress(t *testing.T) {
	w := newWorld(t)
	h := NewHandler(w.svc, plainCookies{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.SetSignupLimiter(ratelimit.NewHourly(1))

	join := func(addr, nickname string) api.JoinAsGuestResponseObject {
		t.Helper()
		resp, err := h.JoinAsGuest(clientip.WithAddr(t.Context(), addr), api.JoinAsGuestRequestObject{
			Code: w.code,
			Body: &api.JoinAsGuestJSONRequestBody{Nickname: nickname},
		})
		if err != nil {
			t.Fatalf("JoinAsGuest: %v", err)
		}
		return resp
	}

	if _, ok := join("203.0.113.1", "Аня").(api.JoinAsGuest201JSONResponse); !ok {
		t.Fatal("the first guest was refused")
	}
	limited, ok := join("203.0.113.1", "Боря").(api.JoinAsGuest429JSONResponse)
	if !ok {
		t.Fatal("a second guest from the same address passed a limit of one")
	}
	if limited.Headers.RetryAfter == nil || *limited.Headers.RetryAfter < 1 {
		t.Errorf("Retry-After = %v, want a positive wait", limited.Headers.RetryAfter)
	}
	if _, ok := join("203.0.113.2", "Боря").(api.JoinAsGuest201JSONResponse); !ok {
		t.Error("another address was limited too")
	}
}
