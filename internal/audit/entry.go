// Package audit owns the moderation journal: one row per action a moderator
// took on a server — a ban, a kick, a granted role, a deleted channel or
// message, an invite, a handed-over owner. The journal is what an
// organization's security service asks for after an incident, so it is written
// for every one of those actions, whether or not the client shows a button for
// them.
//
// The package is storage only: it has no HTTP and does not know servers,
// channels or messages. The modules that act declare a narrow Auditor
// interface (Record) and main passes this service in, so the dependency points
// from them to this package and never back.
package audit

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Action is what happened. The set is open: a new module records its own
// actions and the client filters by the string, so no enum is fixed in the
// API.
type Action string

// The actions the journal knows today.
const (
	// ActionMemberBan is a member banned and removed; the target is the account.
	ActionMemberBan Action = "member.ban"
	// ActionMemberUnban is a ban lifted; the target is the account.
	ActionMemberUnban Action = "member.unban"
	// ActionMemberKick is a member removed by someone else; the target is the
	// account. Leaving a server by oneself is not a moderation action.
	ActionMemberKick Action = "member.kick"
	// ActionMemberRoleGrant is a role handed to a member; the target is the
	// account and metadata names the role.
	ActionMemberRoleGrant Action = "member.role.grant"
	// ActionMemberRoleRevoke is a role taken away; the target is the account.
	ActionMemberRoleRevoke Action = "member.role.revoke"
	// ActionMemberMute is a timeout put on a member: they keep reading but may
	// not write until it ends; the target is the account and metadata names the
	// duration and the reason the moderator gave.
	ActionMemberMute Action = "member.mute"
	// ActionMemberUnmute is a timeout lifted before its end, either by a
	// moderator or with duration zero; the target is the account.
	ActionMemberUnmute Action = "member.unmute"
	// ActionOwnershipTransfer is the server handed to another owner; the target
	// is the new owner.
	ActionOwnershipTransfer Action = "server.ownership_transfer"
	// ActionRoleCreate, ActionRoleUpdate and ActionRoleDelete change a role;
	// the target is the role.
	ActionRoleCreate Action = "role.create"
	ActionRoleUpdate Action = "role.update"
	ActionRoleDelete Action = "role.delete"
	// ActionChannelCreate, ActionChannelUpdate and ActionChannelDelete change a
	// channel; the target is the channel. An update covers a rename, a topic
	// and the read-only flag.
	ActionChannelCreate Action = "channel.create"
	ActionChannelUpdate Action = "channel.update"
	ActionChannelDelete Action = "channel.delete"
	// ActionMessageDelete is a message removed by a moderator, not by its
	// author; the target is the message.
	ActionMessageDelete Action = "message.delete"
	// ActionInviteCreate and ActionInviteRevoke make and withdraw an invite
	// link; the target is the invite. The code itself is never recorded.
	ActionInviteCreate Action = "invite.create"
	ActionInviteRevoke Action = "invite.revoke"
)

// TargetType is what TargetID names.
type TargetType string

// The kinds of target the journal knows today.
const (
	TargetUser    TargetType = "user"
	TargetRole    TargetType = "role"
	TargetChannel TargetType = "channel"
	TargetMessage TargetType = "message"
	TargetInvite  TargetType = "invite"
)

var (
	// ErrInvalidEntry means the entry names no server, no action or no target
	// type: a row that cannot be read back usefully.
	ErrInvalidEntry = errors.New("audit: an entry needs a server, an action and a target type")
	// ErrInvalidCursor means the page cursor is not one this package issued.
	ErrInvalidCursor = errors.New("audit: invalid cursor")
)

// Entry is one row of the journal. ActorID is the account that took the
// action; the database keeps the row when that account is deleted, with
// ActorID then nil. Metadata carries only identifiers and names, never message
// text, invite codes or tokens.
type Entry struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	ActorID     *uuid.UUID
	Action      Action
	TargetType  TargetType
	TargetID    *uuid.UUID
	Metadata    map[string]any
	CreatedAt   time.Time
}

// Target is a pointer to a copy of id, for the nullable TargetID of Entry.
func Target(id uuid.UUID) *uuid.UUID {
	return &id
}

// Cursor is one position in the journal: the last entry of the page just read.
// Together the pair is unique and keeps paging stable even when two rows share
// a timestamp.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// String encodes the cursor for the API. It is opaque to the client: the
// format may change without breaking a contract, and a client must pass it
// back unchanged.
func (c Cursor) String() string {
	raw := strconv.FormatInt(c.CreatedAt.UnixNano(), 10) + ":" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseCursor reads a cursor the API handed out. Anything else is
// ErrInvalidCursor: the handler answers 400, not an empty page.
func ParseCursor(raw string) (Cursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	nanos, id, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return Cursor{}, ErrInvalidCursor
	}
	value, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{CreatedAt: time.Unix(0, value).UTC(), ID: parsed}, nil
}

// Filter narrows one page. The zero value is «everything, first page».
type Filter struct {
	// Action limits the page to one kind of action.
	Action *Action
	// ActorID limits the page to one moderator.
	ActorID *uuid.UUID
	// Before is the previous page's cursor; nil starts at the newest entry.
	Before *Cursor
}

// ListParams is Filter plus the page size. A zero Limit takes DefaultLimit.
type ListParams struct {
	Filter
	Limit int
}

const (
	// DefaultLimit is the page size when the client asks for none.
	DefaultLimit = 50
	// MaxLimit is the largest page a client may ask for.
	MaxLimit = 100
)

// Page is one page of the journal and the cursor for the next, older one. Next
// is nil on the last page — the only reliable way for a client to stop.
type Page struct {
	Entries []Entry
	Next    *Cursor
}

// String names the action for logs and tests.
func (a Action) String() string { return string(a) }

// Validate reports whether the entry has the three things every row needs.
func (e Entry) Validate() error {
	if e.WorkspaceID == uuid.Nil || e.Action == "" || e.TargetType == "" {
		return ErrInvalidEntry
	}
	return nil
}

// newPageLimit is the page size the service actually uses, before the extra
// row that tells it whether another page exists.
func newPageLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultLimit
	case limit > MaxLimit:
		return MaxLimit
	default:
		return limit
	}
}

// pageOf cuts the extra row the store read to detect a next page and builds the
// cursor from the last entry that stays.
func pageOf(entries []Entry, limit int) Page {
	if len(entries) <= limit {
		return Page{Entries: entries}
	}
	entries = entries[:limit]
	last := entries[len(entries)-1]
	return Page{Entries: entries, Next: &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}}
}

// wrap is a small helper so a store failure names what failed.
func wrap(what string, err error) error {
	return fmt.Errorf("audit: %s: %w", what, err)
}
