package conversations

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/events"
)

// systemRecorder keeps the feed lines the service asks for, so a test can check
// that a group change is written into the same history as ordinary messages.
type systemRecorder struct {
	channelID uuid.UUID
	events    []SystemEvent
	fail      bool
}

func (r *systemRecorder) RecordSystem(_ context.Context, channelID uuid.UUID, event SystemEvent) error {
	if r.fail {
		return errors.New("recorder failed")
	}
	r.channelID = channelID
	r.events = append(r.events, event)
	return nil
}

// fakeChannelStore is the one channel a group access test needs. The channels
// module asks the conversations module who may read a channel without a server.
type fakeChannelStore struct {
	channel channels.Channel
}

func (s *fakeChannelStore) Create(context.Context, channels.NewChannel) (channels.Channel, error) {
	return channels.Channel{}, errors.New("not implemented")
}

func (s *fakeChannelStore) Channel(context.Context, uuid.UUID) (channels.Channel, error) {
	return s.channel, nil
}

func (s *fakeChannelStore) List(context.Context, uuid.UUID) ([]channels.Channel, error) {
	return []channels.Channel{s.channel}, nil
}

func (s *fakeChannelStore) Update(context.Context, uuid.UUID, channels.Update) (channels.Channel, error) {
	return s.channel, nil
}

func (s *fakeChannelStore) Delete(context.Context, uuid.UUID) error { return nil }

func (s *fakeChannelStore) SetReadOnly(context.Context, channels.Channel, bool) error { return nil }

func (s *fakeChannelStore) Reorder(context.Context, uuid.UUID, []uuid.UUID) error { return nil }

func groupWith(t *testing.T, w world, members ...uuid.UUID) Conversation {
	t.Helper()
	group, _, err := w.svc.Start(t.Context(), members[0], members[1:])
	if err != nil {
		t.Fatalf("Start group: %v", err)
	}
	return group
}

func TestRenameGroupWritesASystemLineAndTellsEveryone(t *testing.T) {
	w := newWorld(t)
	w.svc.SetProfiles(&countingProfiles{})
	group := groupWith(t, w, w.anna, w.bob, w.vika)
	recorder := &eventRecorder{}
	system := &systemRecorder{}
	w.svc.SetPublisher(recorder)
	w.svc.SetSystemMessages(system)

	renamed, err := w.svc.Rename(t.Context(), group.ID, w.bob, "  Design  ")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Name != "Design" {
		t.Errorf("name = %q, want the trimmed Design", renamed.Name)
	}
	if w.store.conversations[group.ID].Name != "Design" {
		t.Error("the name was not stored")
	}
	if len(recorder.events) != 3 {
		t.Fatalf("published %d events, want one per participant", len(recorder.events))
	}
	for i, event := range recorder.events {
		if event.Kind != events.KindConversationUpdated {
			t.Errorf("event %d kind = %v, want %v", i, event.Kind, events.KindConversationUpdated)
		}
		payload, ok := event.Payload.(api.Conversation)
		if !ok {
			t.Fatalf("event %d payload = %T, want api.Conversation", i, event.Payload)
		}
		if payload.Name == nil || *payload.Name != "Design" {
			t.Errorf("event %d payload name = %v, want Design", i, payload.Name)
		}
	}
	if len(system.events) != 1 || system.events[0].Type != SystemConversationRenamed {
		t.Fatalf("system events = %+v, want one rename line", system.events)
	}
	if system.events[0].Name != "Design" || system.events[0].ActorID != w.bob {
		t.Errorf("system event = %+v, want Bob and the new name", system.events[0])
	}
	if system.channelID != group.ID {
		t.Errorf("system line channel = %v, want %v", system.channelID, group.ID)
	}
}

func TestRenameRejectsEmptyNamesAndDirectDialogues(t *testing.T) {
	w := newWorld(t)
	group := groupWith(t, w, w.anna, w.bob, w.vika)
	if _, err := w.svc.Rename(t.Context(), group.ID, w.anna, "   "); !errors.Is(err, ErrInvalidName) {
		t.Errorf("empty name: err = %v, want ErrInvalidName", err)
	}
	if _, err := w.svc.Rename(t.Context(), group.ID, w.anna, strings.Repeat("x", 101)); !errors.Is(err, ErrInvalidName) {
		t.Errorf("long name: err = %v, want ErrInvalidName", err)
	}

	direct, _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob})
	if err != nil {
		t.Fatalf("Start direct: %v", err)
	}
	if _, err := w.svc.Rename(t.Context(), direct.ID, w.anna, "Not a group"); !errors.Is(err, ErrNotGroup) {
		t.Errorf("direct rename: err = %v, want ErrNotGroup", err)
	}
	if _, err := w.svc.Rename(t.Context(), group.ID, w.outsider, "Mine now"); !errors.Is(err, ErrNotFound) {
		t.Errorf("outsider rename: err = %v, want ErrNotFound", err)
	}
}

func TestAddMembersFollowsPrivacyAndNotifiesOldAndNewPeople(t *testing.T) {
	w := newWorld(t)
	w.svc.SetProfiles(&countingProfiles{})
	group := groupWith(t, w, w.anna, w.bob, w.vika)
	recorder := &eventRecorder{}
	system := &systemRecorder{}
	w.svc.SetPublisher(recorder)
	w.svc.SetSystemMessages(system)
	dina := uuid.New()
	w.store.known[dina] = true

	updated, err := w.svc.AddMembers(t.Context(), group.ID, w.anna, []uuid.UUID{dina, dina})
	if err != nil {
		t.Fatalf("AddMembers: %v", err)
	}
	if len(updated.Members) != 4 {
		t.Fatalf("members = %d, want 4", len(updated.Members))
	}
	inside, err := w.svc.IsParticipant(t.Context(), group.ID, dina)
	if err != nil {
		t.Fatalf("IsParticipant: %v", err)
	}
	if !inside {
		t.Error("Dina was not added")
	}
	if len(system.events) != 1 || system.events[0].Type != SystemMemberAdded {
		t.Fatalf("system events = %+v, want one member_added line", system.events)
	}
	if system.events[0].TargetID == nil || *system.events[0].TargetID != dina {
		t.Errorf("system target = %v, want Dina", system.events[0].TargetID)
	}

	// The two who were there get an update; Vika gets a conversation she has just
	// joined. Duplicate ids are collapsed before the store is asked.
	created, updatedCount := 0, 0
	for _, event := range recorder.events {
		switch event.Kind {
		case events.KindConversationCreated:
			created++
		case events.KindConversationUpdated:
			updatedCount++
		default:
			t.Errorf("unexpected event kind %v", event.Kind)
		}
	}
	if created != 1 || updatedCount != 3 {
		t.Errorf("events = %d created, %d updated; want 1 and 3", created, updatedCount)
	}

	// Adding the same person again is not news.
	system.events = nil
	recorder.events = nil
	if _, err := w.svc.AddMembers(t.Context(), group.ID, w.bob, []uuid.UUID{dina}); err != nil {
		t.Fatalf("AddMembers again: %v", err)
	}
	if len(system.events) != 0 || len(recorder.events) != 0 {
		t.Errorf("re-adding published %d system and %d live events, want none", len(system.events), len(recorder.events))
	}
}

func TestAddMembersRefusesStrangersDirectDialoguesAndAFullGroup(t *testing.T) {
	w := newWorld(t)
	group := groupWith(t, w, w.anna, w.bob, w.vika)

	if _, err := w.svc.AddMembers(t.Context(), group.ID, w.outsider, []uuid.UUID{uuid.New()}); !errors.Is(err, ErrNotFound) {
		t.Errorf("outsider: err = %v, want ErrNotFound", err)
	}

	direct, _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob})
	if err != nil {
		t.Fatalf("Start direct: %v", err)
	}
	if _, err := w.svc.AddMembers(t.Context(), direct.ID, w.anna, []uuid.UUID{w.vika}); !errors.Is(err, ErrNotGroup) {
		t.Errorf("direct: err = %v, want ErrNotGroup", err)
	}

	// maxMembers is 4 and the group already has 3: one more fits, another does not.
	first := uuid.New()
	second := uuid.New()
	w.store.known[first] = true
	w.store.known[second] = true
	if _, err := w.svc.AddMembers(t.Context(), group.ID, w.anna, []uuid.UUID{first}); err != nil {
		t.Fatalf("filling the group: %v", err)
	}
	if _, err := w.svc.AddMembers(t.Context(), group.ID, w.anna, []uuid.UUID{second}); !errors.Is(err, ErrTooManyMembers) {
		t.Errorf("over the limit: err = %v, want ErrTooManyMembers", err)
	}

	// A blocked pair is not added, and the answer hides which rule said no.
	w.svc.privacy = (&fakePrivacy{allowGroup: func(uuid.UUID, []uuid.UUID) bool { return false }})
	third := uuid.New()
	w.store.known[third] = true
	if _, err := w.svc.AddMembers(t.Context(), group.ID, w.anna, []uuid.UUID{third}); !errors.Is(err, ErrNotFound) {
		t.Errorf("blocked account: err = %v, want ErrNotFound", err)
	}
}

func TestLeaveGroupEndsAccessAndLeavesALineForTheRest(t *testing.T) {
	w := newWorld(t)
	w.svc.SetProfiles(&countingProfiles{})
	group := groupWith(t, w, w.anna, w.bob, w.vika)
	recorder := &eventRecorder{}
	system := &systemRecorder{}
	w.svc.SetPublisher(recorder)
	w.svc.SetSystemMessages(system)

	// The channel service is the gate messages, files and reads use.
	channelService := channels.NewService(&fakeChannelStore{channel: channels.Channel{
		ID: group.ID, Kind: channels.KindText, Name: "group",
	}}, nil)
	channelService.SetParticipants(w.svc)
	if _, err := channelService.Get(t.Context(), group.ID, w.bob); err != nil {
		t.Fatalf("Bob cannot read the group before leaving: %v", err)
	}

	if err := w.svc.Leave(t.Context(), group.ID, w.bob); err != nil {
		t.Fatalf("Leave: %v", err)
	}

	inside, err := w.svc.IsParticipant(t.Context(), group.ID, w.bob)
	if err != nil {
		t.Fatalf("IsParticipant: %v", err)
	}
	if inside {
		t.Error("Bob is still a participant after leaving")
	}
	if _, err := w.svc.Get(t.Context(), group.ID, w.bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after leaving: err = %v, want ErrNotFound", err)
	}
	members, err := w.store.MemberIDs(t.Context(), group.ID)
	if err != nil {
		t.Fatalf("MemberIDs: %v", err)
	}
	for _, id := range members {
		if id == w.bob {
			t.Error("Bob is still in the member list messages fan out to")
		}
	}
	// The same check guards history, attachments and the live replay: without
	// participation the channel is not found, so nothing is served.
	if _, err := channelService.Get(t.Context(), group.ID, w.bob); !errors.Is(err, channels.ErrNotFound) {
		t.Errorf("channel access after leaving: err = %v, want channels.ErrNotFound", err)
	}

	// Everyone who was in it hears the change, Bob included: his other devices
	// must drop the conversation. The payload no longer lists him.
	if len(recorder.events) != 3 {
		t.Fatalf("published %d update events, want one per previous member", len(recorder.events))
	}
	for _, event := range recorder.events {
		if event.Kind != events.KindConversationUpdated {
			t.Errorf("event kind = %v, want %v", event.Kind, events.KindConversationUpdated)
		}
		payload, ok := event.Payload.(api.Conversation)
		if !ok {
			t.Fatalf("payload = %T, want api.Conversation", event.Payload)
		}
		for _, member := range payload.Members {
			if member.Id == w.bob {
				t.Error("the update still lists Bob")
			}
		}
	}
	if len(system.events) != 1 || system.events[0].Type != SystemMemberRemoved {
		t.Fatalf("system events = %+v, want one member_removed line", system.events)
	}
	if system.events[0].TargetID != nil {
		t.Errorf("leave target = %v, want no target", system.events[0].TargetID)
	}

	// Leaving twice is the same not-found answer as never being there.
	if err := w.svc.Leave(t.Context(), group.ID, w.bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("second leave: err = %v, want ErrNotFound", err)
	}

	// A dialogue cannot be left: the pair would become a one-sided row nobody
	// could reopen. Hiding is the client's tool for that.
	direct, _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob})
	if err != nil {
		t.Fatalf("Start direct: %v", err)
	}
	if err := w.svc.Leave(t.Context(), direct.ID, w.anna); !errors.Is(err, ErrNotGroup) {
		t.Errorf("leaving a dialogue: err = %v, want ErrNotGroup", err)
	}
}

func TestSecretChatRefusesGroupOperationsAndHiding(t *testing.T) {
	w := newWorld(t)
	secret, _, err := w.svc.EnsureSecret(t.Context(), "secret:"+uuid.NewString(), []uuid.UUID{w.anna, w.bob})
	if err != nil {
		t.Fatalf("EnsureSecret: %v", err)
	}
	target := uuid.New()
	w.store.known[target] = true

	if _, err := w.svc.Rename(t.Context(), secret.ID, w.anna, "Secret plans"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename: err = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.AddMembers(t.Context(), secret.ID, w.anna, []uuid.UUID{target}); !errors.Is(err, ErrNotFound) {
		t.Errorf("add members: err = %v, want ErrNotFound", err)
	}
	if err := w.svc.Leave(t.Context(), secret.ID, w.anna); !errors.Is(err, ErrNotFound) {
		t.Errorf("leave: err = %v, want ErrNotFound", err)
	}
	if err := w.svc.SetHidden(t.Context(), secret.ID, w.anna, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("hide: err = %v, want ErrNotFound", err)
	}
}

// A failure to write the system line must not undo the group change itself.
func TestGroupChangeSurvivesAFailedSystemLine(t *testing.T) {
	w := newWorld(t)
	group := groupWith(t, w, w.anna, w.bob, w.vika)
	w.svc.SetSystemMessages(&systemRecorder{fail: true})

	renamed, err := w.svc.Rename(t.Context(), group.ID, w.anna, "Still renamed")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Name != "Still renamed" {
		t.Errorf("name = %q, want the rename kept", renamed.Name)
	}
}

func TestGroupHandlersMapTheRulesToHTTP(t *testing.T) {
	w := newWorld(t)
	h := NewHandler(w.svc, discardLogger())
	anna := sessionContext(t, w.anna)
	bob := sessionContext(t, w.bob)
	group := groupWith(t, w, w.anna, w.bob, w.vika)
	direct, _, err := w.svc.Start(t.Context(), w.anna, []uuid.UUID{w.bob})
	if err != nil {
		t.Fatalf("Start direct: %v", err)
	}

	rename := api.ConversationRenameRequest{Name: "Team"}
	response, err := h.UpdateConversation(anna, api.UpdateConversationRequestObject{
		ConversationId: group.ID, Body: &rename,
	})
	if err != nil {
		t.Fatalf("UpdateConversation: %v", err)
	}
	if _, ok := response.(api.UpdateConversation200JSONResponse); !ok {
		t.Errorf("rename response = %T, want 200", response)
	}
	response, err = h.UpdateConversation(anna, api.UpdateConversationRequestObject{
		ConversationId: direct.ID, Body: &rename,
	})
	if err != nil {
		t.Fatalf("UpdateConversation on a dialogue: %v", err)
	}
	if _, ok := response.(api.UpdateConversation409JSONResponse); !ok {
		t.Errorf("direct rename response = %T, want 409", response)
	}

	add := api.ConversationMembersRequest{UserIds: []uuid.UUID{w.vika}}
	addResponse, err := h.AddConversationMembers(anna, api.AddConversationMembersRequestObject{
		ConversationId: group.ID, Body: &add,
	})
	if err != nil {
		t.Fatalf("AddConversationMembers: %v", err)
	}
	if _, ok := addResponse.(api.AddConversationMembers200JSONResponse); !ok {
		t.Errorf("add response = %T, want 200", addResponse)
	}
	outsiderAdd, err := h.AddConversationMembers(sessionContext(t, w.outsider), api.AddConversationMembersRequestObject{
		ConversationId: group.ID, Body: &add,
	})
	if err != nil {
		t.Fatalf("AddConversationMembers by an outsider: %v", err)
	}
	if _, ok := outsiderAdd.(api.AddConversationMembers404JSONResponse); !ok {
		t.Errorf("outsider add response = %T, want 404", outsiderAdd)
	}

	responseLeave, err := h.LeaveConversation(bob, api.LeaveConversationRequestObject{ConversationId: group.ID})
	if err != nil {
		t.Fatalf("LeaveConversation: %v", err)
	}
	if _, ok := responseLeave.(api.LeaveConversation204Response); !ok {
		t.Errorf("leave response = %T, want 204", responseLeave)
	}

	hide, err := h.HideConversation(anna, api.HideConversationRequestObject{ConversationId: group.ID})
	if err != nil {
		t.Fatalf("HideConversation: %v", err)
	}
	if _, ok := hide.(api.HideConversation204Response); !ok {
		t.Errorf("hide response = %T, want 204", hide)
	}

	unread, err := h.GetConversationUnread(anna, api.GetConversationUnreadRequestObject{})
	if err != nil {
		t.Fatalf("GetConversationUnread: %v", err)
	}
	if _, ok := unread.(api.GetConversationUnread200JSONResponse); !ok {
		t.Errorf("unread response = %T, want 200", unread)
	}
}
