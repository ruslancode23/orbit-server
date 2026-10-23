package messages

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/push"
)

// notifications records what the service asked push to send.
type notifications struct {
	sent []sentNotice
}

type sentNotice struct {
	recipients []uuid.UUID
	n          push.Notification
}

func (r *notifications) Notify(_ context.Context, recipients []uuid.UUID, n push.Notification) {
	r.sent = append(r.sent, sentNotice{recipients: slices.Clone(recipients), n: n})
}

func withNotifications(w world) *notifications {
	r := &notifications{}
	w.svc.SetNotifier(r)
	return r
}

func TestDirectMessageNotifiesTheOtherSideWithoutTheText(t *testing.T) {
	w := newWorld(t)
	got := withNotifications(w)

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.direct, ClientMessageID: "dm-1", Content: "пароль от вайфая 12345",
	}); err != nil {
		t.Fatal(err)
	}

	if len(got.sent) != 1 {
		t.Fatalf("notified %d times, want once", len(got.sent))
	}
	notice := got.sent[0]
	if !slices.Equal(notice.recipients, []uuid.UUID{w.bob}) {
		t.Errorf("recipients = %v, want Bob only (not the author)", notice.recipients)
	}
	if notice.n.URL != "/me/"+w.direct.String() || notice.n.Tag != w.direct.String() {
		t.Errorf("notification = %+v, want a link to the dialogue tagged by it", notice.n)
	}
	if strings.Contains(notice.n.Title+notice.n.Body, "12345") {
		t.Errorf("the text leaked into the notification: %+v", notice.n)
	}
}

func TestGroupSkipsWhoBlockedTheAuthor(t *testing.T) {
	w := newWorld(t)
	group := w.addConversation(t, w.anna, w.bob, w.admin)
	w.svc.SetPrivacy(&fakePrivacy{blockedBy: map[uuid.UUID]map[uuid.UUID]bool{w.admin: {w.anna: true}}})
	got := withNotifications(w)

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{ChannelID: group, ClientMessageID: "g-1", Content: "всем привет"}); err != nil {
		t.Fatal(err)
	}

	if len(got.sent) != 1 || !slices.Equal(got.sent[0].recipients, []uuid.UUID{w.bob}) {
		t.Fatalf("sent = %+v, want Bob only: the admin blocked Anna", got.sent)
	}
	if got.sent[0].n.Body != "Новое сообщение в групповом чате" {
		t.Errorf("body = %q, want the group wording", got.sent[0].n.Body)
	}
}

func TestServerChannelNotifiesOnlyMentionedReaders(t *testing.T) {
	w := newWorld(t)
	// The admin is on the server but cannot open this channel.
	delete(w.access.readers[w.channel], w.admin)
	got := withNotifications(w)

	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID: w.channel, ClientMessageID: "m-1", Content: "@bob @admin @anna глянь",
		Mentions: []uuid.UUID{w.bob, w.admin, w.anna},
	}); err != nil {
		t.Fatal(err)
	}

	if len(got.sent) != 1 || !slices.Equal(got.sent[0].recipients, []uuid.UUID{w.bob}) {
		t.Fatalf("sent = %+v, want Bob only: not the author, not someone who cannot see the channel", got.sent)
	}
	n := got.sent[0].n
	if n.Title != "Упоминание в #общий" || n.URL != "/s/"+w.workspace.String()+"/"+w.channel.String() {
		t.Errorf("notification = %+v", n)
	}
}

func TestPlainChannelMessageAndResendNotifyNobody(t *testing.T) {
	w := newWorld(t)
	got := withNotifications(w)

	send(t, w, w.anna, "c-1", "без упоминаний")
	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{ChannelID: w.direct, ClientMessageID: "dm-1", Content: "раз"}); err != nil {
		t.Fatal(err)
	}
	// A resend after a network failure is the same message, not news.
	if _, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{ChannelID: w.direct, ClientMessageID: "dm-1", Content: "раз"}); err != nil {
		t.Fatal(err)
	}

	var withRecipients int
	for _, s := range got.sent {
		if len(s.recipients) > 0 {
			withRecipients++
		}
	}
	if withRecipients != 1 {
		t.Fatalf("sent = %+v, want one notification: the first direct message", got.sent)
	}
}
