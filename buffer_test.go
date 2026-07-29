package portal

import (
	"encoding/json"
	"testing"

	"github.com/Jibaru/portal-go/wire"
)

func wireMsg(id string, seq int64, text string) wire.Message {
	s := seq
	content, _ := json.Marshal(map[string]string{"text": text})
	return wire.Message{
		ID: id, Seq: &s, Type: "message", Kind: "text",
		Content: content, Sender: wire.Sender{ID: "u_other"}, Timestamp: 1700000000000 + seq,
	}
}

func TestBufferOrdersBySeq(t *testing.T) {
	b := newMessageBuffer("room")
	b.setBaseline(0)
	b.ingest([]wire.Message{wireMsg("m3", 3, "c"), wireMsg("m1", 1, "a"), wireMsg("m2", 2, "b")})
	msgs := b.messages()
	if len(msgs) != 3 || msgs[0].ID != "m1" || msgs[1].ID != "m2" || msgs[2].ID != "m3" {
		t.Fatalf("bad order: %+v", msgs)
	}
}

func TestBufferDedups(t *testing.T) {
	b := newMessageBuffer("room")
	b.setBaseline(0)
	first, _ := b.ingest([]wire.Message{wireMsg("m1", 1, "a")})
	second, _ := b.ingest([]wire.Message{wireMsg("m1-dup", 1, "a2")})
	if len(first) != 1 || len(second) != 0 {
		t.Fatalf("dedup failed: %d %d", len(first), len(second))
	}
	if len(b.messages()) != 1 {
		t.Fatalf("window should hold 1, holds %d", len(b.messages()))
	}
}

func TestBufferDetectsGaps(t *testing.T) {
	b := newMessageBuffer("room")
	b.setBaseline(5)
	_, gaps := b.ingest([]wire.Message{wireMsg("m6", 6, "a"), wireMsg("m9", 9, "b")})
	if len(gaps) != 1 || gaps[0].from != 7 || gaps[0].to != 8 {
		t.Fatalf("expected gap [7,8], got %+v", gaps)
	}
	// Filling the gap advances contiguous and clears it.
	b.ingestHistory([]wire.Message{wireMsg("m7", 7, "c"), wireMsg("m8", 8, "d")})
	if got := b.contiguousSeq(); got == nil || *got != 9 {
		t.Fatalf("contiguous should be 9, got %v", got)
	}
	if _, gaps := b.ingest(nil); len(gaps) != 0 {
		t.Fatalf("expected no gaps, got %+v", gaps)
	}
}

func TestBufferRetractBeforeArrival(t *testing.T) {
	b := newMessageBuffer("room")
	b.setBaseline(0)
	// Retraction outruns its message.
	b.retract(2)
	b.ingest([]wire.Message{wireMsg("m2", 2, "secret")})
	msgs := b.messages()
	if len(msgs) != 1 || !msgs[0].Retracted {
		t.Fatalf("expected tombstoned message, got %+v", msgs)
	}
	if msgs[0].Content != nil {
		t.Fatalf("tombstone content should be stripped, got %s", msgs[0].Content)
	}
}

func TestBufferWatermarkUnread(t *testing.T) {
	b := newMessageBuffer("room")
	b.setBaseline(10)
	b.setWatermark(10)
	b.ingest([]wire.Message{wireMsg("m11", 11, "a"), wireMsg("m12", 12, "b")})
	if b.channelUnread() != 2 {
		t.Fatalf("expected 2 unread, got %d", b.channelUnread())
	}
	msgs := b.messages()
	for _, m := range msgs {
		if !m.Unread {
			t.Errorf("message %s should be unread", m.ID)
		}
	}
	head, _ := b.headSeq()
	b.setWatermark(head)
	if b.channelUnread() != 0 {
		t.Fatalf("expected 0 unread after markAsRead, got %d", b.channelUnread())
	}
}

func TestBufferOptimisticLifecycle(t *testing.T) {
	b := newMessageBuffer("room")
	b.setMe("u_me", false)
	b.setBaseline(0)
	content, _ := json.Marshal(map[string]string{"text": "hello"})
	b.addOptimistic(optimisticSend{tempID: "cl_1", msgType: "message", content: content, timestamp: 1})

	msgs := b.messages()
	if len(msgs) != 1 || msgs[0].Status != MessagePending || msgs[0].ID != "cl_1" {
		t.Fatalf("expected pending optimistic, got %+v", msgs)
	}

	b.ack("cl_1", wire.SendAck{ID: "m_real", Seq: 1, Timestamp: 2})
	msgs = b.messages()
	if len(msgs) != 1 || msgs[0].Status != MessageSent || msgs[0].ID != "m_real" {
		t.Fatalf("expected acked message, got %+v", msgs)
	}
	// Own send advances the watermark: your own message is not unread.
	if b.channelUnread() != 0 {
		t.Fatalf("own send should not count unread, got %d", b.channelUnread())
	}
	// The broadcast copy of the same seq arrives later — deduped.
	delivered, _ := b.ingest([]wire.Message{wireMsg("m_real", 1, "hello")})
	if len(delivered) != 0 || len(b.messages()) != 1 {
		t.Fatalf("broadcast echo should dedup, delivered=%d window=%d", len(delivered), len(b.messages()))
	}
}

func TestBufferRollback(t *testing.T) {
	b := newMessageBuffer("room")
	b.setMe("u_me", false)
	content, _ := json.Marshal(map[string]string{"text": "nope"})
	b.addOptimistic(optimisticSend{tempID: "cl_1", msgType: "message", content: content, timestamp: 1})
	b.rollback("cl_1")
	if len(b.messages()) != 0 {
		t.Fatalf("rollback should empty the window, got %+v", b.messages())
	}
}

func TestBufferReconnectBaselineGap(t *testing.T) {
	// Reconnect: ready arrives with a head far beyond what we hold — the
	// connection schedules [heldBefore+1, head]; the buffer itself just anchors.
	b := newMessageBuffer("room")
	b.setBaseline(3)
	b.ingest([]wire.Message{wireMsg("m4", 4, "a")})
	held := b.contiguousSeq()
	if held == nil || *held != 4 {
		t.Fatalf("contiguous should be 4, got %v", held)
	}
	b.setBaseline(9)
	if got := b.contiguousSeq(); got == nil || *got != 9 {
		t.Fatalf("baseline should raise contiguous to 9, got %v", got)
	}
}

func TestBufferLowestSeqPaging(t *testing.T) {
	b := newMessageBuffer("room")
	b.setBaseline(0)
	b.ingest([]wire.Message{wireMsg("m5", 5, "a"), wireMsg("m6", 6, "b")})
	lowest := b.lowestSeq()
	if lowest == nil || *lowest != 5 {
		t.Fatalf("lowest should be 5, got %v", lowest)
	}
}
