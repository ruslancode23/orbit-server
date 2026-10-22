package perms

import (
	"slices"
	"testing"

	"github.com/google/uuid"
)

var (
	everyoneID  = uuid.New()
	adminID     = uuid.New()
	moderatorID = uuid.New()
	djID        = uuid.New()
)

// A server like most: @everyone talks, a DJ speaks in voice, a moderator
// cleans up, an administrator does everything.
func server() Server {
	return Server{Roles: []Role{
		{ID: everyoneID, Default: true, Permissions: ViewChannel | SendMessages | Speak | AttachFiles},
		{ID: djID, Position: 1, Permissions: Speak},
		{ID: moderatorID, Position: 2, Permissions: ManageMessages | KickMembers},
		{ID: adminID, Position: 3, Permissions: Administrator},
	}}
}

func TestBase(t *testing.T) {
	s := server()
	tests := []struct {
		name   string
		member Member
		want   Set
	}{
		{"plain member has @everyone", Member{}, ViewChannel | SendMessages | Speak | AttachFiles},
		{
			"roles add up",
			Member{Roles: []uuid.UUID{moderatorID, djID}},
			ViewChannel | SendMessages | Speak | AttachFiles | ManageMessages | KickMembers,
		},
		{"administrator is everything", Member{Roles: []uuid.UUID{adminID}}, All},
		{"owner is everything", Member{Owner: true}, All},
		{
			"an unknown role adds nothing",
			Member{Roles: []uuid.UUID{uuid.New()}},
			ViewChannel | SendMessages | Speak | AttachFiles,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.Base(tt.member); got != tt.want {
				t.Errorf("Base = %v, want %v", got.Names(), tt.want.Names())
			}
		})
	}
}

func TestInChannel(t *testing.T) {
	s := server()
	member := Member{}
	moderator := Member{Roles: []uuid.UUID{moderatorID}}
	dj := Member{Roles: []uuid.UUID{djID}}
	admin := Member{Roles: []uuid.UUID{adminID}}

	// A staff room: hidden from @everyone, open to moderators.
	staff := []Override{
		{RoleID: everyoneID, Deny: ViewChannel},
		{RoleID: moderatorID, Allow: ViewChannel},
	}
	// An announcements channel: everyone reads, only moderators write.
	announcements := []Override{
		{RoleID: everyoneID, Deny: SendMessages},
		{RoleID: moderatorID, Allow: SendMessages},
	}
	// A stage: only the DJ speaks.
	stage := []Override{{RoleID: everyoneID, Deny: Speak}, {RoleID: djID, Allow: Speak}}

	tests := []struct {
		name      string
		member    Member
		overrides []Override
		want      Set
		lacks     Set
	}{
		{"no overrides keeps the base", member, nil, ViewChannel | SendMessages, 0},
		{"@everyone deny hides the channel", member, staff, 0, ViewChannel},
		{"a role allow beats the @everyone deny", moderator, staff, ViewChannel, 0},
		{"read-only for members", member, announcements, ViewChannel, SendMessages},
		{"moderators write in read-only", moderator, announcements, SendMessages, 0},
		{"only the DJ speaks", member, stage, 0, Speak},
		{"the DJ speaks", dj, stage, Speak, 0},
		{"administrator ignores overrides", admin, staff, All, 0},
		{"owner ignores overrides", Member{Owner: true}, staff, All, 0},
		// A role deny and another role's allow on the same bit: allow wins, as in Discord.
		{
			"role allow beats role deny",
			Member{Roles: []uuid.UUID{djID, moderatorID}},
			[]Override{{RoleID: djID, Deny: SendMessages}, {RoleID: moderatorID, Allow: SendMessages}},
			SendMessages, 0,
		},
		// Server-wide permissions cannot be granted by a channel.
		{
			"a channel cannot grant a server permission", member,
			[]Override{{RoleID: everyoneID, Allow: BanMembers | ManageRoles}},
			0, BanMembers | ManageRoles,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.InChannel(tt.member, tt.overrides)
			if !got.Has(tt.want) {
				t.Errorf("InChannel = %v, want it to include %v", got.Names(), tt.want.Names())
			}
			if tt.lacks != 0 && got&tt.lacks != 0 {
				t.Errorf("InChannel = %v, want it to lack %v", got.Names(), tt.lacks.Names())
			}
		})
	}
}

func TestHierarchy(t *testing.T) {
	s := server()
	owner := Member{Owner: true}
	admin := Member{Roles: []uuid.UUID{adminID}}
	moderator := Member{Roles: []uuid.UUID{moderatorID}}
	member := Member{}
	otherModerator := Member{Roles: []uuid.UUID{moderatorID, djID}}

	tests := []struct {
		name          string
		actor, target Member
		want          bool
	}{
		{"owner over admin", owner, admin, true},
		{"admin over moderator", admin, moderator, true},
		{"moderator over member", moderator, member, true},
		{"equal ranks cannot act on each other", moderator, otherModerator, false},
		{"lower cannot act on higher", moderator, admin, false},
		{"nobody acts on the owner", admin, owner, false},
		{"member over nobody", member, member, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.Outranks(tt.actor, tt.target); got != tt.want {
				t.Errorf("Outranks = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMayManageRole(t *testing.T) {
	s := server()
	s.Roles = append(s.Roles, Role{ID: uuid.New(), Position: 4, Permissions: ManageRoles})
	roleManager := Member{Roles: []uuid.UUID{s.Roles[len(s.Roles)-1].ID}}
	moderator := Member{Roles: []uuid.UUID{moderatorID}}
	everyone, _ := s.role(everyoneID)
	moderatorRole, _ := s.role(moderatorID)
	adminRole, _ := s.role(adminID)

	if !s.MayManageRole(roleManager, moderatorRole) {
		t.Error("a role manager could not manage a role below")
	}
	if !s.MayManageRole(roleManager, adminRole) {
		t.Error("a role manager at 4 could not manage the administrator at 3")
	}
	if s.MayManageRole(moderator, everyone) {
		t.Error("a moderator without manageRoles managed @everyone")
	}
	if !s.MayManageRole(roleManager, everyone) {
		t.Error("a role manager could not edit @everyone")
	}
	own := s.Roles[len(s.Roles)-1]
	if s.MayManageRole(roleManager, own) {
		t.Error("a role manager managed their own top role")
	}
	if s.MayGrant(roleManager, BanMembers) {
		t.Error("a role manager granted a permission they do not have")
	}
	if !s.MayGrant(Member{Owner: true}, All) {
		t.Error("the owner could not grant everything")
	}
}

func TestNames(t *testing.T) {
	set := Administrator | SendMessages | Speak
	parsed, ok := Parse(set.Names())
	if !ok || parsed != set {
		t.Errorf("round trip = %v, %v; want %v", parsed.Names(), ok, set.Names())
	}
	if _, ok := Parse([]string{"sendMessages", "flyToTheMoon"}); ok {
		t.Error("an unknown name was accepted")
	}
	if !slices.Equal(All.Names()[:2], []string{"administrator", "manageServer"}) {
		t.Errorf("names are not in bit order: %v", All.Names())
	}
	if !All.Valid() || (All + 1).Valid() {
		t.Error("Valid does not tell known bits from unknown ones")
	}
}
