// Package email sends the few messages Orbit writes itself: a link to reset a
// forgotten password, for now. No template engine and no marketing — one
// envelope, one plain-text body, and an honest error when mail is not set up.
package email

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/ruslancode23/orbit-server/internal/config"
)

// Message is one plain-text letter.
type Message struct {
	To      string
	Subject string
	Text    string
}

// ErrNotConfigured is returned when the instance has no SMTP server. Callers
// turn it into "mail is off" for the client instead of pretending to send.
var ErrNotConfigured = errors.New("email: no SMTP server is configured")

// Sender sends messages. The interface exists so tests and the reset handler
// can work without a mail server.
type Sender interface {
	Send(ctx context.Context, msg Message) error
	Enabled() bool
}

// disabled is the sender of an instance without SMTP.
type disabled struct{ log *slog.Logger }

func (d disabled) Send(context.Context, Message) error { return ErrNotConfigured }
func (d disabled) Enabled() bool                       { return false }

type smtpSender struct {
	cfg config.SMTP
	log *slog.Logger
	// deliver is a field so a test can watch the envelope without a server.
	deliver func(ctx context.Context, from string, msg Message) error
}

func (s *smtpSender) Enabled() bool { return true }

func (s *smtpSender) Send(ctx context.Context, msg Message) error {
	if err := s.deliver(ctx, s.cfg.From, msg); err != nil {
		s.log.Error("email was not sent", "to", msg.To, "subject", msg.Subject, "error", err)
		return err
	}
	s.log.Info("email sent", "to", msg.To, "subject", msg.Subject)
	return nil
}

// New returns a sender for the configuration: a real one when SMTP is set, a
// refusing one otherwise.
func New(cfg config.SMTP, log *slog.Logger) Sender {
	if !cfg.Enabled() {
		return disabled{log: log}
	}
	return &smtpSender{cfg: cfg, log: log, deliver: func(ctx context.Context, from string, msg Message) error {
		return send(ctx, cfg, from, msg)
	}}
}

// send writes the envelope over a live connection: STARTTLS on 587-style ports,
// implicit TLS otherwise.
func send(ctx context.Context, cfg config.SMTP, from string, msg Message) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}

	var conn net.Conn
	var err error
	if cfg.ImplicitTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", cfg.Addr(), &tls.Config{
			ServerName: cfg.Host,
			MinVersion: tls.VersionTLS12,
		})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", cfg.Addr())
	}
	if err != nil {
		return fmt.Errorf("email: dial %s: %w", cfg.Addr(), err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("email: greeting: %w", err)
	}
	defer func() { _ = client.Close() }()

	if !cfg.ImplicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("email: starttls: %w", err)
			}
		}
	}
	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("email: auth: %w", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("email: sender: %w", err)
	}
	if err := client.Rcpt(msg.To); err != nil {
		return fmt.Errorf("email: recipient: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("email: body: %w", err)
	}
	if _, err := writer.Write(payload(from, msg)); err != nil {
		return fmt.Errorf("email: body: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("email: body: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("email: quit: %w", err)
	}
	return nil
}

// payload is the RFC 5322 document: plain text, UTF-8, with the subject encoded
// because it is Russian.
func payload(from string, msg Message) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + msg.To + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", msg.Subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(msg.Text)
	b.WriteString("\r\n")
	return []byte(b.String())
}

// plural picks the Russian form for a count: 1 час, 2 часа, 5 часов.
func plural(n int, one, few, many string) string {
	if n%100 >= 11 && n%100 <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	default:
		return many
	}
}

// PasswordReset is the letter behind a reset link. It says what to do, how long
// the link lives, and what to do if the request was not theirs.
func PasswordReset(to, link string, ttl time.Duration) Message {
	life := "несколько минут"
	hours, minutes := int(ttl.Hours()), int(ttl.Minutes())
	switch {
	case hours >= 2:
		life = fmt.Sprintf("%d %s", hours, plural(hours, "час", "часа", "часов"))
	case hours == 1:
		life = "час"
	case minutes >= 2:
		life = fmt.Sprintf("%d %s", minutes, plural(minutes, "минуту", "минуты", "минут"))
	}
	return Message{
		To:      to,
		Subject: "Восстановление доступа к Orbit",
		Text: fmt.Sprintf(`Здравствуйте!

Кто-то попросил восстановить доступ к аккаунту Orbit для этого адреса.
Чтобы задать новый пароль, откройте ссылку:

%s

Ссылка действует %s и срабатывает один раз.

Если вы не просили восстановление, ничего делать не нужно: пароль остался
прежним, и эта ссылка просто перестанет действовать.
`, link, life),
	}
}
