// Package wire is the Go port of @portalsdk/wire-protocol: the transport-layer
// vocabulary of the Portal realtime protocol (v1).
//
// It defines every frame the platform and a client exchange over the two socket
// families (channel and inbox), the HTTP request/response bodies of the publish,
// history and members endpoints, upgrade-refusal codes, and total, non-panicking
// parsers with unknown-frame passthrough (§6 forward compatibility).
//
// This is the layer BELOW the SDK's public types: it keeps `seq`, frame shapes and
// reconnect tokens, which the client runtime strips at its public edge.
package wire

// ProtocolVersion is carried by every upgrade as `?v=` (§1.1, §6).
//
// An unknown version is refused at the upgrade with HTTP 426 `unsupported_version`.
// Within v1 the protocol evolves additively only; a breaking change bumps this.
const ProtocolVersion = 1

// Upgrade query-parameter names (§1.1). Build upgrade URLs from these rather than
// string literals, so a rename is a compile error rather than a silent 4xx.
const (
	// ParamVersion is required on every upgrade; unknown → 426.
	ParamVersion = "v"
	// ParamToken carries the signed JWT. Identifies the user.
	ParamToken = "token"
	// ParamKey carries the publishable apiKey, identifying the app (§1).
	ParamKey = "key"
	// ParamLeaf is the opaque reconnect token; echo back what `ready` gave you, unchanged.
	ParamLeaf = "leaf"
	// ParamMeta is initial presence metadata, base64 JSON (standard channels; ≤1KB decoded).
	ParamMeta = "meta"
	// ParamLast is the highest contiguous seq held, sent on reconnect to request replay (§1.4).
	ParamLast = "last"
)

// ErrorHeader is the response header carrying the refusal code on a refused
// upgrade (§1.1). It duplicates `code` from the body so a client behind a
// body-eating proxy can still tell why the socket never opened.
const ErrorHeader = "x-portal-error"

// APIKeyHeader authenticates the app on HTTP requests.
const APIKeyHeader = "x-portal-key"

// RefusalCode says why an upgrade was refused (§1.1).
//
// Refusals happen at the HTTP upgrade — the socket never opens. They are therefore
// disjoint from PublishErrorCode (HTTP publish rejections) and from the in-session
// `error` frame, which both presuppose a working connection.
type RefusalCode string

const (
	RefusalInvalidToken        RefusalCode = "invalid_token"
	RefusalTokenExpired        RefusalCode = "token_expired"
	RefusalInvalidAPIKey       RefusalCode = "invalid_api_key"
	RefusalNotMember           RefusalCode = "not_member"
	RefusalBanned              RefusalCode = "banned"
	RefusalAnonymousNotAllowed RefusalCode = "anonymous_not_allowed"
	RefusalUnknownChannel      RefusalCode = "unknown_channel"
	RefusalUnsupportedVersion  RefusalCode = "unsupported_version"
	RefusalChannelAtCapacity   RefusalCode = "channel_at_capacity"
)

// RefusalStatus maps each refusal to the HTTP status it is delivered with (§1.1).
//
// The mapping is many-to-one: status alone does not identify the cause, so read
// `code` from the body (or the x-portal-error header) rather than branching on status.
var RefusalStatus = map[RefusalCode]int{
	RefusalInvalidToken:        401,
	RefusalTokenExpired:        401,
	RefusalInvalidAPIKey:       403,
	RefusalNotMember:           403,
	RefusalBanned:              403,
	RefusalAnonymousNotAllowed: 403,
	RefusalUnknownChannel:      404,
	RefusalUnsupportedVersion:  426,
	RefusalChannelAtCapacity:   429,
}

// IsRefusalCode reports whether value is a refusal code this version knows (§1.1).
//
// A refusal body arriving with an unrecognised code is not a refusal this client
// can reason about — treat it as an opaque failure rather than coercing it.
func IsRefusalCode(value string) bool {
	_, ok := RefusalStatus[RefusalCode(value)]
	return ok
}

// RefusalBody is the body of a refused upgrade (§1.1).
type RefusalBody struct {
	Code   RefusalCode `json:"code"`
	Reason string      `json:"reason,omitempty"`
	// RetryAfter is seconds to wait before retrying. Documented for
	// `channel_at_capacity` (429) only; absent (zero) on every other refusal.
	RetryAfter int `json:"retryAfter,omitempty"`
}

// PublishErrorCode says why an HTTP publish was rejected (§3.1).
//
// Deliberately NOT merged into RefusalCode: these arrive on a live connection in
// response to `POST /v1/channels/{id}/messages`, and a client reacts to them per
// send, not per connection. `blocked_by_middleware` carries user-visible copy in Reason.
type PublishErrorCode = string

const (
	PublishNotPermitted        PublishErrorCode = "not_permitted"
	PublishBlockedByMiddleware PublishErrorCode = "blocked_by_middleware"
	PublishContentTooLarge     PublishErrorCode = "content_too_large"
	PublishRateLimited         PublishErrorCode = "rate_limited"
)

// PublishErrorBody is the body of a rejected publish (§3.1): `4xx { code, reason? }`.
type PublishErrorBody struct {
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
}

// Mention is a mention as declared by the sender and verified by the platform (§2.1).
type Mention struct {
	UserID string `json:"userId"`
}

// Sender identifies who sent a Message (§2.1).
type Sender struct {
	ID   string `json:"id"`
	Anon bool   `json:"anon"`
	// Username is populated on broadcast channels only — they have no roster to
	// join against. On standard channels display data is joined app-side by ID.
	Username string `json:"username,omitempty"`
}

// Message is the message envelope as it travels on the wire (§2.1).
//
// This is the transport form, one layer BELOW the SDK's public Message. Notably it
// keeps Seq: ordering, dedup and gap-fill are expressed in terms of it. Stripping
// Seq and deriving unread/status is the client runtime's job, not this package's.
type Message struct {
	// ID is platform-assigned: the dedup and mutation key.
	ID string `json:"id"`
	// Seq is per-channel, assigned at persist; contiguous within a connection's
	// delivery stream. Nil for ephemeral messages, which are not persisted and
	// carry no ordering or gap guarantees (§4).
	Seq *int64 `json:"seq"`
	// Type is the userland discriminator; defaults to "message". Opaque to the platform.
	Type string `json:"type"`
	// Kind is the envelope content class; "text" throughout v1. Media kinds are
	// reserved and will not appear in v1 (§7). A string rather than a closed set —
	// a future kind must not cause a v1 parser to drop the frame (§6).
	Kind string `json:"kind"`
	// Content is the customer payload, ≤2KB. Opaque to the platform and to this package.
	Content RawJSON `json:"content"`
	Sender  Sender  `json:"sender"`
	// Timestamp is epoch milliseconds.
	Timestamp int64 `json:"timestamp"`
	// To is the targeted-delivery recipient; the message skips fan-out (§2.1).
	To       string    `json:"to,omitempty"`
	Mentions []Mention `json:"mentions,omitempty"`
	// Retracted flips in place via a `retract` frame; content is stripped per policy.
	Retracted bool `json:"retracted"`
	// Ephemeral messages are not persisted and have Seq == nil.
	Ephemeral bool `json:"ephemeral"`
}

// ChannelMode decides presence shape and whether Sender.Username is populated.
type ChannelMode string

const (
	ModeStandard  ChannelMode = "standard"
	ModeBroadcast ChannelMode = "broadcast"
)

// ChannelInfo describes the channel, from the connect snapshot (§1.2).
type ChannelInfo struct {
	ID   string      `json:"id"`
	Mode ChannelMode `json:"mode"`
	Name string      `json:"name,omitempty"`
	Meta RawJSON     `json:"meta,omitempty"`
}

// Capabilities says what this connection is allowed to do (§1.2).
//
// Open-ended by design: the platform adds capabilities additively (§6) without
// breaking older clients. Named accessors cover the observed keys; absent means
// not granted.
type Capabilities map[string]any

// Publish reports whether this connection may publish persistent messages via
// `POST /v1/channels/{id}/messages` (§3.1).
func (c Capabilities) Publish() bool { v, _ := c["publish"].(bool); return v }

// SendDirect reports whether this connection may send targeted `to:` messages.
func (c Capabilities) SendDirect() bool { v, _ := c["sendDirect"].(bool); return v }

// MeInfo is the connected user's own verified identity (§1.2).
type MeInfo struct {
	ID   string `json:"id"`
	Anon bool   `json:"anon"`
	// Claims is whatever the token signer signed. Never assembled client-side.
	Claims       map[string]any `json:"claims"`
	Capabilities Capabilities   `json:"capabilities"`
}

// PresenceParticipant is a participant as it appears on the wire (§1.2 snapshot,
// §2.1 joined deltas). It carries no claims — the token's claim bag is the
// connected user's own (me.claims in `ready`), never another participant's.
type PresenceParticipant struct {
	ID       string         `json:"id"`
	Anon     bool           `json:"anon"`
	Username string         `json:"username,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// PresenceSnapshot is the `presence` value inside `ready` (§1.2), discriminated
// on Mode: "detailed" (Participants populated) or "aggregate" (Recent populated).
//
// Distinct from PresenceFrame: the snapshot is the full roster, the frame a delta.
type PresenceSnapshot struct {
	Mode         string                `json:"mode"`
	Participants []PresenceParticipant `json:"participants,omitempty"`
	Count        int                   `json:"count"`
	// Recent's element shape is elided in the spec (§2.1) and unproven; it stays
	// raw rather than guessing.
	Recent []RawJSON `json:"recent,omitempty"`
}

// ChannelReadyFrame is the first frame on a channel socket, exactly once (§1.2).
//
// One fat snapshot — there is no staged handshake. Initial history is NOT
// included; the client fetches `GET /history` (§3.2) in parallel with the upgrade.
type ChannelReadyFrame struct {
	T       string      `json:"t"` // "ready"
	Channel ChannelInfo `json:"channel"`
	Me      MeInfo      `json:"me"`
	// Seq is the channel head at snapshot time. The gap-fill baseline (§4).
	Seq int64 `json:"seq"`
	// Leaf is an opaque reconnect token; send it back unchanged as `?leaf=` on
	// the next connect.
	Leaf     string           `json:"leaf"`
	Presence PresenceSnapshot `json:"presence"`
	// Watermark is this user's read position. Nil when watermarks are off (§1.2).
	Watermark *int64 `json:"watermark,omitempty"`
	// Ext holds cached extension snapshots, keyed by namespace. An unavailable
	// extension is key-absent rather than null. Optional on the wire.
	Ext map[string]RawJSON `json:"ext,omitempty"`
	// Bindings maps extension namespace → transport ("ws"/"http"), for routing
	// sends (§1.2). Optional on the wire.
	Bindings map[string]string `json:"bindings,omitempty"`
}

// BatchFrame is THE data frame (§2.1). Messages are coalesced per window; Msgs is
// ordered and seq is contiguous within a connection's delivery stream.
type BatchFrame struct {
	T    string    `json:"t"` // "batch"
	Msgs []Message `json:"msgs"`
}

// RetractFrame says a message was retracted (§2.1).
//
// It may reference a seq the client does not hold yet (the retraction can outrun
// its message): keep a tombstone set and apply on arrival (§4).
type RetractFrame struct {
	T      string `json:"t"` // "retract"
	ID     string `json:"id"`
	Seq    int64  `json:"seq"`
	Reason string `json:"reason,omitempty"`
}

// PresenceFrame is a presence delta (§2.1), discriminated on Mode.
//
// Detailed ("detailed"): Joined carries full participants; Left carries bare
// participant ids. Aggregate ("aggregate"): Count and Recent.
type PresenceFrame struct {
	T      string                `json:"t"` // "presence"
	Mode   string                `json:"mode"`
	Joined []PresenceParticipant `json:"joined,omitempty"`
	Left   []string              `json:"left,omitempty"`
	Count  int                   `json:"count"`
	Recent []RawJSON             `json:"recent,omitempty"`
}

// ActivityFrame is transient per-user activity — typing, thinking, uploading (§2.1).
//
// Never echoed for yourself. Peers expire by absence (~5s client-side); there is
// no explicit "stopped" frame.
type ActivityFrame struct {
	T      string `json:"t"` // "activity" (S→C: has userId+since)
	UserID string `json:"userId"`
	Kind   string `json:"kind"`
	// Since is epoch milliseconds the activity started.
	Since int64 `json:"since"`
}

// DirectFrame is a delivery to THIS connection only — `to:`-sends and targeted
// pushes (§2.1).
type DirectFrame struct {
	T   string  `json:"t"` // "direct"
	Msg Message `json:"msg"`
}

// ReassignFrame is a connection reassignment (§2.1). Close and reconnect, sending
// the new token back as `?leaf=`.
type ReassignFrame struct {
	T    string `json:"t"` // "reassign"
	Leaf string `json:"leaf"`
}

// ErrorFrame is an in-session error (§2.1). Ref echoes the `cl` tag of the client
// frame it answers, so a rejected ephemeral can be matched back to its send.
//
// Code is a string, not a closed set: the spec never enumerates in-session codes.
type ErrorFrame struct {
	T      string `json:"t"` // "error"
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

// PongFrame is the keepalive response (§1.3). Shared with the inbox socket.
type PongFrame struct {
	T string `json:"t"` // "pong"
}

// EphemeralFrame is the ephemeral lane (§2.2): no persistence, no seq, no history.
// Cursors, transient signals, and ws-transport extension traffic all ride this.
type EphemeralFrame struct {
	T string `json:"t"` // "ephemeral"
	// Cl is the client tag. An `error` frame answering this send echoes it as Ref.
	Cl      string  `json:"cl"`
	Type    string  `json:"type"`
	Content RawJSON `json:"content"`
}

// ActivityUpFrame announces own activity (§2.2). Throttled client-side (~3s).
//
// Distinct from the S→C ActivityFrame, which adds UserID and Since: same `t`,
// different shape, different direction.
type ActivityUpFrame struct {
	T    string `json:"t"` // "activity" (C→S: kind only)
	Kind string `json:"kind"`
}

// WatermarkFrame advances this user's read position (§2.2). Independent of inbox
// read state (§5).
type WatermarkFrame struct {
	T   string `json:"t"` // "watermark"
	Seq int64  `json:"seq"`
}

// MetaFrame replaces this session's presence metadata mid-session (§2.2); the
// change is re-announced to other participants via presence deltas.
//
// Metadata is client-supplied and presentation-only — it never feeds
// authorization. Sends the full replacement bag, not a patch.
type MetaFrame struct {
	T        string         `json:"t"` // "meta"
	Metadata map[string]any `json:"metadata"`
}

// PingFrame is the keepalive (§1.3). Shared with the inbox socket.
type PingFrame struct {
	T string `json:"t"` // "ping"
}

// InboxEntry is one conversation row in the inbox (§5).
//
// Muted silences aggregation, not data: a muted entry keeps updating and stops
// contributing to the counter, but items addressed to you still land.
type InboxEntry struct {
	// ID is the channel id this row tracks.
	ID   string  `json:"id"`
	Name string  `json:"name,omitempty"`
	Meta RawJSON `json:"meta,omitempty"`
	// Latest is a preview of the most recent message. Absent on large channels
	// (seq-only tier).
	Latest *InboxLatest `json:"latest,omitempty"`
	Unread int          `json:"unread"`
	Muted  bool         `json:"muted"`
	// At is recency, epoch milliseconds. The sort key.
	At int64 `json:"at"`
}

// InboxLatest previews the most recent message of an inbox entry (§5).
type InboxLatest struct {
	Text   string `json:"text"`
	Sender struct {
		ID string `json:"id"`
	} `json:"sender"`
	At int64 `json:"at"`
}

// InboxItem is a targeted item: a mention, a `to:`-send, or a notify descriptor (§5).
//
// Items carry per-item read state, unlike channels which are positional (watermark).
type InboxItem struct {
	// ID is the event id; the idempotency key.
	ID string `json:"id"`
	// Type is userland: "mention", "ticket.assigned", …
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
	// Data is the userland payload. Opaque to the platform and to this package.
	Data RawJSON `json:"data"`
	// ChannelID is present when the item originated in a channel (mention, `to:`-send).
	ChannelID string `json:"channelId,omitempty"`
	At        int64  `json:"at"`
	Read      bool   `json:"read"`
}

// InboxReadyFrame is the first frame on an inbox socket (§5).
//
// Anonymous tokens never get here — they are refused at the upgrade with 403
// `anonymous_not_allowed`, because no inbox exists for them.
type InboxReadyFrame struct {
	T       string       `json:"t"` // "ready"
	Entries []InboxEntry `json:"entries"`
	Items   []InboxItem  `json:"items"`
	Counter int          `json:"counter"`
}

// InboxEntryFrame is a row upsert — preview, unread, or mute changed (§5).
type InboxEntryFrame struct {
	T     string     `json:"t"` // "entry"
	Entry InboxEntry `json:"entry"`
}

// InboxItemFrame says a targeted item arrived (§5).
type InboxItemFrame struct {
	T    string    `json:"t"` // "item"
	Item InboxItem `json:"item"`
}

// InboxCounterFrame says the global badge changed (§5). Pushed on change.
type InboxCounterFrame struct {
	T string `json:"t"` // "counter"
	N int    `json:"n"`
}

// InboxReadFrame advances the inbox position for one channel — clears its sidebar
// badge (§5). NOT the channel watermark: the inbox tracks noticing, the channel
// tracks reading, and the two may legitimately disagree.
type InboxReadFrame struct {
	T         string `json:"t"` // "read"
	ChannelID string `json:"channelId"`
}

// InboxItemReadFrame flips one item's read flag (§5). Never cascades to older items.
type InboxItemReadFrame struct {
	T  string `json:"t"` // "item.read"
	ID string `json:"id"`
}

// InboxReadAllFrame marks ALL items read (§5). Global and zero-arg — it ignores
// any client-side filter.
type InboxReadAllFrame struct {
	T string `json:"t"` // "read.all"
}

// InboxMuteFrame sets the durable per-user-per-channel mute preference (§5).
type InboxMuteFrame struct {
	T         string `json:"t"` // "mute"
	ChannelID string `json:"channelId"`
	Muted     bool   `json:"muted"`
}

// PublishBody is the body of `POST /v1/channels/{channelId}/messages` (§3.1).
//
// Persistent publishes go over HTTP, never the socket (§2.2).
type PublishBody struct {
	// Type is the userland discriminator; defaults to "message".
	Type string `json:"type,omitempty"`
	// Content is ≤2KB, opaque.
	Content any `json:"content"`
	// Kind defaults to "text". Media kinds are rejected in v1 (§7).
	Kind string `json:"kind,omitempty"`
	// To is a delivery instruction: skip fan-out and deliver to this member only,
	// writing their inbox item. A field named `to` inside Content routes nothing.
	To string `json:"to,omitempty"`
	// Mentions are declared by the sender; the platform verifies, dedupes, caps.
	Mentions []Mention `json:"mentions,omitempty"`
}

// SendAck is a successful publish (§3.1) — the wire form of the SDK's SendAck.
//
// The SDK's public ack is {id, timestamp} with no seq, because seq is transport
// and gets stripped at the SDK edge. Same concept, different layer.
type SendAck struct {
	ID        string `json:"id"`
	Seq       int64  `json:"seq"`
	Timestamp int64  `json:"timestamp"`
}

// HistoryResponse is `GET /v1/channels/{channelId}/history` (§3.2).
//
// One endpoint serves initial backfill, scroll-up paging (?before=&limit=), and
// gap-fill ranges (?from=&to=). Retracted messages come back as tombstoned
// envelopes, consistent with live rendering.
type HistoryResponse struct {
	Msgs    []Message `json:"msgs"`
	HasMore bool      `json:"hasMore"`
}

// MemberRow is one row of the member directory (§3.3).
type MemberRow struct {
	UserID string         `json:"userId"`
	Online bool           `json:"online"`
	Claims map[string]any `json:"claims"`
}

// MembersResponse is `GET /v1/channels/{channelId}/members` (§3.3). Standard
// channels only. A fetched directory including offline members — not live
// presence state. Cursor is empty on the last page.
type MembersResponse struct {
	Members []MemberRow `json:"members"`
	Cursor  string      `json:"cursor,omitempty"`
}
