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
	body := api.DmPolicyUpdate{DmPolicy: api.DmPolicyNobody}

	response, err := h.SetDmPolicy(ctx, api.SetDmPolicyRequestObject{Body: &body})
	if err != nil {
		t.Fatalf("SetDmPolicy: %v", err)
	}
	setting, ok := response.(api.SetDmPolicy200JSONResponse)
	if !ok || setting.DmPolicy != api.DmPolicyNobody {
		t.Fatalf("response = %#v, want nobody", response)
	}

	got, err := h.GetDmPolicy(ctx, api.GetDmPolicyRequestObject{})
	if err != nil {
		t.Fatalf("GetDmPolicy: %v", err)
	}
	if current, ok := got.(api.GetDmPolicy200JSONResponse); !ok || current.DmPolicy != api.DmPolicyNobody {
		t.Errorf("policy = %#v, want nobody", got)
	}
}

// The friendship request setting has its own endpoint next to the dm policy:
// they answer different questions, so one PUT cannot silently clear the other.
func TestHandlerSetsTheFriendRequestPolicy(t *testing.T) {
	w := newWorld(t)
	h := newHandler(t, w)
	ctx := auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: w.anna}})

	got, err := h.GetFriendRequestPolicy(ctx, api.GetFriendRequestPolicyRequestObject{})
	if err != nil {
		t.Fatalf("GetFriendRequestPolicy: %v", err)
	}
	setting, ok := got.(api.GetFriendRequestPolicy200JSONResponse)
	if !ok || setting.FriendRequestPolicy != api.FriendshipPolicyShared {
		t.Fatalf("default setting = %#v, want shared", got)
	}

	body := api.FriendRequestPolicyUpdate{FriendRequestPolicy: api.FriendshipPolicyAll}
	response, err := h.SetFriendRequestPolicy(ctx, api.SetFriendRequestPolicyRequestObject{Body: &body})
	if err != nil {
		t.Fatalf("SetFriendRequestPolicy: %v", err)
	}
	if setting, ok := response.(api.SetFriendRequestPolicy200JSONResponse); !ok || setting.FriendRequestPolicy != api.FriendshipPolicyAll {
		t.Fatalf("response = %#v, want all", response)
	}

	invalid := api.FriendRequestPolicyUpdate{FriendRequestPolicy: api.FriendshipPolicy("everyone")}
	response, err = h.SetFriendRequestPolicy(ctx, api.SetFriendRequestPolicyRequestObject{Body: &invalid})
	if err != nil {
		t.Fatalf("SetFriendRequestPolicy: %v", err)
	}
	if _, ok := response.(api.SetFriendRequestPolicy400JSONResponse); !ok {
		t.Errorf("response = %T, want 400", response)
	}

	withoutSession, err := h.GetFriendRequestPolicy(t.Context(), api.GetFriendRequestPolicyRequestObject{})
	if err != nil {
		t.Fatalf("GetFriendRequestPolicy without a session: %v", err)
	}
	if _, ok := withoutSession.(api.GetFriendRequestPolicy401JSONResponse); !ok {
		t.Errorf("response = %T, want 401", withoutSession)
	}
}
