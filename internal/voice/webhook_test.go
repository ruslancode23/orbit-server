package voice

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/protojson"

	"github.com/ruslancode23/orbit-server/internal/api"
)

func signedWebhook(t *testing.T, event *livekit.WebhookEvent) *http.Request {
	t.Helper()
	body, err := protojson.Marshal(event)
	if err != nil {
		t.Fatalf("marshal webhook: %v", err)
	}
	hash := sha256.Sum256(body)
	token, err := auth.NewAccessToken(testKey, testSecret).
		SetValidFor(time.Minute).
		SetSha256(base64.StdEncoding.EncodeToString(hash[:])).
		ToJWT()
	if err != nil {
		t.Fatalf("sign webhook: %v", err)
	}

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/livekit/webhook", bytes.NewReader(body))
	request.Header.Set("Authorization", token)
	return request
}

func TestWebhookUpdatesPresence(t *testing.T) {
	presence, rooms, publisher, _, channel := presenceFixture()
	rooms.participants[channel.String()] = []Participant{{UserID: uuid.New(), Name: "Anna"}}
	handler := NewWebhook(testKey, testSecret, presence, testLogger())

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, signedWebhook(t, &livekit.WebhookEvent{
		Event: "participant_joined",
		Room:  &livekit.Room{Name: channel.String()},
	}))

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", response.Code, response.Body)
	}
	if len(publisher.events) != 1 {
		t.Fatalf("events = %d, want 1", len(publisher.events))
	}
	payload, ok := publisher.events[0].Payload.(api.VoicePresence)
	if !ok || len(payload.Participants) != 1 || payload.Participants[0].Name != "Anna" {
		t.Errorf("payload = %#v, want Anna in the room", publisher.events[0].Payload)
	}
}

func TestWebhookRejectsForgedSignature(t *testing.T) {
	presence, _, publisher, _, channel := presenceFixture()
	handler := NewWebhook(testKey, testSecret, presence, testLogger())
	request := signedWebhook(t, &livekit.WebhookEvent{
		Event: "participant_joined",
		Room:  &livekit.Room{Name: channel.String()},
	})
	// The body no longer matches the checksum the token was signed for.
	request.Body = http.NoBody

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if len(publisher.events) != 0 {
		t.Error("a forged webhook must not change presence")
	}
}

func TestWebhookWithoutAuthorization(t *testing.T) {
	presence, _, _, _, _ := presenceFixture()
	handler := NewWebhook(testKey, testSecret, presence, testLogger())
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/livekit/webhook", bytes.NewReader(nil))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestWebhookRoomFinishedClearsPresence(t *testing.T) {
	presence, _, publisher, workspace, channel := presenceFixture()
	handler := NewWebhook(testKey, testSecret, presence, testLogger())

	for _, event := range []string{"participant_joined", "room_finished"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, signedWebhook(t, &livekit.WebhookEvent{
			Event: event,
			Room:  &livekit.Room{Name: channel.String()},
		}))
	}

	if len(publisher.events) != 2 {
		t.Fatalf("events = %d, want joined and cleared", len(publisher.events))
	}
	snapshot := presence.ForWorkspace(t.Context(), workspace)
	if len(snapshot) != 1 || len(snapshot[0].Participants) != 0 {
		t.Errorf("presence = %+v, want an empty room", snapshot)
	}
}

func TestWebhookIgnoresRoomsThatAreNotChannels(t *testing.T) {
	presence, _, publisher, _, _ := presenceFixture()
	handler := NewWebhook(testKey, testSecret, presence, testLogger())

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, signedWebhook(t, &livekit.WebhookEvent{
		Event: "participant_joined",
		Room:  &livekit.Room{Name: "dev-room"},
	}))

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
	if len(publisher.events) != 0 {
		t.Error("an unknown room must not publish presence")
	}
}
