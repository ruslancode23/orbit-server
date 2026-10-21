package messages

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/events"
)

// A system line is stored as a message with kind = "system": it has a seq and
// an author, appears in history and reaches the participants' personal streams.
func TestRecordSystemStoresAnOrdinaryMessageRow(t *testing.T) {
	w := newWorld(t)
	target := uuid.New()
	err := w.svc.RecordSystem(t.Context(), w.direct, conversations.SystemEvent{
		ActorID:  w.anna,
		Type:     conversations.SystemMemberAdded,
		TargetID: &target,
		Content:  "Анна добавила Боба",
	})
	if err != nil {
		t.Fatalf("RecordSystem: %v", err)
	}

	page, err := w.svc.History(t.Context(), w.direct, w.anna, nil, 50)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(page.Messages) != 1 {
		t.Fatalf("messages = %d, want the system line in history", len(page.Messages))
	}
	line := page.Messages[0]
	if line.Kind != MessageKindSystem {
		t.Errorf("kind = %q, want system", line.Kind)
	}
	if line.Content != "Анна добавила Боба" {
		t.Errorf("content = %q, want the readable fallback", line.Content)
	}
	if line.System == nil || line.System.Type != conversations.SystemMemberAdded {
		t.Fatalf("system = %+v, want member_added", line.System)
	}
	if line.System.TargetID == nil || *line.System.TargetID != target {
		t.Errorf("target = %v, want %v", line.System.TargetID, target)
	}
	if len(line.Mentions) != 0 {
		t.Errorf("mentions = %v, a system line mentions nobody", line.Mentions)
	}
	if line.Seq == 0 {
		t.Error("the system line has no seq")
	}

	// The personal stream gets it as message.created, in the same shape as any
	// message, so a client can render it differently by `kind`.
	var found bool
	for i, event := range w.published.published {
		if event.Kind != events.KindMessageCreated {
			continue
		}
		payload, ok := event.Payload.(api.Message)
		if !ok {
			t.Fatalf("payload = %T, want api.Message", event.Payload)
		}
		if payload.Kind != api.MessageKind("system") {
			t.Errorf("event kind = %q, want system", payload.Kind)
		}
		if payload.System == nil || payload.System.Type != api.SystemMessageType("member_added") {
			t.Errorf("event system = %+v, want member_added", payload.System)
		}
		if w.published.personal[i] != w.anna && w.published.personal[i] != w.bob {
			t.Errorf("published to %v, want a participant", w.published.personal[i])
		}
		found = true
	}
	if !found {
		t.Error("no message.created event was published")
	}
}

// A system line is not the author's text: nobody may edit or delete it.
func TestSystemLineCannotBeEditedOrDeleted(t *testing.T) {
	w := newWorld(t)
	if err := w.svc.RecordSystem(t.Context(), w.direct, conversations.SystemEvent{
		ActorID: w.anna,
		Type:    conversations.SystemMemberRemoved,
		Content: "Анна вышла из группы",
	}); err != nil {
		t.Fatalf("RecordSystem: %v", err)
	}
	page, err := w.svc.History(t.Context(), w.direct, w.anna, nil, 50)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	line := page.Messages[0]

	if _, err := w.svc.Edit(t.Context(), line.ID, w.anna, "edited"); !errors.Is(err, ErrForbidden) {
		t.Errorf("Edit: err = %v, want ErrForbidden", err)
	}
	if _, err := w.svc.Delete(t.Context(), line.ID, w.anna); !errors.Is(err, ErrForbidden) {
		t.Errorf("Delete: err = %v, want ErrForbidden", err)
	}
}

// A direct call with no profiles still produces a valid API message: the kind
// is never empty, which would fail the schema.
func TestAPIMessageDefaultsTheKind(t *testing.T) {
	message := APIMessage(Message{Content: "text"})
	if message.Kind != api.MessageKind("user") {
		t.Errorf("kind = %q, want user", message.Kind)
	}
}

// A conversation has no server, so its participants are the audience of a
// mention: the ones in it count, an outsider is dropped.
func TestMentionsInAConversationFollowItsMembers(t *testing.T) {
	w := newWorld(t)
	sent, _, err := w.svc.Send(t.Context(), w.anna, NewMessage{
		ChannelID:       w.direct,
		ClientMessageID: "mention-1",
		Content:         "hi",
		Mentions:        []uuid.UUID{w.bob, w.outsider},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(sent.Mentions) != 1 || sent.Mentions[0] != w.bob {
		t.Errorf("mentions = %v, want only Bob", sent.Mentions)
	}
}
