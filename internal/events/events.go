// Package events carries what happens on a server to the clients watching it.
// Modules publish; the WebSocket gateway subscribes. Nothing here touches HTTP
// or the database, so a module can publish without knowing who listens.
package events

import "github.com/google/uuid"

// Kind names an event. The same strings go over the wire to clients.
type Kind string

const (
	KindMessageCreated  Kind = "message.created"
	KindMessageUpdated  Kind = "message.updated"
	KindMessageDeleted  Kind = "message.deleted"
	KindReactionChanged Kind = "message.reaction"
	KindChannelChanged  Kind = "channel.changed"
	KindChannelDeleted  Kind = "channel.deleted"
	KindMemberChanged   Kind = "member.changed"
	KindVoicePresence   Kind = "voice.presence"
	// KindWorkspaceUpdated says the server itself changed: its name, description,
	// icon or what members may do. The payload is the server in its API shape.
	KindWorkspaceUpdated Kind = "workspace.updated"
	// KindReadChanged is personal: it goes only to the account whose cursor moved.
	KindReadChanged Kind = "read.changed"
	// KindConversationCreated is personal: a dialogue or group was opened, or a
	// secret chat was started. It goes to each participant's own stream.
	KindConversationCreated Kind = "conversation.created"
	// KindConversationUpdated says the name or the people of a group changed.
	KindConversationUpdated Kind = "conversation.updated"
	// KindSecretEnvelope is one ciphertext waiting for a device. It goes to the
	// recipient's personal stream and nowhere else.
	KindSecretEnvelope Kind = "secret.envelope"
)

// Event is one thing that happened on one server.
type Event struct {
	Kind        Kind
	WorkspaceID uuid.UUID
	// ChannelID is set for everything that happens inside a channel.
	ChannelID uuid.UUID
	// Seq orders message events inside a channel; a client that reconnects asks
	// for what it missed by this number. Zero for events without an order.
	Seq int64
	// Payload is what the client receives, already in its API shape.
	Payload any
	// Personal events travel on the account stream rather than a server's: a
	// conversation has no server, and an envelope belongs to one device.
	Personal bool
}

// Publisher is what a module needs to announce an event. Modules declare their
// own narrow interface over this one.
type Publisher interface {
	Publish(event Event)
}
