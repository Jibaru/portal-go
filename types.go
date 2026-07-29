package portal

import (
	"encoding/json"
	"time"
)

// RawJSON is an opaque userland JSON payload.
type RawJSON = json.RawMessage

// Sender identifies who sent a Message.
//
// Username is populated only on broadcast channels; on standard channels the
// sender is {ID, Anon} and display data is joined app-side by ID.
type Sender struct {
	ID       string
	Anon     bool
	Username string
}

// Mention is a mention as declared by the sender and verified by the platform
// (members-only, deduped, capped).
type Mention struct {
	UserID string
}

// MessageStatus is the local delivery state of own messages
// (optimistic → ack → rejection). Further states are reserved.
type MessageStatus string

const (
	MessagePending MessageStatus = "pending"
	MessageSent    MessageStatus = "sent"
	MessageFailed  MessageStatus = "failed"
)

// Message is the SDK's public message: platform envelope + userland payload.
// Transport concerns (seq, frame shapes) are stripped at this edge.
type Message struct {
	// ID is platform-assigned; the dedup + mutation key. For an optimistic
	// (still-pending) own message it is a temporary client tag, replaced by the
	// platform ID on ack.
	ID        string
	ChannelID string
	// Type is the userland discriminator; default "message".
	Type string
	// Kind is the envelope content class; "text" in v1.
	Kind string
	// Content is the customer payload (≤2KB), opaque to the platform. Decode it
	// with DecodeContent or json.Unmarshal.
	Content json.RawMessage
	Sender  Sender
	// Timestamp is epoch milliseconds; see Time.
	Timestamp int64
	// To is the targeted-delivery recipient, when this was a targeted send.
	To       string
	Mentions []Mention
	// Retracted flips in place; content stripped per policy.
	Retracted bool
	Ephemeral bool
	// Unread is SDK-derived (not on the wire): whether this message lies beyond
	// the channel watermark.
	Unread bool
	// Status is the local delivery state of own messages.
	Status MessageStatus
}

// Time is the message timestamp as a time.Time.
func (m *Message) Time() time.Time { return time.UnixMilli(m.Timestamp) }

// DecodeContent unmarshals the opaque content payload into v.
func (m *Message) DecodeContent(v any) error { return json.Unmarshal(m.Content, v) }

// SendInput describes one send. A persistent send (Ephemeral false) resolves
// once the edge accepts it; an ephemeral send has no persistence, no ordering,
// no history (cursors, transient signals).
type SendInput struct {
	// Content is the channel's content shape; for chat, e.g. a struct with a
	// text field. Marshalled to JSON.
	Content any
	// Type is the userland discriminator, only for mixed-vocabulary channels;
	// default "message".
	Type string
	// Kind defaults to "text"; media kinds are rejected in v1.
	Kind string
	// To is a delivery instruction: skip fan-out, deliver to this member only,
	// write their inbox item. v1: must be a member. Ignored on ephemeral sends.
	// A field named `to` inside Content routes nothing.
	To string
	// Mentions are declared, not parsed — from the app's autocomplete
	// (presence ∪ members()). Ignored on ephemeral sends.
	Mentions []Mention
	// Ephemeral selects the ephemeral lane: no persistence, no seq, no history.
	Ephemeral bool
}

// SendAck acknowledges an accepted send: accepted and durable (a retraction may
// still follow). Timestamp is epoch milliseconds.
type SendAck struct {
	ID        string
	Timestamp int64
}

// Participant is one member of a detailed presence roster.
type Participant struct {
	ID       string
	Anon     bool
	Username string
	Metadata map[string]any
}

// PresenceKind discriminates the two presence shapes.
type PresenceKind string

const (
	// PresenceDetailed — standard channels: the full participant roster.
	PresenceDetailed PresenceKind = "detailed"
	// PresenceAggregate — broadcast channels: a count plus recent join/leave events.
	PresenceAggregate PresenceKind = "aggregate"
)

// Presence is the channel's current presence: detailed (Participants) on
// standard channels, aggregate (Count, Recent) on broadcast channels.
type Presence struct {
	Kind         PresenceKind
	Participants []Participant // detailed only
	Count        int
	Recent       []json.RawMessage // aggregate only; element shape reserved
}

// ChannelStatus is the connection status of a channel handle.
type ChannelStatus string

const (
	StatusIdle         ChannelStatus = "idle"
	StatusConnecting   ChannelStatus = "connecting"
	StatusReady        ChannelStatus = "ready"
	StatusReconnecting ChannelStatus = "reconnecting"
	// StatusDegraded — an extension namespace is degraded; the channel itself
	// keeps working.
	StatusDegraded ChannelStatus = "degraded"
	// StatusDegradedHTTP — socket down + reconnecting, but HTTP publish still
	// works: you can speak, incoming may lag until reconnect gap-fill heals it.
	StatusDegradedHTTP ChannelStatus = "degraded-http"
	// StatusBlocked — terminal refusal (bad key, banned, not a member, at capacity).
	StatusBlocked ChannelStatus = "blocked"
)

// ActivityEntry is one live transient per-user activity signal (never self).
type ActivityEntry struct {
	UserID string
	Kind   string
	// Since is epoch milliseconds the activity started.
	Since int64
}

// ChannelInfo describes the channel, from the connect snapshot.
type ChannelInfo struct {
	ID   string
	Mode string // "standard" | "broadcast"
	Name string
	Meta json.RawMessage
}

// Me is the connected user's own verified identity, post-connect.
type Me struct {
	ID   string
	Anon bool
	// Claims is whatever the token signer signed. Never assembled client-side.
	Claims map[string]any
}

// MemberRow is one row of the fetched member directory (standard channels;
// includes offline members — not live presence state).
type MemberRow struct {
	UserID string
	Online bool
	Claims map[string]any
}

// ChannelSnapshot is a point-in-time copy of a channel's public state.
type ChannelSnapshot struct {
	Messages []Message
	Presence *Presence
	Activity []ActivityEntry
	Status   ChannelStatus
	Unread   int
	Info     *ChannelInfo
	Me       *Me
	// Ext holds extension snapshots from the connect frame, keyed by handle —
	// the late-joiner's view of state broadcast before this client connected.
	// Nil before ready. A degraded extension is key-absent, so a missing key
	// means "no snapshot", never "empty snapshot". Replaced wholesale on every
	// ready (including reconnects). Live updates arrive via OnMessage, not here.
	Ext               map[string]json.RawMessage
	IsLoadingPrevious bool
	HasPrevious       bool
}
