package friends

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
)

func sessionFor(userID uuid.UUID) context.Context {
	return auth.WithSession(context.Background(), auth.Session{User: auth.User{ID: userID}})
}

func newTestHandler(w *world) *Handler { return NewHandler(w.svc, testLogger()) }

func TestHandlerNeutralAnswerForEveryRefusal(t *testing.T) {
	w := newWorld(t)
	h := newTestHandler(w)
	ctx := sessionFor(w.anna)

	responses := make([]api.CreateFriendRequestResponseObject, 0, 4)
	// A request that is stored.
	responses = append(responses, mustCreate(t, h, ctx, "Bob"))
	// No such account.
	responses = append(responses, mustCreate(t, h, ctx, "Nobody"))
	// A block in the other direction.
	w.privacy.blocked[w.bob.String()+":"+w.anna.String()] = true
	responses = append(responses, mustCreate(t, h, ctx, "Vika"))
	w.privacy.blocked = map[string]bool{}
	// A guest sends into the void.
	guestCtx := sessionFor(w.guest)
	responses = append(responses, mustCreate(t, h, guestCtx, "Bob"))

	message := ""
	for i, response := range responses {
		accepted, ok := response.(api.CreateFriendRequest202JSONResponse)
		if !ok {
			t.Fatalf("response %d = %T, want 202", i, response)
		}
		if i == 0 {
			message = accepted.Message
			continue
		}
		if accepted.Message != message {
			t.Errorf("response %d message = %q, want %q", i, accepted.Message, message)
		}
	}
	if message != requestAcceptedMessage {
		t.Errorf("message = %q, want the neutral one", message)
	}
}

func mustCreate(t *testing.T, h *Handler, ctx context.Context, nickname string) api.CreateFriendRequestResponseObject {
	t.Helper()
	body := api.FriendRequestCreate{Nickname: nickname}
	response, err := h.CreateFriendRequest(ctx, api.CreateFriendRequestRequestObject{Body: &body})
	if err != nil {
		t.Fatalf("CreateFriendRequest: %v", err)
	}
	return response
}

func TestHandlerRequiresSession(t *testing.T) {
	w := newWorld(t)
	h := newTestHandler(w)

	body := api.FriendRequestCreate{Nickname: "Bob"}
	response, err := h.CreateFriendRequest(context.Background(), api.CreateFriendRequestRequestObject{Body: &body})
	if err != nil {
		t.Fatalf("CreateFriendRequest: %v", err)
	}
	if _, ok := response.(api.CreateFriendRequest401JSONResponse); !ok {
		t.Errorf("response = %T, want 401", response)
	}

	list, err := h.ListFriends(context.Background(), api.ListFriendsRequestObject{})
	if err != nil {
		t.Fatalf("ListFriends: %v", err)
	}
	if _, ok := list.(api.ListFriends401JSONResponse); !ok {
		t.Errorf("response = %T, want 401", list)
	}
}

func TestHandlerRejectsAnEmptyBody(t *testing.T) {
	w := newWorld(t)
	h := newTestHandler(w)
	response, err := h.CreateFriendRequest(sessionFor(w.anna), api.CreateFriendRequestRequestObject{})
	if err != nil {
		t.Fatalf("CreateFriendRequest: %v", err)
	}
	if _, ok := response.(api.CreateFriendRequest400JSONResponse); !ok {
		t.Errorf("response = %T, want 400", response)
	}
}

// The daily bucket and the pending limit both answer 429 with a Retry-After.
func TestHandlerRateLimitsWithRetryAfter(t *testing.T) {
	t.Run("daily bucket", func(t *testing.T) {
		w := newWorld(t)
		w.svc.SetLimiter(&fakeLimiter{allowed: false, retry: 90 * time.Second})
		response := mustCreate(t, newTestHandler(w), sessionFor(w.anna), "Bob")
		limited, ok := response.(api.CreateFriendRequest429JSONResponse)
		if !ok {
			t.Fatalf("response = %T, want 429", response)
		}
		if limited.Headers.RetryAfter == nil || *limited.Headers.RetryAfter != 90 {
			t.Errorf("Retry-After = %v, want 90", limited.Headers.RetryAfter)
		}
	})

	t.Run("pending limit", func(t *testing.T) {
		w := newWorld(t)
		w.svc.limits.MaxPending = 1
		h := newTestHandler(w)
		ctx := sessionFor(w.anna)
		if _, ok := mustCreate(t, h, ctx, "Bob").(api.CreateFriendRequest202JSONResponse); !ok {
			t.Fatalf("first request did not get 202")
		}
		response := mustCreate(t, h, ctx, "Vika")
		limited, ok := response.(api.CreateFriendRequest429JSONResponse)
		if !ok {
			t.Fatalf("response = %T, want 429", response)
		}
		if limited.Headers.RetryAfter == nil || *limited.Headers.RetryAfter < 1 {
			t.Errorf("Retry-After = %v, want a positive number of seconds", limited.Headers.RetryAfter)
		}
	})
}

func TestHandlerFriendLimitIs409(t *testing.T) {
	w := newWorld(t)
	w.svc.limits.MaxFriends = 1
	w.friend(w.bob, w.vika)
	_ = w.svc.Request(t.Context(), w.anna, "Bob")

	response, err := newTestHandler(w).AcceptFriendRequest(sessionFor(w.bob), api.AcceptFriendRequestRequestObject{UserId: w.anna})
	if err != nil {
		t.Fatalf("AcceptFriendRequest: %v", err)
	}
	if _, ok := response.(api.AcceptFriendRequest409JSONResponse); !ok {
		t.Fatalf("response = %T, want 409", response)
	}
}

func TestHandlerListsOnlyTheCallersRequests(t *testing.T) {
	w := newWorld(t)
	_ = w.svc.Request(t.Context(), w.anna, "Bob")
	h := newTestHandler(w)

	outgoing, err := h.ListFriendRequests(sessionFor(w.anna), api.ListFriendRequestsRequestObject{
		Params: api.ListFriendRequestsParams{Direction: api.Outgoing},
	})
	if err != nil {
		t.Fatalf("ListFriendRequests: %v", err)
	}
	list, ok := outgoing.(api.ListFriendRequests200JSONResponse)
	if !ok || len(list) != 1 || list[0].UserId != w.bob || list[0].Direction != api.Outgoing {
		t.Errorf("Anna's outgoing = %#v, want Bob", outgoing)
	}

	incoming, err := h.ListFriendRequests(sessionFor(w.bob), api.ListFriendRequestsRequestObject{
		Params: api.ListFriendRequestsParams{Direction: api.Incoming},
	})
	if err != nil {
		t.Fatalf("ListFriendRequests: %v", err)
	}
	list, ok = incoming.(api.ListFriendRequests200JSONResponse)
	if !ok || len(list) != 1 || list[0].UserId != w.anna || list[0].Direction != api.Incoming {
		t.Errorf("Bob's incoming = %#v, want Anna", incoming)
	}

	// Nobody can ask for somebody else's list: the handler has no argument for it.
	bobOutgoing, err := h.ListFriendRequests(sessionFor(w.bob), api.ListFriendRequestsRequestObject{
		Params: api.ListFriendRequestsParams{Direction: api.Outgoing},
	})
	if err != nil {
		t.Fatalf("ListFriendRequests: %v", err)
	}
	if list, ok := bobOutgoing.(api.ListFriendRequests200JSONResponse); !ok || len(list) != 0 {
		t.Errorf("Bob's outgoing = %#v, want empty", bobOutgoing)
	}

	bad, err := h.ListFriendRequests(sessionFor(w.anna), api.ListFriendRequestsRequestObject{
		Params: api.ListFriendRequestsParams{Direction: api.FriendRequestDirection("sideways")},
	})
	if err != nil {
		t.Fatalf("ListFriendRequests: %v", err)
	}
	if _, ok := bad.(api.ListFriendRequests400JSONResponse); !ok {
		t.Errorf("response = %T, want 400", bad)
	}
}

func TestHandlerAcceptAndDeletePermissions(t *testing.T) {
	w := newWorld(t)
	_ = w.svc.Request(t.Context(), w.anna, "Bob")
	h := newTestHandler(w)

	// The author cannot accept their own request.
	author, err := h.AcceptFriendRequest(sessionFor(w.anna), api.AcceptFriendRequestRequestObject{UserId: w.bob})
	if err != nil {
		t.Fatalf("AcceptFriendRequest: %v", err)
	}
	if _, ok := author.(api.AcceptFriendRequest404JSONResponse); !ok {
		t.Errorf("author accept response = %T, want 404", author)
	}

	accepted, err := h.AcceptFriendRequest(sessionFor(w.bob), api.AcceptFriendRequestRequestObject{UserId: w.anna})
	if err != nil {
		t.Fatalf("AcceptFriendRequest: %v", err)
	}
	friend, ok := accepted.(api.AcceptFriendRequest200JSONResponse)
	if !ok || friend.Id != w.anna || friend.Nickname != "Anna" {
		t.Errorf("accepted = %#v, want Anna", accepted)
	}

	// The request is gone, so deleting it again is 404.
	gone, err := h.DeleteFriendRequest(sessionFor(w.anna), api.DeleteFriendRequestRequestObject{UserId: w.bob})
	if err != nil {
		t.Fatalf("DeleteFriendRequest: %v", err)
	}
	if _, ok := gone.(api.DeleteFriendRequest404JSONResponse); !ok {
		t.Errorf("response = %T, want 404", gone)
	}
}

func TestHandlerFriendsCarryPresence(t *testing.T) {
	w := newWorld(t)
	w.friend(w.anna, w.bob)
	w.presence.statuses[w.bob] = api.Dnd

	response, err := newTestHandler(w).ListFriends(sessionFor(w.anna), api.ListFriendsRequestObject{})
	if err != nil {
		t.Fatalf("ListFriends: %v", err)
	}
	list, ok := response.(api.ListFriends200JSONResponse)
	if !ok || len(list) != 1 {
		t.Fatalf("response = %#v, want one friend", response)
	}
	if list[0].Id != w.bob || list[0].Presence != api.Dnd || list[0].Nickname != "Bob" {
		t.Errorf("friend = %+v, want Bob dnd", list[0])
	}
}

func TestHandlerRemoveFriend(t *testing.T) {
	w := newWorld(t)
	w.friend(w.anna, w.bob)
	response, err := newTestHandler(w).RemoveFriend(sessionFor(w.anna), api.RemoveFriendRequestObject{UserId: w.bob})
	if err != nil {
		t.Fatalf("RemoveFriend: %v", err)
	}
	if _, ok := response.(api.RemoveFriend204Response); !ok {
		t.Errorf("response = %T, want 204", response)
	}
}

type fakeLimiter struct {
	allowed bool
	retry   time.Duration
}

func (f *fakeLimiter) Allow(uuid.UUID) (bool, time.Duration) {
	return f.allowed, f.retry
}
