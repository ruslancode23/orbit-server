package email

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/ruslancode23/orbit-server/internal/config"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestResetLetterIsReadable(t *testing.T) {
	msg := PasswordReset("anna@orbit.example", "https://orbit.example/reset?token=abc", time.Hour)

	if !strings.Contains(msg.Text, "https://orbit.example/reset?token=abc") {
		t.Fatalf("the link is missing from the letter: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "час") {
		t.Fatalf("the lifetime is missing from the letter: %q", msg.Text)
	}
	if msg.Subject == "" || msg.To != "anna@orbit.example" {
		t.Fatalf("unexpected envelope: %+v", msg)
	}
}

func TestPayloadIsAValidMessage(t *testing.T) {
	msg := PasswordReset("anna@orbit.example", "https://orbit.example/reset?token=abc", time.Hour)

	parsed, err := mail.ReadMessage(strings.NewReader(string(payload("orbit@orbit.example", msg))))
	if err != nil {
		t.Fatalf("payload is not a message: %v", err)
	}
	if got := parsed.Header.Get("To"); got != msg.To {
		t.Fatalf("To = %q, want %q", got, msg.To)
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("subject is not decodable: %v", err)
	}
	if decoded != msg.Subject {
		t.Fatalf("Subject = %q, want %q", decoded, msg.Subject)
	}
	if ct := parsed.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; charset=utf-8") {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestWithoutSMTPMailIsRefused(t *testing.T) {
	sender := New(config.SMTP{}, quiet())

	if sender.Enabled() {
		t.Fatal("an empty configuration must not look enabled")
	}
	err := sender.Send(context.Background(), Message{To: "anna@orbit.example"})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Send error = %v, want ErrNotConfigured", err)
	}
}

func TestConfiguredSenderHandsTheEnvelopeOver(t *testing.T) {
	var got Message
	var from string
	sender := &smtpSender{
		cfg: config.SMTP{Host: "mail.orbit.example", Port: 587, From: "orbit@orbit.example"},
		log: quiet(),
		deliver: func(_ context.Context, senderFrom string, msg Message) error {
			from, got = senderFrom, msg
			return nil
		},
	}

	msg := PasswordReset("anna@orbit.example", "https://orbit.example/reset?token=abc", time.Hour)
	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if from != "orbit@orbit.example" || got.To != msg.To {
		t.Fatalf("envelope = %q → %q, want the configured sender and the recipient", from, got.To)
	}
}

func TestConfiguredSenderReportsAFailure(t *testing.T) {
	failure := errors.New("connection refused")
	sender := &smtpSender{
		cfg: config.SMTP{Host: "mail.orbit.example", Port: 587, From: "orbit@orbit.example"},
		log: quiet(),
		deliver: func(context.Context, string, Message) error {
			return failure
		},
	}

	err := sender.Send(context.Background(), Message{To: "anna@orbit.example"})
	if !errors.Is(err, failure) {
		t.Fatalf("Send error = %v, want the delivery failure", err)
	}
}
