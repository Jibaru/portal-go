package wire

import (
	"encoding/json"
)

// RawJSON is an opaque userland payload, kept verbatim.
//
// Content bags (message content, extension snapshots, item data) are opaque to
// the platform and to this package; RawJSON preserves them byte-for-byte so a
// parse → serialize round-trip drops nothing.
type RawJSON = json.RawMessage

// UnknownFrame is a well-formed frame whose `t` this version does not know (§6).
//
// v1 evolves additively: the platform may introduce new frame types, and an older
// client MUST ignore them. Ignorable is not the same as droppable — the parser
// hands the frame back intact (Raw) so it survives a parse → serialize
// round-trip and can be logged, forwarded, or inspected rather than silently
// vanishing.
type UnknownFrame struct {
	T   string
	Raw RawJSON
}

// Frame is any parsed frame: one of the concrete frame structs in this package,
// or *UnknownFrame for an unrecognised `t`.
//
// Callers type-switch on the concrete type:
//
//	switch f := frame.(type) {
//	case *wire.ChannelReadyFrame: …
//	case *wire.BatchFrame:        …
//	case *wire.UnknownFrame:      // ignore, but don't lose
//	}
type Frame interface{ frameType() string }

func (f *ChannelReadyFrame) frameType() string  { return "ready" }
func (f *BatchFrame) frameType() string         { return "batch" }
func (f *RetractFrame) frameType() string       { return "retract" }
func (f *PresenceFrame) frameType() string      { return "presence" }
func (f *ActivityFrame) frameType() string      { return "activity" }
func (f *DirectFrame) frameType() string        { return "direct" }
func (f *ReassignFrame) frameType() string      { return "reassign" }
func (f *ErrorFrame) frameType() string         { return "error" }
func (f *PongFrame) frameType() string          { return "pong" }
func (f *EphemeralFrame) frameType() string     { return "ephemeral" }
func (f *ActivityUpFrame) frameType() string    { return "activity" }
func (f *WatermarkFrame) frameType() string     { return "watermark" }
func (f *MetaFrame) frameType() string          { return "meta" }
func (f *PingFrame) frameType() string          { return "ping" }
func (f *InboxReadyFrame) frameType() string    { return "ready" }
func (f *InboxEntryFrame) frameType() string    { return "entry" }
func (f *InboxItemFrame) frameType() string     { return "item" }
func (f *InboxCounterFrame) frameType() string  { return "counter" }
func (f *InboxReadFrame) frameType() string     { return "read" }
func (f *InboxItemReadFrame) frameType() string { return "item.read" }
func (f *InboxReadAllFrame) frameType() string  { return "read.all" }
func (f *InboxMuteFrame) frameType() string     { return "mute" }
func (f *UnknownFrame) frameType() string       { return f.T }

// header extracts only the discriminator.
type header struct {
	T *string `json:"t"`
}

// parseInto decodes raw into dst, returning nil (drop the frame) on a shape
// mismatch — the total-parser contract: a known `t` whose shape does not match
// yields nil rather than a panic or a half-filled struct.
func parseInto(raw []byte, dst Frame) Frame {
	if err := json.Unmarshal(raw, dst); err != nil {
		return nil
	}
	return dst
}

// parse is the shared total parser over a `t` → constructor table.
//
//   - malformed JSON, a non-object, or a missing/non-string `t` → nil
//   - a known `t` whose shape does not match → nil
//   - an unknown `t` → the frame is returned intact as *UnknownFrame (§6),
//     because forward compatibility says ignore it, not lose it
//
// Unknown fields on known frames are NOT preserved through the typed structs;
// callers needing byte-perfect passthrough keep the raw text they received.
func parse(raw []byte, known map[string]func() Frame) Frame {
	var h header
	if err := json.Unmarshal(raw, &h); err != nil || h.T == nil {
		return nil
	}
	make, ok := known[*h.T]
	if !ok {
		return &UnknownFrame{T: *h.T, Raw: append(RawJSON(nil), raw...)}
	}
	return parseInto(raw, make())
}

var channelServerFrames = map[string]func() Frame{
	"ready":    func() Frame { return &ChannelReadyFrame{} },
	"batch":    func() Frame { return &BatchFrame{} },
	"retract":  func() Frame { return &RetractFrame{} },
	"presence": func() Frame { return &PresenceFrame{} },
	"activity": func() Frame { return &ActivityFrame{} },
	"direct":   func() Frame { return &DirectFrame{} },
	"reassign": func() Frame { return &ReassignFrame{} },
	"error":    func() Frame { return &ErrorFrame{} },
	"pong":     func() Frame { return &PongFrame{} },
}

var channelClientFrames = map[string]func() Frame{
	"ephemeral": func() Frame { return &EphemeralFrame{} },
	"activity":  func() Frame { return &ActivityUpFrame{} },
	"watermark": func() Frame { return &WatermarkFrame{} },
	"meta":      func() Frame { return &MetaFrame{} },
	"ping":      func() Frame { return &PingFrame{} },
}

var inboxServerFrames = map[string]func() Frame{
	"ready":   func() Frame { return &InboxReadyFrame{} },
	"entry":   func() Frame { return &InboxEntryFrame{} },
	"item":    func() Frame { return &InboxItemFrame{} },
	"counter": func() Frame { return &InboxCounterFrame{} },
	"pong":    func() Frame { return &PongFrame{} },
}

var inboxClientFrames = map[string]func() Frame{
	"read":      func() Frame { return &InboxReadFrame{} },
	"item.read": func() Frame { return &InboxItemReadFrame{} },
	"read.all":  func() Frame { return &InboxReadAllFrame{} },
	"mute":      func() Frame { return &InboxMuteFrame{} },
	"ping":      func() Frame { return &PingFrame{} },
}

// ParseChannelFrame parses a text frame from a channel socket (S→C).
//
// Total and non-panicking: nil on malformed JSON / missing `t` / a known `t` with
// a bad shape, and an *UnknownFrame passthrough for an unrecognised `t` (§6).
func ParseChannelFrame(raw []byte) Frame { return parse(raw, channelServerFrames) }

// ParseInboxFrame parses a text frame from an inbox socket (S→C).
//
// Same contract as ParseChannelFrame. The two families are disjoint: an inbox
// `ready` and a channel `ready` share a `t` but not a shape, so a frame must be
// parsed with the function matching the socket it arrived on.
func ParseInboxFrame(raw []byte) Frame { return parse(raw, inboxServerFrames) }

// ParseChannelClientFrame parses a text frame a client sent on a channel socket
// (C→S) — for a server or test mock receiving the frames a client sends.
func ParseChannelClientFrame(raw []byte) Frame { return parse(raw, channelClientFrames) }

// ParseInboxClientFrame parses a text frame a client sent on an inbox socket (C→S).
func ParseInboxClientFrame(raw []byte) Frame { return parse(raw, inboxClientFrames) }

// SerializeFrame serializes a frame to a JSON text frame.
//
// Primarily for C→S sends. An *UnknownFrame re-serializes to its original raw
// bytes — the round-trip that proves unknown frames survive (§6).
func SerializeFrame(frame Frame) ([]byte, error) {
	if u, ok := frame.(*UnknownFrame); ok {
		return append([]byte(nil), u.Raw...), nil
	}
	setFrameType(frame)
	return json.Marshal(frame)
}

// setFrameType stamps the discriminator so callers can construct frame structs
// without repeating the `t` literal (it is derived from the concrete type).
func setFrameType(frame Frame) {
	switch f := frame.(type) {
	case *ChannelReadyFrame:
		f.T = f.frameType()
	case *BatchFrame:
		f.T = f.frameType()
	case *RetractFrame:
		f.T = f.frameType()
	case *PresenceFrame:
		f.T = f.frameType()
	case *ActivityFrame:
		f.T = f.frameType()
	case *DirectFrame:
		f.T = f.frameType()
	case *ReassignFrame:
		f.T = f.frameType()
	case *ErrorFrame:
		f.T = f.frameType()
	case *PongFrame:
		f.T = f.frameType()
	case *EphemeralFrame:
		f.T = f.frameType()
	case *ActivityUpFrame:
		f.T = f.frameType()
	case *WatermarkFrame:
		f.T = f.frameType()
	case *MetaFrame:
		f.T = f.frameType()
	case *PingFrame:
		f.T = f.frameType()
	case *InboxReadyFrame:
		f.T = f.frameType()
	case *InboxEntryFrame:
		f.T = f.frameType()
	case *InboxItemFrame:
		f.T = f.frameType()
	case *InboxCounterFrame:
		f.T = f.frameType()
	case *InboxReadFrame:
		f.T = f.frameType()
	case *InboxItemReadFrame:
		f.T = f.frameType()
	case *InboxReadAllFrame:
		f.T = f.frameType()
	case *InboxMuteFrame:
		f.T = f.frameType()
	}
}
