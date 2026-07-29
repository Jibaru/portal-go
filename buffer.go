package portal

import (
	"encoding/json"
	"sort"

	"github.com/Jibaru/portal-go/wire"
)

// optimisticSend is an own message inserted before the edge acked it.
type optimisticSend struct {
	tempID    string
	msgType   string
	content   json.RawMessage
	to        string
	mentions  []Mention
	timestamp int64
	status    MessageStatus
}

// messageBuffer holds the seq-ordered persistent window plus optimistic sends —
// a direct port of @portalsdk/core's MessageBuffer.
//
// Not goroutine-safe: the owning connection serialises access under its lock.
type messageBuffer struct {
	channelID  string
	persistent map[int64]wire.Message
	// pendingRetracts holds seqs whose retraction outran the message; applied
	// when the message arrives.
	pendingRetracts map[int64]struct{}
	optimistic      []optimisticSend
	me              *Sender
	// contiguous is the highest seq held with no gap below it — the `last=`
	// value and gap-fill anchor. Seeded from the ready head; the live stream
	// begins at contiguous+1. Nil until seeded.
	contiguous *int64
	// head is the latest seq known for the channel, independent of what is loaded.
	head *int64
	// watermark is my read position; unread counts what lies beyond it.
	watermark   *int64
	hasPrevious bool
}

func newMessageBuffer(channelID string) *messageBuffer {
	return &messageBuffer{
		channelID:       channelID,
		persistent:      map[int64]wire.Message{},
		pendingRetracts: map[int64]struct{}{},
		hasPrevious:     true,
	}
}

func (b *messageBuffer) setMe(id string, anon bool) {
	b.me = &Sender{ID: id, Anon: anon}
}

// setBaseline anchors the live stream and gap baseline to a ready snapshot's head.
func (b *messageBuffer) setBaseline(seq int64) {
	b.raiseHead(seq)
	if b.contiguous == nil || seq > *b.contiguous {
		s := seq
		b.contiguous = &s
		b.advanceContiguous()
	}
}

// setWatermark sets my read position (from ready.watermark, or advanced by MarkAsRead).
func (b *messageBuffer) setWatermark(seq int64) {
	s := seq
	b.watermark = &s
}

// headSeq is the head seq — what MarkAsRead advances the watermark to.
func (b *messageBuffer) headSeq() (int64, bool) {
	if b.head == nil {
		return 0, false
	}
	return *b.head, true
}

// channelUnread counts unread messages: how far the head runs beyond the watermark.
func (b *messageBuffer) channelUnread() int {
	if b.head == nil || b.watermark == nil {
		return 0
	}
	if n := *b.head - *b.watermark; n > 0 {
		return int(n)
	}
	return 0
}

// contiguousSeq is the `last=` reconnect value: highest contiguous seq held (or
// the baseline). Nil before any ready.
func (b *messageBuffer) contiguousSeq() *int64 {
	if b.contiguous == nil {
		return nil
	}
	s := *b.contiguous
	return &s
}

// lowestSeq is the lowest seq held — the `before=` cursor for the next older page.
func (b *messageBuffer) lowestSeq() *int64 {
	var lowest *int64
	for seq := range b.persistent {
		if lowest == nil || seq < *lowest {
			s := seq
			lowest = &s
		}
	}
	return lowest
}

// gapRange is a maximal run of missing seqs, inclusive.
type gapRange struct{ from, to int64 }

// ingest handles delivered messages (a batch or a direct). Persistent ones are
// stored and deduped, and the newly-stored ones are returned in public form for
// message/mention dispatch. Also reports the missing seq ranges a gap opened,
// for the caller to range-fetch.
//
// Incoming ephemeral messages (no seq) are not modeled by the contract — they
// are dropped here rather than guessed at.
func (b *messageBuffer) ingest(msgs []wire.Message) (delivered []Message, gaps []gapRange) {
	for _, msg := range msgs {
		if msg.Seq == nil || msg.Ephemeral {
			continue
		}
		if stored, ok := b.store(msg); ok {
			delivered = append(delivered, b.toPublic(stored))
		}
	}
	b.advanceContiguous()
	return delivered, b.gaps()
}

// ingestHistory handles an older page or a gap-fill range; never opens a gap.
func (b *messageBuffer) ingestHistory(msgs []wire.Message) {
	for _, msg := range msgs {
		if msg.Seq == nil {
			continue
		}
		b.store(msg)
	}
	b.advanceContiguous()
}

// retract applies a retraction, or remembers it if its message has not arrived yet.
func (b *messageBuffer) retract(seq int64) {
	held, ok := b.persistent[seq]
	if !ok {
		b.pendingRetracts[seq] = struct{}{}
		return
	}
	b.persistent[seq] = tombstone(held)
}

func (b *messageBuffer) addOptimistic(send optimisticSend) {
	send.status = MessagePending
	b.optimistic = append(b.optimistic, send)
}

// ack reconciles an accepted send: drop the optimistic entry, store the durable message.
func (b *messageBuffer) ack(tempID string, ack wire.SendAck) {
	index := b.findOptimistic(tempID)
	if index == -1 {
		return
	}
	opt := b.optimistic[index]
	b.optimistic = append(b.optimistic[:index], b.optimistic[index+1:]...)
	if b.me == nil {
		return
	}
	seq := ack.Seq
	msg := wire.Message{
		ID:        ack.ID,
		Seq:       &seq,
		Type:      opt.msgType,
		Kind:      "text",
		Content:   wire.RawJSON(opt.content),
		Sender:    wire.Sender{ID: b.me.ID, Anon: b.me.Anon},
		Timestamp: ack.Timestamp,
		To:        opt.to,
		Mentions:  toWireMentions(opt.mentions),
	}
	b.store(msg)
	b.advanceContiguous()
	if b.watermark == nil || ack.Seq > *b.watermark {
		b.setWatermark(ack.Seq)
	}
}

// rollback rolls an optimistic send back out of the window (a rejected publish).
func (b *messageBuffer) rollback(tempID string) {
	if index := b.findOptimistic(tempID); index != -1 {
		b.optimistic = append(b.optimistic[:index], b.optimistic[index+1:]...)
	}
}

// reset drops all state (a teardown).
func (b *messageBuffer) reset() {
	b.persistent = map[int64]wire.Message{}
	b.pendingRetracts = map[int64]struct{}{}
	b.optimistic = nil
	b.me = nil
	b.contiguous = nil
	b.head = nil
	b.watermark = nil
	b.hasPrevious = true
}

// messages is the public, seq-ordered window with unacked sends appended.
func (b *messageBuffer) messages() []Message {
	seqs := make([]int64, 0, len(b.persistent))
	for seq := range b.persistent {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	out := make([]Message, 0, len(seqs)+len(b.optimistic))
	for _, seq := range seqs {
		out = append(out, b.toPublic(b.persistent[seq]))
	}
	for _, opt := range b.optimistic {
		out = append(out, b.optimisticToPublic(opt))
	}
	return out
}

// ── Internals ─────────────────────────────────────────────

func (b *messageBuffer) findOptimistic(tempID string) int {
	for i, opt := range b.optimistic {
		if opt.tempID == tempID {
			return i
		}
	}
	return -1
}

// store stores a persistent message, reporting whether it was new (not a dup).
func (b *messageBuffer) store(msg wire.Message) (wire.Message, bool) {
	seq := *msg.Seq
	if _, dup := b.persistent[seq]; dup {
		return wire.Message{}, false
	}
	stored := msg
	if _, pending := b.pendingRetracts[seq]; pending {
		stored = tombstone(msg)
		delete(b.pendingRetracts, seq)
	}
	b.persistent[seq] = stored
	b.raiseHead(seq)
	return stored, true
}

func (b *messageBuffer) raiseHead(seq int64) {
	if b.head == nil || seq > *b.head {
		s := seq
		b.head = &s
	}
}

func tombstone(msg wire.Message) wire.Message {
	msg.Retracted = true
	msg.Content = nil
	return msg
}

func (b *messageBuffer) advanceContiguous() {
	if b.contiguous == nil {
		return
	}
	for {
		if _, ok := b.persistent[*b.contiguous+1]; !ok {
			return
		}
		*b.contiguous++
	}
}

// gaps returns maximal runs of missing seqs above the contiguous head (live gaps only).
func (b *messageBuffer) gaps() []gapRange {
	if b.contiguous == nil {
		return nil
	}
	maxHeld := *b.contiguous
	for seq := range b.persistent {
		if seq > maxHeld {
			maxHeld = seq
		}
	}
	var ranges []gapRange
	var start *int64
	for seq := *b.contiguous + 1; seq <= maxHeld; seq++ {
		if _, held := b.persistent[seq]; !held {
			if start == nil {
				s := seq
				start = &s
			}
		} else if start != nil {
			ranges = append(ranges, gapRange{from: *start, to: seq - 1})
			start = nil
		}
	}
	return ranges
}

func (b *messageBuffer) toPublic(msg wire.Message) Message {
	return Message{
		ID:        msg.ID,
		ChannelID: b.channelID,
		Type:      msg.Type,
		Kind:      "text",
		Content:   json.RawMessage(msg.Content),
		Sender:    Sender{ID: msg.Sender.ID, Anon: msg.Sender.Anon, Username: msg.Sender.Username},
		Timestamp: msg.Timestamp,
		To:        msg.To,
		Mentions:  fromWireMentions(msg.Mentions),
		Retracted: msg.Retracted,
		Ephemeral: msg.Ephemeral,
		Unread:    b.isUnread(msg.Seq),
		Status:    MessageSent,
	}
}

// isUnread: a persistent message is unread when its seq lies beyond my watermark.
func (b *messageBuffer) isUnread(seq *int64) bool {
	return seq != nil && b.watermark != nil && *seq > *b.watermark
}

func (b *messageBuffer) optimisticToPublic(opt optimisticSend) Message {
	sender := Sender{}
	if b.me != nil {
		sender = Sender{ID: b.me.ID, Anon: b.me.Anon}
	}
	return Message{
		ID:        opt.tempID,
		ChannelID: b.channelID,
		Type:      opt.msgType,
		Kind:      "text",
		Content:   opt.content,
		Sender:    sender,
		Timestamp: opt.timestamp,
		To:        opt.to,
		Mentions:  opt.mentions,
		Status:    opt.status,
	}
}

func toWireMentions(mentions []Mention) []wire.Mention {
	if mentions == nil {
		return nil
	}
	out := make([]wire.Mention, len(mentions))
	for i, m := range mentions {
		out[i] = wire.Mention{UserID: m.UserID}
	}
	return out
}

func fromWireMentions(mentions []wire.Mention) []Mention {
	if mentions == nil {
		return nil
	}
	out := make([]Mention, len(mentions))
	for i, m := range mentions {
		out[i] = Mention{UserID: m.UserID}
	}
	return out
}
