package servers

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/profile"
)

// countingProfiles answers like the profile module and counts its calls: a list
// must fill every account in one batch, not one call per member.
type countingProfiles struct {
	calls  int
	loaded int
}

func (p *countingProfiles) Profiles(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]profile.Public, error) {
	p.calls++
	p.loaded += len(ids)
	out := make(map[uuid.UUID]profile.Public, len(ids))
	for _, id := range ids {
		out[id] = profile.Public{
			ID:          id,
			Nickname:    "nick-" + id.String()[:8],
			DisplayName: "name-" + id.String()[:8],
			AccentColor: profile.AccentSky,
		}
	}
	return out, nil
}

func TestMemberListFillsProfilesInOneCall(t *testing.T) {
	w := newWorld(t)
	profiles := &countingProfiles{}
	w.svc.SetProfiles(profiles)

	members, err := w.svc.ListMembers(t.Context(), w.workspace, w.member)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if profiles.calls != 1 {
		t.Errorf("profile calls = %d, want 1 for the whole member list", profiles.calls)
	}
	if profiles.loaded != len(members) {
		t.Errorf("profiles loaded = %d, want %d", profiles.loaded, len(members))
	}
	for _, member := range members {
		if member.User.DisplayName != "name-"+member.User.ID.String()[:8] {
			t.Errorf("member %s = %+v, want the filled profile", member.User.ID, member.User)
		}
	}
}

func TestBanListFillsProfilesInOneCall(t *testing.T) {
	w := newWorld(t)
	if _, err := w.svc.Ban(t.Context(), w.workspace, w.owner, w.member2, "flood"); err != nil {
		t.Fatalf("Ban: %v", err)
	}
	profiles := &countingProfiles{}
	w.svc.SetProfiles(profiles)

	bans, err := w.svc.ListBans(t.Context(), w.workspace, w.owner)
	if err != nil {
		t.Fatalf("ListBans: %v", err)
	}
	if profiles.calls != 1 {
		t.Errorf("profile calls = %d, want 1 for the whole ban list", profiles.calls)
	}
	if len(bans) == 0 || bans[0].User.DisplayName == "" {
		t.Errorf("bans = %+v, want the profile filled", bans)
	}
}
