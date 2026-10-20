package privacy

import (
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
)

func newHandler(t *testing.T, w *world) *Handler {
	t.Helper()
	return NewHandler(w.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// The list endpoint has no user argument: it always reads the session account,
// so one person cannot ask for another person's blocks.
func TestHandlerListsOnlyTheSessionAccountsBlocks(t *testing.T) {
	w := newWorld(t)
	w.store.block(w.anna, w.bob)
	w.store.block(w.vika, w.bob)
	h := newHandler(t, w)
	ctx := auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: w.anna}})

	response, err := h.ListUserBlocks(ctx, api.ListUserBlocksRequestObject{})
	if err != nil {
		t.Fatalf("ListUserBlocks: %v", err)
	}
	list, ok := response.(api.ListUserBlocks200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", response)
	}
	if len(list) != 1 || list[0].Id != w.bob {
		t.Errorf("blocks = %+v, want only Bob", list)
	}

	// No session, no list.
	withoutSession, err := h.ListUserBlocks(t.Context(), api.ListUserBlocksRequestObject{})
	if err != nil {
		t.Fatalf("ListUserBlocks without a session: %v", err)
	}
	if _, ok := withoutSession.(api.ListUserBlocks401JSONResponse); !ok {
		t.Errorf("response = %T, want 401", withoutSession)
	}
}

// Blocking yourself is a client mistake, not a server failure: 400.
func TestHandlerRefusesBlockingYourself(t *testing.T) {
	w := newWorld(t)
	h := newHandler(t, w)
	ctx := auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: w.anna}})

	response, err := h.BlockUser(ctx, api.BlockUserRequestObject{UserId: w.anna})
	if err != nil {
		t.Fatalf("BlockUser: %v", err)
	}
	if _, ok := response.(api.BlockUser400JSONResponse); !ok {
		t.Errorf("response = %T, want 400", response)
	}

	// An unknown account is a 404, like every other refusal.
	response, err = h.BlockUser(ctx, api.BlockUserRequestObject{UserId: uuid.New()})
	if err != nil {
		t.Fatalf("BlockUser: %v", err)
	}
	if _, ok := response.(api.BlockUser404JSONResponse); !ok {
		t.Errorf("response = %T, want 404", response)
	}
}

// The policy endpoint writes and reads the session account's own setting.
func TestHandlerSetsTheSessionAccountsPolicy(t *testing.T) {
	w := newWorld(t)
	h := newHandler(t, w)
	ctx := auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: w.anna}})
	body := api.DmPolicyUpdate{DmPolicy: api.Nobody}

	response, err := h.SetDmPolicy(ctx, api.SetDmPolicyRequestObject{Body: &body})
	if err != nil {
		t.Fatalf("SetDmPolicy: %v", err)
	}
	setting, ok := response.(api.SetDmPolicy200JSONResponse)
	if !ok || setting.DmPolicy != api.Nobody {
		t.Fatalf("response = %#v, want nobody", response)
	}

	got, err := h.GetDmPolicy(ctx, api.GetDmPolicyRequestObject{})
	if err != nil {
		t.Fatalf("GetDmPolicy: %v", err)
	}
	if current, ok := got.(api.GetDmPolicy200JSONResponse); !ok || current.DmPolicy != api.Nobody {
		t.Errorf("policy = %#v, want nobody", got)
	}
}
