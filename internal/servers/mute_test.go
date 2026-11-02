package servers

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/audit"
	"github.com/ruslancode23/orbit-server/internal/events"
)

// The owner silences a member: the timeout is stored, the send rules report it,
// the journal gets the reason, and every member hears it.
func TestOwnerMutesAMember(t *testing.T) {
	w, _, watchers := newProfileWorld(t)
	journal := &captureAuditor{}
	w.svc.SetAuditor(journal)

	member, err := w.svc.Mute(t.Context(), w.workspace, w.owner, w.member, 10*time.Minute, "  флуд  ")
	if err != nil {
		t.Fatalf("Mute: %v", err)
	}
	if member.MutedUntil == nil {
		t.Fatal("the muted member carries no mutedUntil")
	}
	if left := time.Until(*member.MutedUntil); left < 9*time.Minute || left > 11*time.Minute {
		t.Errorf("the mute lasts %v, want about ten minutes", left)
	}

	rules, err := w.svc.SendRules(t.Context(), w.workspace, w.member)
	if err != nil {
		t.Fatalf("SendRules: %v", err)
	}
	if !rules.Muted(time.Now()) || rules.MaxMentions != 5 || rules.MaxLinks != 5 {
		t.Errorf("rules = %+v, want a running mute and the default caps", rules)
	}

	if len(journal.entries) != 1 {
		t.Fatalf("journal = %+v, want one entry", journal.entries)
	}
	entry := journal.entries[0]
	if entry.Action != audit.ActionMemberMute || entry.TargetType != audit.TargetUser {
		t.Errorf("entry = %+v, want member.mute of a user", entry)
	}
	if entry.TargetID == nil || *entry.TargetID != w.member {
		t.Errorf("target = %v, want %v", entry.TargetID, w.member)
	}
	if entry.Metadata["durationSeconds"] != 600 || entry.Metadata["reason"] != "флуд" {
		t.Errorf("metadata = %+v, want the duration and the trimmed reason", entry.Metadata)
	}

	// Every member, the muted one included, hears about the new membership
	// state; the reason is not in the event.
	for id, heard := range watchers.personal {
		var changed *events.Event
		for i := range heard {
			if heard[i].Kind == events.KindMemberChanged {
				changed = &heard[i]
			}
		}
		if changed == nil {
			t.Errorf("%s heard no member.changed", id)
			continue
		}
		payload, ok := changed.Payload.(map[string]any)
		if !ok {
			t.Fatalf("payload = %T, want a map", changed.Payload)
		}
		if len(payload) != 4 {
			t.Errorf("payload = %+v, want exactly four fields", payload)
		}
		mutedUntil, ok := payload["mutedUntil"].(*string)
		if !ok || mutedUntil == nil {
			t.Errorf("payload mutedUntil = %v, want the moment", payload["mutedUntil"])
			continue
		}
		if *mutedUntil != member.MutedUntil.UTC().Format(time.RFC3339) {
			t.Errorf("payload mutedUntil = %s, want %v", *mutedUntil, member.MutedUntil)
		}
		for _, value := range payload {
			if text, ok := value.(string); ok && text == "флуд" {
				t.Error("the mute reason travelled in an event")
			}
		}
	}
}

// Duration zero lifts a mute and leaves nothing behind.
func TestUnmuteLiftsTheTimeout(t *testing.T) {
	w, _, _ := newProfileWorld(t)
	journal := &captureAuditor{}
	w.svc.SetAuditor(journal)

	if _, err := w.svc.Mute(t.Context(), w.workspace, w.owner, w.member, time.Hour, "спам"); err != nil {
		t.Fatalf("Mute: %v", err)
	}
	member, err := w.svc.Mute(t.Context(), w.workspace, w.owner, w.member, 0, "")
	if err != nil {
		t.Fatalf("unmute: %v", err)
	}
	if member.MutedUntil != nil {
		t.Errorf("mutedUntil = %v, want none", member.MutedUntil)
	}
	rules, err := w.svc.SendRules(t.Context(), w.workspace, w.member)
	if err != nil {
		t.Fatalf("SendRules: %v", err)
	}
	if rules.Muted(time.Now()) {
		t.Error("the member is still muted after duration zero")
	}
	if len(journal.entries) != 2 || journal.entries[1].Action != audit.ActionMemberUnmute {
		t.Errorf("journal = %+v, want member.mute then member.unmute", journal.entries)
	}
}

// Neither the owner nor anyone at or above the caller may be silenced, and
// nobody may silence themselves: a mute one could lift at will is not a mute.
func TestMuteFollowsTheRoleHierarchy(t *testing.T) {
	w, _, _ := newProfileWorld(t)
	secondAdmin := uuid.New()
	if _, err := w.store.AddMember(t.Context(), w.workspace, secondAdmin, RoleAdmin); err != nil {
		t.Fatalf("add second admin: %v", err)
	}

	for _, tc := range []struct {
		name   string
		caller uuid.UUID
		target uuid.UUID
	}{
		{"the owner cannot be muted", w.admin, w.owner},
		{"an equal role cannot be muted", w.admin, secondAdmin},
		{"a member cannot mute", w.member, w.member2},
		{"self-mute is refused", w.admin, w.admin},
		{"self-unmute is refused", w.admin, w.admin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.svc.Mute(t.Context(), w.workspace, tc.caller, tc.target, time.Minute, ""); !errors.Is(err, ErrForbidden) {
				t.Errorf("Mute = %v, want ErrForbidden", err)
			}
		})
	}

	// A moderator above the target may: an admin outranks a plain member.
	if _, err := w.svc.Mute(t.Context(), w.workspace, w.admin, w.member, time.Minute, ""); err != nil {
		t.Errorf("admin muting a member: %v", err)
	}
}

// A stranger is not a member of anything here: the same answer as a missing
// server, as everywhere else.
func TestMuteOfAnOutsiderIsNotFound(t *testing.T) {
	w, _, _ := newProfileWorld(t)

	if _, err := w.svc.Mute(t.Context(), w.workspace, w.owner, w.outsider, time.Minute, ""); !errors.Is(err, ErrNotMember) {
		t.Errorf("Mute of an outsider = %v, want ErrNotMember", err)
	}
}

func TestMuteRejectsOutOfRangeInput(t *testing.T) {
	w, _, _ := newProfileWorld(t)

	if _, err := w.svc.Mute(t.Context(), w.workspace, w.owner, w.member, maxMuteDuration+time.Second, ""); !errors.Is(err, ErrInvalidMuteDuration) {
		t.Errorf("too long a mute = %v, want ErrInvalidMuteDuration", err)
	}
	long := make([]rune, maxMuteReason+1)
	for i := range long {
		long[i] = 'я'
	}
	if _, err := w.svc.Mute(t.Context(), w.workspace, w.owner, w.member, time.Minute, string(long)); !errors.Is(err, ErrInvalidMuteReason) {
		t.Errorf("too long a reason = %v, want ErrInvalidMuteReason", err)
	}
}

// A server that automutes its newcomers silences them on joining: a message
// cannot slip in between the join and the timeout.
func TestAutomuteSilencesANewMember(t *testing.T) {
	w, _, watchers := newProfileWorld(t)
	seconds := 3600
	if _, err := w.svc.Update(t.Context(), w.workspace, w.owner, WorkspaceUpdate{AutomuteNewMembersSeconds: &seconds}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	newcomer := uuid.New()
	member, err := w.svc.Join(t.Context(), w.workspace, newcomer)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if member.MutedUntil == nil {
		t.Fatal("the newcomer has no automute")
	}
	if left := time.Until(*member.MutedUntil); left < 59*time.Minute {
		t.Errorf("the automute lasts %v, want about an hour", left)
	}
	rules, err := w.svc.SendRules(t.Context(), w.workspace, newcomer)
	if err != nil {
		t.Fatalf("SendRules: %v", err)
	}
	if !rules.Muted(time.Now()) {
		t.Error("the newcomer may write right after joining")
	}
	// The join event carries the timeout, so the newcomer's client shows it.
	heard := watchers.personal[newcomer]
	if len(heard) != 1 || heard[0].Kind != events.KindMemberChanged {
		t.Fatalf("newcomer heard %+v, want one member.changed", heard)
	}
	payload, ok := heard[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload = %T, want a map", heard[0].Payload)
	}
	if payload["mutedUntil"] == nil {
		t.Error("the join event carries no mutedUntil")
	}
}

// Without the setting a new member may write at once.
func TestAutomuteIsOffByDefault(t *testing.T) {
	w, _, _ := newProfileWorld(t)

	member, err := w.svc.Join(t.Context(), w.workspace, uuid.New())
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if member.MutedUntil != nil {
		t.Errorf("mutedUntil = %v, want none", member.MutedUntil)
	}
}

// The settings are validated by the update that carries them.
func TestUpdateRejectsOutOfRangeSettings(t *testing.T) {
	w, _, _ := newProfileWorld(t)
	tooSlow, tooManyMentions, tooManyLinks, negative := 604801, 101, 101, -1

	for _, tc := range []struct {
		name   string
		update WorkspaceUpdate
		want   error
	}{
		{"automute too long", WorkspaceUpdate{AutomuteNewMembersSeconds: &tooSlow}, ErrInvalidAutomute},
		{"automute negative", WorkspaceUpdate{AutomuteNewMembersSeconds: &negative}, ErrInvalidAutomute},
		{"mentions too many", WorkspaceUpdate{MaxMentionsPerMessage: &tooManyMentions}, ErrInvalidMentionLimit},
		{"links too many", WorkspaceUpdate{MaxLinksPerMessage: &tooManyLinks}, ErrInvalidLinkLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.svc.Update(t.Context(), w.workspace, w.owner, tc.update); !errors.Is(err, tc.want) {
				t.Errorf("Update = %v, want %v", err, tc.want)
			}
		})
	}
}

// The caps and the automute are part of the server in its API shape, since the
// workspace.updated event carries that shape.
func TestSettingsTravelInTheWorkspace(t *testing.T) {
	w, _, _ := newProfileWorld(t)
	automute, mentions, links := 300, 0, 7

	updated, err := w.svc.Update(t.Context(), w.workspace, w.owner, WorkspaceUpdate{
		AutomuteNewMembersSeconds: &automute,
		MaxMentionsPerMessage:     &mentions,
		MaxLinksPerMessage:        &links,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Workspace.AutomuteNewMembersSeconds != automute ||
		updated.Workspace.MaxMentionsPerMessage != mentions ||
		updated.Workspace.MaxLinksPerMessage != links {
		t.Errorf("workspace = %+v, want the new settings", updated.Workspace)
	}
}
