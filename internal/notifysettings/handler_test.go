package notifysettings

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
)

func signedIn(userID uuid.UUID) context.Context {
	return auth.WithSession(context.Background(), auth.Session{ID: uuid.New(), User: auth.User{ID: userID}})
}

func newHandler() (*Handler, *fakeChannels, uuid.UUID) {
	svc, _, access, _, channelID := newWorld()
	return NewHandler(svc, slog.New(slog.DiscardHandler)), access, channelID
}

func TestHandlerNeedsASession(t *testing.T) {
	h, _, _ := newHandler()
	ctx := context.Background()

	res, _ := h.ListNotificationSettings(ctx, api.ListNotificationSettingsRequestObject{})
	if _, ok := res.(api.ListNotificationSettings401JSONResponse); !ok {
		t.Errorf("ListNotificationSettings = %T, want 401", res)
	}
	set, _ := h.SetNotificationSetting(ctx, api.SetNotificationSettingRequestObject{})
	if _, ok := set.(api.SetNotificationSetting401JSONResponse); !ok {
		t.Errorf("SetNotificationSetting = %T, want 401", set)
	}
	del, _ := h.DeleteNotificationSetting(ctx, api.DeleteNotificationSettingRequestObject{})
	if _, ok := del.(api.DeleteNotificationSetting401JSONResponse); !ok {
		t.Errorf("DeleteNotificationSetting = %T, want 401", del)
	}
}

func TestHandlerStoresAndListsLevels(t *testing.T) {
	h, access, channelID := newHandler()
	anna := uuid.New()
	access.readers[channelID] = map[uuid.UUID]bool{anna: true}
	level := api.NotificationSettingUpdateLevelAll

	set, err := h.SetNotificationSetting(signedIn(anna), api.SetNotificationSettingRequestObject{
		Scope:    api.NotificationScopeChannel,
		TargetId: channelID,
		Body:     &api.NotificationSettingUpdate{Level: level},
	})
	if err != nil {
		t.Fatalf("SetNotificationSetting: %v", err)
	}
	stored, ok := set.(api.SetNotificationSetting200JSONResponse)
	if !ok || stored.Level != api.NotificationSettingLevelAll {
		t.Fatalf("SetNotificationSetting = %#v, want the stored level", set)
	}

	list, err := h.ListNotificationSettings(signedIn(anna), api.ListNotificationSettingsRequestObject{})
	if err != nil {
		t.Fatalf("ListNotificationSettings: %v", err)
	}
	page, ok := list.(api.ListNotificationSettings200JSONResponse)
	if !ok || len(page.Items) != 1 || page.Items[0].TargetId != channelID {
		t.Fatalf("ListNotificationSettings = %#v, want the one row", list)
	}

	del, err := h.DeleteNotificationSetting(signedIn(anna), api.DeleteNotificationSettingRequestObject{
		Scope: api.NotificationScopeChannel, TargetId: channelID,
	})
	if err != nil {
		t.Fatalf("DeleteNotificationSetting: %v", err)
	}
	if _, ok := del.(api.DeleteNotificationSetting204Response); !ok {
		t.Fatalf("DeleteNotificationSetting = %T, want 204", del)
	}
	again, _ := h.DeleteNotificationSetting(signedIn(anna), api.DeleteNotificationSettingRequestObject{
		Scope: api.NotificationScopeChannel, TargetId: channelID,
	})
	if _, ok := again.(api.DeleteNotificationSetting404JSONResponse); !ok {
		t.Fatalf("the second delete = %T, want 404", again)
	}
}

func TestHandlerAnswers404ForAHiddenChannel(t *testing.T) {
	h, _, channelID := newHandler()
	res, _ := h.SetNotificationSetting(signedIn(uuid.New()), api.SetNotificationSettingRequestObject{
		Scope:    api.NotificationScopeChannel,
		TargetId: channelID,
		Body:     &api.NotificationSettingUpdate{Level: api.NotificationSettingUpdateLevelInherit},
	})
	if _, ok := res.(api.SetNotificationSetting404JSONResponse); !ok {
		t.Fatalf("SetNotificationSetting = %T, want 404", res)
	}
}
