package notifysettings

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/events"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

// fakeStore keeps the rows in memory, with the same rules the table enforces.
type fakeStore struct {
	rows []Setting
}

func (s *fakeStore) List(_ context.Context, userID uuid.UUID) ([]Setting, error) {
	var out []Setting
	for _, row := range s.rows {
		if row.UserID == userID {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *fakeStore) Get(_ context.Context, userID uuid.UUID, scope Scope, targetID uuid.UUID) (Setting, bool, error) {
	for _, row := range s.rows {
		if row.UserID == userID && row.Scope == scope && row.TargetID == targetID {
			return row, true, nil
		}
	}
	return Setting{}, false, nil
}

func (s *fakeStore) Upsert(_ context.Context, setting Setting) (Setting, error) {
	for i := range s.rows {
		row := &s.rows[i]
		if row.UserID == setting.UserID && row.Scope == setting.Scope && row.TargetID == setting.TargetID {
			row.Level = setting.Level
			row.MutedUntil = setting.MutedUntil
			row.UpdatedAt = time.Now()
			return *row, nil
		}
	}
	setting.UpdatedAt = time.Now()
	s.rows = append(s.rows, setting)
	return setting, nil
}

func (s *fakeStore) Delete(_ context.Context, userID uuid.UUID, scope Scope, targetID uuid.UUID) (bool, error) {
	for i, row := range s.rows {
		if row.UserID == userID && row.Scope == scope && row.TargetID == targetID {
			s.rows = slices.Delete(s.rows, i, i+1)
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeStore) Count(_ context.Context, userID uuid.UUID) (int, error) {
	var n int
	for _, row := range s.rows {
		if row.UserID == userID {
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) ForTargets(_ context.Context, targetIDs []uuid.UUID) ([]Setting, error) {
	var out []Setting
	for _, row := range s.rows {
		if slices.Contains(targetIDs, row.TargetID) {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *fakeStore) ForUserTargets(_ context.Context, userID uuid.UUID, targetIDs []uuid.UUID) ([]Setting, error) {
	var out []Setting
	for _, row := range s.rows {
		if row.UserID == userID && slices.Contains(targetIDs, row.TargetID) {
			out = append(out, row)
		}
	}
	return out, nil
}

// fakeChannels answers like internal/channels: readers may read, nobody else
// hears of the channel.
type fakeChannels struct {
	channels map[uuid.UUID]channels.Channel
	readers  map[uuid.UUID]map[uuid.UUID]bool
}

func (c *fakeChannels) Get(_ context.Context, channelID, callerID uuid.UUID) (channels.View, error) {
	channel, ok := c.channels[channelID]
	if !ok || !c.readers[channelID][callerID] {
		return channels.View{}, channels.ErrNotFound
	}
	return channels.View{Channel: channel}, nil
}

// fakeWorkspaces answers like internal/servers for one server.
type fakeWorkspaces struct {
	members map[uuid.UUID]map[uuid.UUID]bool
}

func (w *fakeWorkspaces) Authorize(_ context.Context, workspaceID, userID uuid.UUID) (servers.Role, error) {
	if !w.members[workspaceID][userID] {
		return "", servers.ErrNotMember
	}
	return servers.RoleMember, nil
}

// recorder keeps what was published to the account's other devices.
type recorder struct {
	events []events.Event
}

func (r *recorder) PublishToUser(_ uuid.UUID, event events.Event) {
	r.events = append(r.events, event)
}

func levelPtr(level Level) *Level { return &level }

func timePtr(at time.Time) *time.Time { return &at }

func newWorld() (*Service, *fakeStore, *fakeChannels, uuid.UUID, uuid.UUID) {
	workspaceID, channelID, conversationID := uuid.New(), uuid.New(), uuid.New()
	store := &fakeStore{}
	access := &fakeChannels{
		channels: map[uuid.UUID]channels.Channel{
			channelID:      {ID: channelID, WorkspaceID: workspaceID, Kind: channels.KindText, Name: "общий"},
			conversationID: {ID: conversationID, Kind: channels.KindText, Name: "личный"},
			uuid.New():     {ID: uuid.New(), Kind: channels.KindSecret, Name: "секрет"},
		},
		readers: map[uuid.UUID]map[uuid.UUID]bool{},
	}
	workspaces := &fakeWorkspaces{members: map[uuid.UUID]map[uuid.UUID]bool{workspaceID: {}}}
	svc := NewService(store, access, workspaces)
	return svc, store, access, workspaceID, channelID
}

func TestEffectiveLevel(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	userID := uuid.New()
	workspaceID, channelID, conversationID := uuid.New(), uuid.New(), uuid.New()
	serverChannel := channels.Channel{ID: channelID, WorkspaceID: workspaceID, Kind: channels.KindText}
	conversation := channels.Channel{ID: conversationID, Kind: channels.KindText}
	secret := channels.Channel{ID: uuid.New(), Kind: channels.KindSecret}

	row := func(scope Scope, target uuid.UUID, level *Level, mutedUntil *time.Time) Setting {
		return Setting{UserID: userID, Scope: scope, TargetID: target, Level: level, MutedUntil: mutedUntil}
	}

	cases := []struct {
		name    string
		rows    []Setting
		channel channels.Channel
		want    Level
	}{
		{
			name:    "a server channel defaults to mentions",
			channel: serverChannel,
			want:    LevelMentions,
		},
		{
			name:    "a conversation defaults to every message",
			channel: conversation,
			want:    LevelAll,
		},
		{
			name:    "the channel's own level wins",
			rows:    []Setting{row(ScopeChannel, channelID, levelPtr(LevelAll), nil)},
			channel: serverChannel,
			want:    LevelAll,
		},
		{
			name:    "a channel inherits the server's all",
			rows:    []Setting{row(ScopeWorkspace, workspaceID, levelPtr(LevelAll), nil)},
			channel: serverChannel,
			want:    LevelAll,
		},
		{
			name: "the channel outranks the server's level",
			rows: []Setting{
				row(ScopeWorkspace, workspaceID, levelPtr(LevelAll), nil),
				row(ScopeChannel, channelID, levelPtr(LevelMentions), nil),
			},
			channel: serverChannel,
			want:    LevelMentions,
		},
		{
			name: "a channel with no level of its own inherits the server's none",
			rows: []Setting{
				row(ScopeWorkspace, workspaceID, levelPtr(LevelNone), nil),
				row(ScopeChannel, channelID, nil, nil),
			},
			channel: serverChannel,
			want:    LevelNone,
		},
		{
			name:    "a mute deadline silences the channel",
			rows:    []Setting{row(ScopeChannel, channelID, levelPtr(LevelAll), timePtr(now.Add(time.Hour)))},
			channel: serverChannel,
			want:    LevelNone,
		},
		{
			name:    "a passed mute deadline leaves the level alone",
			rows:    []Setting{row(ScopeChannel, channelID, levelPtr(LevelAll), timePtr(now.Add(-time.Minute)))},
			channel: serverChannel,
			want:    LevelAll,
		},
		{
			name:    "muting the server silences its channel",
			rows:    []Setting{row(ScopeWorkspace, workspaceID, levelPtr(LevelAll), timePtr(now.Add(time.Hour)))},
			channel: serverChannel,
			want:    LevelNone,
		},
		{
			name: "the latest deadline wins on the way",
			rows: []Setting{
				row(ScopeWorkspace, workspaceID, levelPtr(LevelAll), timePtr(now.Add(time.Hour))),
				row(ScopeChannel, channelID, levelPtr(LevelAll), timePtr(now.Add(-time.Hour))),
			},
			channel: serverChannel,
			want:    LevelNone,
		},
		{
			name:    "a muted conversation is silent",
			rows:    []Setting{row(ScopeConversation, conversationID, levelPtr(LevelMentions), nil)},
			channel: conversation,
			want:    LevelMentions,
		},
		{
			name:    "a muted conversation is silent until the deadline",
			rows:    []Setting{row(ScopeConversation, conversationID, nil, timePtr(now.Add(time.Hour)))},
			channel: conversation,
			want:    LevelNone,
		},
		{
			name:    "a secret chat never speaks",
			rows:    []Setting{row(ScopeConversation, secret.ID, levelPtr(LevelAll), nil)},
			channel: secret,
			want:    LevelNone,
		},
		{
			name:    "another account's rows do not count",
			rows:    []Setting{{UserID: uuid.New(), Scope: ScopeChannel, TargetID: channelID, Level: levelPtr(LevelAll)}},
			channel: serverChannel,
			want:    LevelMentions,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewService(&fakeStore{rows: tc.rows}, &fakeChannels{}, &fakeWorkspaces{})
			svc.now = func() time.Time { return now }
			got, err := svc.Effective(t.Context(), userID, tc.channel)
			if err != nil {
				t.Fatalf("Effective: %v", err)
			}
			if got != tc.want {
				t.Errorf("Effective = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLevelsBatchesOneAudience(t *testing.T) {
	userID, otherID, silentID := uuid.New(), uuid.New(), uuid.New()
	workspaceID, channelID := uuid.New(), uuid.New()
	row := func(user uuid.UUID, scope Scope, target uuid.UUID, level *Level) Setting {
		return Setting{UserID: user, Scope: scope, TargetID: target, Level: level}
	}
	store := &fakeStore{rows: []Setting{
		row(userID, ScopeChannel, channelID, levelPtr(LevelAll)),
		row(otherID, ScopeWorkspace, workspaceID, levelPtr(LevelAll)),
		row(silentID, ScopeChannel, channelID, levelPtr(LevelNone)),
	}}
	svc := NewService(store, &fakeChannels{}, &fakeWorkspaces{})
	channel := channels.Channel{ID: channelID, WorkspaceID: workspaceID, Kind: channels.KindText}

	levels, err := svc.Levels(t.Context(), channel)
	if err != nil {
		t.Fatalf("Levels: %v", err)
	}
	want := map[uuid.UUID]Level{userID: LevelAll, otherID: LevelAll, silentID: LevelNone}
	if len(levels) != len(want) {
		t.Fatalf("Levels = %v, want %v", levels, want)
	}
	for id, level := range want {
		if levels[id] != level {
			t.Errorf("level of %s = %q, want %q", id, levels[id], level)
		}
	}
}

func TestSetChecksTheTarget(t *testing.T) {
	svc, _, access, workspaceID, channelID := newWorld()
	anna := uuid.New()
	access.readers[channelID] = map[uuid.UUID]bool{anna: true}
	workspaces := svc.workspaces.(*fakeWorkspaces)
	workspaces.members[workspaceID][anna] = true

	conversationID := uuid.New()
	access.channels[conversationID] = channels.Channel{ID: conversationID, Kind: channels.KindText}
	access.readers[conversationID] = map[uuid.UUID]bool{anna: true}
	secretID := uuid.New()
	access.channels[secretID] = channels.Channel{ID: secretID, Kind: channels.KindSecret}
	access.readers[secretID] = map[uuid.UUID]bool{anna: true}

	if _, err := svc.Set(t.Context(), anna, ScopeChannel, channelID, levelPtr(LevelAll), nil); err != nil {
		t.Fatalf("a visible channel: %v", err)
	}
	if _, err := svc.Set(t.Context(), anna, ScopeWorkspace, workspaceID, levelPtr(LevelAll), nil); err != nil {
		t.Fatalf("a server the caller is in: %v", err)
	}
	if _, err := svc.Set(t.Context(), anna, ScopeConversation, conversationID, levelPtr(LevelNone), nil); err != nil {
		t.Fatalf("the caller's conversation: %v", err)
	}

	cases := []struct {
		name   string
		user   uuid.UUID
		scope  Scope
		target uuid.UUID
	}{
		{"a stranger's server", uuid.New(), ScopeWorkspace, workspaceID},
		{"a hidden channel", uuid.New(), ScopeChannel, channelID},
		{"somebody else's conversation", uuid.New(), ScopeConversation, conversationID},
		{"a server channel as a conversation", anna, ScopeConversation, channelID},
		{"a conversation as a channel", anna, ScopeChannel, conversationID},
		{"a secret chat", anna, ScopeConversation, secretID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Set(t.Context(), tc.user, tc.scope, tc.target, levelPtr(LevelAll), nil); !errors.Is(err, ErrNotFound) {
				t.Errorf("Set = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestSetRejectsUnknownValues(t *testing.T) {
	svc, _, _, _, channelID := newWorld()
	anna := uuid.New()
	if _, err := svc.Set(t.Context(), anna, "server", channelID, nil, nil); !errors.Is(err, ErrInvalidScope) {
		t.Errorf("unknown scope = %v, want ErrInvalidScope", err)
	}
	bad := Level("loud")
	if _, err := svc.Set(t.Context(), anna, ScopeChannel, channelID, &bad, nil); !errors.Is(err, ErrInvalidLevel) {
		t.Errorf("unknown level = %v, want ErrInvalidLevel", err)
	}
}

func TestTheLimitStopsNewRowsButNotUpdates(t *testing.T) {
	svc, store, _, workspaceID, channelID := newWorld()
	anna := uuid.New()
	store.rows = append(store.rows, Setting{UserID: anna, Scope: ScopeChannel, TargetID: channelID})
	for i := 1; i < MaxPerUser; i++ {
		store.rows = append(store.rows, Setting{UserID: anna, Scope: ScopeChannel, TargetID: uuid.New()})
	}
	access := svc.channels.(*fakeChannels)
	access.readers[channelID] = map[uuid.UUID]bool{anna: true}
	workspaces := svc.workspaces.(*fakeWorkspaces)
	workspaces.members[workspaceID][anna] = true

	// A new row over the limit is refused...
	if _, err := svc.Set(t.Context(), anna, ScopeWorkspace, workspaceID, levelPtr(LevelAll), nil); !errors.Is(err, ErrLimit) {
		t.Errorf("a new row over the limit = %v, want ErrLimit", err)
	}
	// ...while the one that is already there may still change.
	if _, err := svc.Set(t.Context(), anna, ScopeChannel, channelID, levelPtr(LevelAll), nil); err != nil {
		t.Errorf("updating an existing row = %v, want no error", err)
	}
}

func TestSetAndDeleteTellTheOtherDevices(t *testing.T) {
	svc, _, access, _, channelID := newWorld()
	anna := uuid.New()
	access.readers[channelID] = map[uuid.UUID]bool{anna: true}
	recorder := &recorder{}
	svc.SetPublisher(recorder)

	if _, err := svc.Set(t.Context(), anna, ScopeChannel, channelID, levelPtr(LevelAll), nil); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if len(recorder.events) != 1 || recorder.events[0].Kind != events.KindNotificationSettingsUpdated {
		t.Fatalf("published %+v, want notification_settings.updated", recorder.events)
	}
	if !recorder.events[0].Personal {
		t.Error("the event must go to the account's own stream, not to a server")
	}

	if err := svc.Delete(t.Context(), anna, ScopeChannel, channelID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(recorder.events) != 2 {
		t.Fatalf("published %+v, want a second event after delete", recorder.events)
	}
	if err := svc.Delete(t.Context(), anna, ScopeChannel, channelID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a missing row = %v, want ErrNotFound", err)
	}
}
