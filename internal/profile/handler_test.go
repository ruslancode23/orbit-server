package profile

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

func sessionFor(userID uuid.UUID) context.Context {
	return auth.WithSession(context.Background(), auth.Session{
		ID:   uuid.New(),
		User: auth.User{ID: userID, Nickname: "anna"},
	})
}

func TestHandlerRequiresASession(t *testing.T) {
	h := NewHandler(newHarness(t).svc, testLogger())
	ctx := context.Background()

	t.Run("update profile", func(t *testing.T) {
		resp, err := h.UpdateMyProfile(ctx, api.UpdateMyProfileRequestObject{Body: &api.ProfileUpdate{}})
		if err != nil {
			t.Fatalf("UpdateMyProfile: %v", err)
		}
		if _, ok := resp.(api.UpdateMyProfile401JSONResponse); !ok {
			t.Errorf("response = %T, want 401", resp)
		}
	})

	t.Run("set status", func(t *testing.T) {
		resp, err := h.SetMyStatus(ctx, api.SetMyStatusRequestObject{Body: &api.StatusUpdate{}})
		if err != nil {
			t.Fatalf("SetMyStatus: %v", err)
		}
		if _, ok := resp.(api.SetMyStatus401JSONResponse); !ok {
			t.Errorf("response = %T, want 401", resp)
		}
	})

	t.Run("clear status", func(t *testing.T) {
		resp, err := h.ClearMyStatus(ctx, api.ClearMyStatusRequestObject{})
		if err != nil {
			t.Fatalf("ClearMyStatus: %v", err)
		}
		if _, ok := resp.(api.ClearMyStatus401JSONResponse); !ok {
			t.Errorf("response = %T, want 401", resp)
		}
	})

	t.Run("get profile", func(t *testing.T) {
		resp, err := h.GetUserProfile(ctx, api.GetUserProfileRequestObject{UserId: uuid.New()})
		if err != nil {
			t.Fatalf("GetUserProfile: %v", err)
		}
		if _, ok := resp.(api.GetUserProfile401JSONResponse); !ok {
			t.Errorf("response = %T, want 401", resp)
		}
	})

	t.Run("complete onboarding", func(t *testing.T) {
		resp, err := h.CompleteMyProfileOnboarding(ctx, api.CompleteMyProfileOnboardingRequestObject{})
		if err != nil {
			t.Fatalf("CompleteMyProfileOnboarding: %v", err)
		}
		if _, ok := resp.(api.CompleteMyProfileOnboarding401JSONResponse); !ok {
			t.Errorf("response = %T, want 401", resp)
		}
	})
}

func TestHandlerUpdatesProfile(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	h.seed(user, "anna")
	handler := NewHandler(h.svc, testLogger())

	resp, err := handler.UpdateMyProfile(sessionFor(user), api.UpdateMyProfileRequestObject{
		Body: &api.ProfileUpdate{
			DisplayName: ptr("Anna"),
			Bio:         ptr("hello"),
			AccentColor: ptr(api.Sky),
		},
	})
	if err != nil {
		t.Fatalf("UpdateMyProfile: %v", err)
	}
	updated, ok := resp.(api.UpdateMyProfile200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", resp)
	}
	if updated.DisplayName != "Anna" || updated.Bio != "hello" || updated.AccentColor != api.Sky {
		t.Errorf("profile = %+v, want the sent fields", updated)
	}
	if updated.Nickname != "anna" {
		t.Errorf("nickname = %q, want it unchanged", updated.Nickname)
	}
}

func TestHandlerRejectsInvalidProfile(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	h.seed(user, "anna")
	handler := NewHandler(h.svc, testLogger())

	t.Run("too long", func(t *testing.T) {
		resp, err := handler.UpdateMyProfile(sessionFor(user), api.UpdateMyProfileRequestObject{
			Body: &api.ProfileUpdate{DisplayName: ptr(strings.Repeat("я", MaxDisplayNameLength+1))},
		})
		if err != nil {
			t.Fatalf("UpdateMyProfile: %v", err)
		}
		bad, ok := resp.(api.UpdateMyProfile400JSONResponse)
		if !ok {
			t.Fatalf("response = %T, want 400", resp)
		}
		if bad.Error.Code != httpx.CodeInvalidRequest || bad.Error.Message == "" {
			t.Errorf("error = %+v, want invalid_request with a message", bad.Error)
		}
	})

	t.Run("zero-width", func(t *testing.T) {
		resp, err := handler.UpdateMyProfile(sessionFor(user), api.UpdateMyProfileRequestObject{
			Body: &api.ProfileUpdate{DisplayName: ptr("An\u200Bna")},
		})
		if err != nil {
			t.Fatalf("UpdateMyProfile: %v", err)
		}
		if _, ok := resp.(api.UpdateMyProfile400JSONResponse); !ok {
			t.Fatalf("response = %T, want 400", resp)
		}
	})

	t.Run("no body", func(t *testing.T) {
		resp, err := handler.UpdateMyProfile(sessionFor(user), api.UpdateMyProfileRequestObject{})
		if err != nil {
			t.Fatalf("UpdateMyProfile: %v", err)
		}
		if _, ok := resp.(api.UpdateMyProfile400JSONResponse); !ok {
			t.Fatalf("response = %T, want 400", resp)
		}
	})

	t.Run("nothing to update", func(t *testing.T) {
		resp, err := handler.UpdateMyProfile(sessionFor(user), api.UpdateMyProfileRequestObject{
			Body: &api.ProfileUpdate{},
		})
		if err != nil {
			t.Fatalf("UpdateMyProfile: %v", err)
		}
		if _, ok := resp.(api.UpdateMyProfile400JSONResponse); !ok {
			t.Fatalf("response = %T, want 400", resp)
		}
	})
}

func TestHandlerSetsAndClearsStatus(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	h.seed(user, "anna")
	handler := NewHandler(h.svc, testLogger())

	resp, err := handler.SetMyStatus(sessionFor(user), api.SetMyStatusRequestObject{
		Body: &api.StatusUpdate{Emoji: ptr("🎧"), Text: ptr("на связи"), ExpiresIn: ptr(api.N30m)},
	})
	if err != nil {
		t.Fatalf("SetMyStatus: %v", err)
	}
	set, ok := resp.(api.SetMyStatus200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", resp)
	}
	if set.CustomStatus == nil || set.CustomStatus.Emoji == nil || *set.CustomStatus.Emoji != "🎧" {
		t.Fatalf("custom status = %+v, want the emoji", set.CustomStatus)
	}
	if set.CustomStatus.ExpiresAt == nil {
		t.Fatal("expiresAt is missing, want the server-computed deadline")
	}

	clearResp, err := handler.ClearMyStatus(sessionFor(user), api.ClearMyStatusRequestObject{})
	if err != nil {
		t.Fatalf("ClearMyStatus: %v", err)
	}
	cleared, ok := clearResp.(api.ClearMyStatus200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", clearResp)
	}
	if cleared.CustomStatus != nil {
		t.Errorf("custom status = %+v, want it removed", cleared.CustomStatus)
	}
}

// Both "save" and "skip" end here: the first call marks the step, and a retry
// answers the same 204 without moving the timestamp.
func TestHandlerMarksOnboardingOnce(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	h.seed(user, "anna")
	handler := NewHandler(h.svc, testLogger())

	resp, err := handler.CompleteMyProfileOnboarding(sessionFor(user), api.CompleteMyProfileOnboardingRequestObject{})
	if err != nil {
		t.Fatalf("CompleteMyProfileOnboarding: %v", err)
	}
	if _, ok := resp.(api.CompleteMyProfileOnboarding204Response); !ok {
		t.Fatalf("response = %T, want 204", resp)
	}
	first := h.store.profiles[user].ProfileOnboardedAt
	if first == nil || !first.Equal(h.clock.Now()) {
		t.Fatalf("onboardedAt = %v, want %v", first, h.clock.Now())
	}

	h.clock.advance(time.Hour)
	resp, err = handler.CompleteMyProfileOnboarding(sessionFor(user), api.CompleteMyProfileOnboardingRequestObject{})
	if err != nil {
		t.Fatalf("repeated CompleteMyProfileOnboarding: %v", err)
	}
	if _, ok := resp.(api.CompleteMyProfileOnboarding204Response); !ok {
		t.Fatalf("response = %T, want 204 again", resp)
	}
	if again := h.store.profiles[user].ProfileOnboardedAt; again == nil || !again.Equal(*first) {
		t.Errorf("onboardedAt = %v, want the first timestamp %v", again, *first)
	}
}

func TestHandlerRefusesOnboardingForAGuest(t *testing.T) {
	h := newHarness(t)
	guest := uuid.New()
	seed := h.seed(guest, "Guest")
	seed.IsGuest = true
	h.store.profiles[guest] = seed
	handler := NewHandler(h.svc, testLogger())

	resp, err := handler.CompleteMyProfileOnboarding(sessionFor(guest), api.CompleteMyProfileOnboardingRequestObject{})
	if err != nil {
		t.Fatalf("CompleteMyProfileOnboarding: %v", err)
	}
	refused, ok := resp.(api.CompleteMyProfileOnboarding403JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 403", resp)
	}
	if refused.Error.Code != httpx.CodeForbidden || refused.Error.Message == "" {
		t.Errorf("error = %+v, want forbidden with a message", refused.Error)
	}
	if at := h.store.profiles[guest].ProfileOnboardedAt; at != nil {
		t.Errorf("onboardedAt = %v, want a guest to stay unpassed", at)
	}
}

func TestHandlerRejectsInvalidStatus(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	h.seed(user, "anna")
	handler := NewHandler(h.svc, testLogger())

	for name, body := range map[string]*api.StatusUpdate{
		"empty":           {},
		"too long text":   {Text: ptr(strings.Repeat("я", MaxStatusTextLength+1))},
		"control in text": {Text: ptr("a\nb")},
		"unknown expiry":  {Text: ptr("hello"), ExpiresIn: ptr(api.StatusUpdateExpiresIn("1h"))},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := handler.SetMyStatus(sessionFor(user), api.SetMyStatusRequestObject{Body: body})
			if err != nil {
				t.Fatalf("SetMyStatus: %v", err)
			}
			if _, ok := resp.(api.SetMyStatus400JSONResponse); !ok {
				t.Fatalf("response = %T, want 400", resp)
			}
		})
	}
}

func TestHandlerProfileVisibility(t *testing.T) {
	h := newHarness(t)
	anna, dina, stranger := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{anna, dina, stranger} {
		h.seed(id, "user")
	}
	h.friends.pairs[pairKey(anna, dina)] = true
	handler := NewHandler(h.svc, testLogger())

	t.Run("self", func(t *testing.T) {
		resp, err := handler.GetUserProfile(sessionFor(anna), api.GetUserProfileRequestObject{UserId: anna})
		if err != nil {
			t.Fatalf("GetUserProfile: %v", err)
		}
		if _, ok := resp.(api.GetUserProfile200JSONResponse); !ok {
			t.Fatalf("response = %T, want 200", resp)
		}
	})

	t.Run("friend", func(t *testing.T) {
		resp, err := handler.GetUserProfile(sessionFor(anna), api.GetUserProfileRequestObject{UserId: dina})
		if err != nil {
			t.Fatalf("GetUserProfile: %v", err)
		}
		if _, ok := resp.(api.GetUserProfile200JSONResponse); !ok {
			t.Fatalf("response = %T, want 200", resp)
		}
	})

	t.Run("stranger", func(t *testing.T) {
		resp, err := handler.GetUserProfile(sessionFor(anna), api.GetUserProfileRequestObject{UserId: stranger})
		if err != nil {
			t.Fatalf("GetUserProfile: %v", err)
		}
		missing, ok := resp.(api.GetUserProfile404JSONResponse)
		if !ok {
			t.Fatalf("response = %T, want 404", resp)
		}
		if missing.Error.Code != httpx.CodeNotFound {
			t.Errorf("code = %q, want not_found", missing.Error.Code)
		}
	})

	t.Run("unknown account", func(t *testing.T) {
		resp, err := handler.GetUserProfile(sessionFor(anna), api.GetUserProfileRequestObject{UserId: uuid.New()})
		if err != nil {
			t.Fatalf("GetUserProfile: %v", err)
		}
		if _, ok := resp.(api.GetUserProfile404JSONResponse); !ok {
			t.Fatalf("response = %T, want 404", resp)
		}
	})
}

func TestHandlerHidesAnExpiredStatus(t *testing.T) {
	h := newHarness(t)
	user := uuid.New()
	seed := h.seed(user, "anna")
	past := h.clock.Now().Add(-time.Minute)
	seed.CustomStatus = &CustomStatus{Text: "gone", ExpiresAt: &past}
	h.store.profiles[user] = seed
	handler := NewHandler(h.svc, testLogger())

	resp, err := handler.GetUserProfile(sessionFor(user), api.GetUserProfileRequestObject{UserId: user})
	if err != nil {
		t.Fatalf("GetUserProfile: %v", err)
	}
	got, ok := resp.(api.GetUserProfile200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", resp)
	}
	if got.CustomStatus != nil {
		t.Fatalf("custom status = %+v, want it hidden", got.CustomStatus)
	}
}

func avatarRequest(t *testing.T, field string, data []byte) *multipart.Reader {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(field, "avatar.png")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
}

func TestHandlerUploadsAvatar(t *testing.T) {
	h, _ := newAvatarHarness(t, 8<<20)
	user := uuid.New()
	h.seed(user, "anna")
	handler := NewHandler(h.svc, testLogger())

	resp, err := handler.UploadMyAvatar(sessionFor(user), api.UploadMyAvatarRequestObject{
		Body: avatarRequest(t, "file", pngAvatar(t, 40, 40)),
	})
	if err != nil {
		t.Fatalf("UploadMyAvatar: %v", err)
	}
	updated, ok := resp.(api.UploadMyAvatar200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", resp)
	}
	if updated.AvatarUrl == nil || *updated.AvatarUrl == "" {
		t.Error("the response has no avatar link")
	}
}

func TestHandlerAvatarErrors(t *testing.T) {
	h, _ := newAvatarHarness(t, 1024)
	user := uuid.New()
	h.seed(user, "anna")
	handler := NewHandler(h.svc, testLogger())

	tests := []struct {
		name string
		body *multipart.Reader
		want any
	}{
		{
			name: "wrong field",
			body: avatarRequest(t, "image", pngAvatar(t, 10, 10)),
			want: api.UploadMyAvatar400JSONResponse{},
		},
		{
			name: "not an image",
			body: avatarRequest(t, "file", []byte("<html>hello</html>")),
			want: api.UploadMyAvatar415JSONResponse{},
		},
		{
			name: "too large",
			body: avatarRequest(t, "file", append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 2048)...)),
			want: api.UploadMyAvatar413JSONResponse{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := handler.UploadMyAvatar(sessionFor(user), api.UploadMyAvatarRequestObject{Body: tt.body})
			if err != nil {
				t.Fatalf("UploadMyAvatar: %v", err)
			}
			if got, want := typeName(resp), typeName(tt.want); got != want {
				t.Errorf("response = %s, want %s", got, want)
			}
		})
	}
}

func TestHandlerDeleteAvatarNeedsASession(t *testing.T) {
	h, _ := newAvatarHarness(t, 8<<20)
	handler := NewHandler(h.svc, testLogger())

	resp, err := handler.DeleteMyAvatar(context.Background(), api.DeleteMyAvatarRequestObject{})
	if err != nil {
		t.Fatalf("DeleteMyAvatar: %v", err)
	}
	if _, ok := resp.(api.DeleteMyAvatar401JSONResponse); !ok {
		t.Errorf("response = %T, want 401", resp)
	}
}

func typeName(value any) string {
	return strings.TrimPrefix(strings.TrimPrefix(fmt.Sprintf("%T", value), "*"), "api.")
}
