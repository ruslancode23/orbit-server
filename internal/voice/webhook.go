package voice

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/protojson"

	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

// The webhook body is the protobuf JSON of one LiveKit event; these are not
// large, and reading a whole one keeps the checksum check simple.
const maxWebhookBytes = 1 << 20

// Webhook receives LiveKit's room events. The signature is checked with the
// same API secret the tokens are signed with; an unsigned request never reaches
// the presence state.
type Webhook struct {
	keys     auth.KeyProvider
	presence *Presence
	log      *slog.Logger
}

func NewWebhook(apiKey, apiSecret string, presence *Presence, log *slog.Logger) *Webhook {
	return &Webhook{
		keys:     staticKeys{key: apiKey, secret: apiSecret},
		presence: presence,
		log:      log,
	}
}

func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	event, err := receiveWebhook(r, h.keys)
	if err != nil {
		h.log.Warn("reject LiveKit webhook", slog.Any("error", err))
		httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid webhook signature")
		return
	}

	room := event.GetRoom()
	if room == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	channelID, err := uuid.Parse(room.GetName())
	if err != nil {
		// A room that is not one of ours (an old dev room, a stray room).
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch event.GetEvent() {
	case "room_finished":
		h.presence.Finish(r.Context(), channelID)
	case "room_started", "participant_joined", "participant_left", "participant_updated",
		"track_published", "track_unpublished", "track_muted", "track_unmuted":
		h.presence.Refresh(r.Context(), channelID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// receiveWebhook reads and verifies a LiveKit webhook: the Authorization token
// is signed with the API secret and carries the SHA-256 of the body, so a
// forged request cannot claim a room event.
func receiveWebhook(r *http.Request, keys auth.KeyProvider) (*livekit.WebhookEvent, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes))
	if err != nil {
		return nil, fmt.Errorf("read webhook body: %w", err)
	}
	token := r.Header.Get("Authorization")
	if token == "" {
		return nil, fmt.Errorf("webhook has no authorization header")
	}
	verifier, err := auth.ParseAPIToken(token)
	if err != nil {
		return nil, fmt.Errorf("parse webhook token: %w", err)
	}
	secret := keys.GetSecret(verifier.APIKey())
	if secret == "" {
		return nil, fmt.Errorf("unknown webhook api key")
	}
	_, claims, err := verifier.Verify(secret)
	if err != nil {
		return nil, fmt.Errorf("verify webhook signature: %w", err)
	}
	hash := sha256.Sum256(body)
	checksum := base64.StdEncoding.EncodeToString(hash[:])
	if subtle.ConstantTimeCompare([]byte(claims.Sha256), []byte(checksum)) != 1 {
		return nil, fmt.Errorf("webhook checksum does not match")
	}

	event := livekit.WebhookEvent{}
	options := protojson.UnmarshalOptions{DiscardUnknown: true, AllowPartial: true}
	if err := options.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("decode webhook: %w", err)
	}
	return &event, nil
}

// staticKeys is the one API key/secret this server knows. LiveKit picks the
// secret by the key id in the token it signed the webhook with.
type staticKeys struct {
	key    string
	secret string
}

func (k staticKeys) GetSecret(key string) string {
	if key == k.key {
		return k.secret
	}
	return ""
}

func (k staticKeys) NumKeys() int { return 1 }
