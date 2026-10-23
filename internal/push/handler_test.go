package push

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
)

func signedIn(userID uuid.UUID) context.Context {
	return auth.WithSession(context.Background(), auth.Session{ID: uuid.New(), User: auth.User{ID: userID}})
}

func subscriptionBody(endpoint string) *api.PushSubscriptionInput {
	body := &api.PushSubscriptionInput{Endpoint: endpoint}
	body.Keys.P256dh = "p256dh"
	body.Keys.Auth = "auth"
	return body
}

func TestHandlerNeedsASession(t *testing.T) {
	h := NewHandler(NewService(newFakeStore(), nil, newFakeSender(), "key", discard()), discard())
	ctx := context.Background()

	if res, _ := h.GetPushConfig(ctx, api.GetPushConfigRequestObject{}); res == nil {
		t.Fatal("no response")
	} else if _, ok := res.(api.GetPushConfig401JSONResponse); !ok {
		t.Errorf("GetPushConfig = %T, want 401", res)
	}
	res, _ := h.CreatePushSubscription(ctx, api.CreatePushSubscriptionRequestObject{Body: subscriptionBody(fcm + "a")})
	if _, ok := res.(api.CreatePushSubscription401JSONResponse); !ok {
		t.Errorf("CreatePushSubscription = %T, want 401", res)
	}
	del, _ := h.DeletePushSubscription(ctx, api.DeletePushSubscriptionRequestObject{SubscriptionId: uuid.New()})
	if _, ok := del.(api.DeletePushSubscription401JSONResponse); !ok {
		t.Errorf("DeletePushSubscription = %T, want 401", del)
	}
}

func TestHandlerTellsWhenPushIsOff(t *testing.T) {
	h := NewHandler(NewService(newFakeStore(), nil, nil, "", discard()), discard())
	ctx := signedIn(uuid.New())

	res, _ := h.GetPushConfig(ctx, api.GetPushConfigRequestObject{})
	config, ok := res.(api.GetPushConfig200JSONResponse)
	if !ok || config.Enabled || config.PublicKey != nil {
		t.Fatalf("GetPushConfig = %#v, want 200 with push off and no key", res)
	}
	created, _ := h.CreatePushSubscription(ctx, api.CreatePushSubscriptionRequestObject{Body: subscriptionBody(fcm + "a")})
	if _, ok := created.(api.CreatePushSubscription503JSONResponse); !ok {
		t.Fatalf("CreatePushSubscription = %T, want 503", created)
	}
}

func TestHandlerSubscribesAndUnsubscribes(t *testing.T) {
	h := NewHandler(NewService(newFakeStore(), nil, newFakeSender(), "public-key", discard()), discard())
	anna, boris := uuid.New(), uuid.New()

	res, _ := h.GetPushConfig(signedIn(anna), api.GetPushConfigRequestObject{})
	if config, ok := res.(api.GetPushConfig200JSONResponse); !ok || !config.Enabled || config.PublicKey == nil || *config.PublicKey != "public-key" {
		t.Fatalf("GetPushConfig = %#v, want push on with the key", res)
	}

	bad, _ := h.CreatePushSubscription(signedIn(anna), api.CreatePushSubscriptionRequestObject{Body: subscriptionBody("https://169.254.169.254/x")})
	if _, ok := bad.(api.CreatePushSubscription400JSONResponse); !ok {
		t.Fatalf("an internal endpoint = %T, want 400", bad)
	}

	created, _ := h.CreatePushSubscription(signedIn(anna), api.CreatePushSubscriptionRequestObject{Body: subscriptionBody(fcm + "a")})
	sub, ok := created.(api.CreatePushSubscription201JSONResponse)
	if !ok {
		t.Fatalf("CreatePushSubscription = %T, want 201", created)
	}

	foreign, _ := h.DeletePushSubscription(signedIn(boris), api.DeletePushSubscriptionRequestObject{SubscriptionId: sub.Id})
	if _, ok := foreign.(api.DeletePushSubscription404JSONResponse); !ok {
		t.Fatalf("someone else's subscription = %T, want 404", foreign)
	}
	own, _ := h.DeletePushSubscription(signedIn(anna), api.DeletePushSubscriptionRequestObject{SubscriptionId: sub.Id})
	if _, ok := own.(api.DeletePushSubscription204Response); !ok {
		t.Fatalf("own subscription = %T, want 204", own)
	}
}
