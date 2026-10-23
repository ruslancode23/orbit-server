package push

import (
	"context"
	"fmt"
	"net/http"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// WebPushSender encrypts the payload for the browser (RFC 8291) and signs the
// request with the VAPID keys (RFC 8292). The push service sees neither the
// payload nor the account.
type WebPushSender struct {
	publicKey  string
	privateKey string
	subject    string
	client     *http.Client
}

// NewWebPushSender takes the VAPID key pair in base64url and the subject the
// push services may contact, a mailto: or https: URL.
func NewWebPushSender(publicKey, privateKey, subject string, client *http.Client) *WebPushSender {
	if client == nil {
		client = &http.Client{Timeout: sendTimeout}
	}
	return &WebPushSender{publicKey: publicKey, privateKey: privateKey, subject: subject, client: client}
}

func (w *WebPushSender) Send(ctx context.Context, s Subscription, payload []byte) (int, error) {
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: s.Endpoint,
		Keys:     webpush.Keys{Auth: s.Auth, P256dh: s.P256dh},
	}, &webpush.Options{
		HTTPClient:      w.client,
		Subscriber:      w.subject,
		VAPIDPublicKey:  w.publicKey,
		VAPIDPrivateKey: w.privateKey,
		TTL:             ttl,
		Urgency:         webpush.UrgencyHigh,
	})
	if err != nil {
		return 0, fmt.Errorf("send web push: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}
