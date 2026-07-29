package wire

import (
	"encoding/json"
	"testing"
)

func TestParseChannelReady(t *testing.T) {
	raw := []byte(`{
		"t": "ready",
		"channel": {"id": "room-7", "mode": "standard", "name": "Room 7"},
		"me": {"id": "u_1", "anon": false, "claims": {"role": "admin"}, "capabilities": {"publish": true, "sendDirect": true, "future": "maybe"}},
		"seq": 41,
		"leaf": "leaf-token",
		"presence": {"mode": "detailed", "participants": [{"id": "u_1", "anon": false}], "count": 1},
		"watermark": 40
	}`)
	frame := ParseChannelFrame(raw)
	ready, ok := frame.(*ChannelReadyFrame)
	if !ok {
		t.Fatalf("expected *ChannelReadyFrame, got %T", frame)
	}
	if ready.Channel.ID != "room-7" || ready.Channel.Mode != ModeStandard {
		t.Errorf("bad channel info: %+v", ready.Channel)
	}
	if !ready.Me.Capabilities.Publish() || !ready.Me.Capabilities.SendDirect() {
		t.Errorf("capabilities not decoded: %+v", ready.Me.Capabilities)
	}
	if ready.Seq != 41 || ready.Leaf != "leaf-token" {
		t.Errorf("seq/leaf mismatch: %d %q", ready.Seq, ready.Leaf)
	}
	if ready.Watermark == nil || *ready.Watermark != 40 {
		t.Errorf("watermark mismatch: %v", ready.Watermark)
	}
	if ready.Presence.Mode != "detailed" || len(ready.Presence.Participants) != 1 {
		t.Errorf("presence mismatch: %+v", ready.Presence)
	}
}

func TestParseBatchKeepsSeqAndContent(t *testing.T) {
	raw := []byte(`{"t":"batch","msgs":[
		{"id":"m1","seq":7,"type":"message","kind":"text","content":{"text":"hi"},"sender":{"id":"u_2","anon":true},"timestamp":1700000000000,"retracted":false,"ephemeral":false},
		{"id":"m2","seq":null,"type":"cursor","kind":"text","content":{"x":1},"sender":{"id":"u_3","anon":true},"timestamp":1700000000001,"retracted":false,"ephemeral":true}
	]}`)
	frame := ParseChannelFrame(raw)
	batch, ok := frame.(*BatchFrame)
	if !ok {
		t.Fatalf("expected *BatchFrame, got %T", frame)
	}
	if len(batch.Msgs) != 2 {
		t.Fatalf("expected 2 msgs, got %d", len(batch.Msgs))
	}
	if batch.Msgs[0].Seq == nil || *batch.Msgs[0].Seq != 7 {
		t.Errorf("persistent seq mismatch: %v", batch.Msgs[0].Seq)
	}
	if batch.Msgs[1].Seq != nil {
		t.Errorf("ephemeral seq should be nil, got %v", batch.Msgs[1].Seq)
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(batch.Msgs[0].Content, &content); err != nil || content.Text != "hi" {
		t.Errorf("content not preserved: %s", batch.Msgs[0].Content)
	}
}

func TestParseUnknownFramePassthrough(t *testing.T) {
	raw := []byte(`{"t":"hologram","payload":{"deep":[1,2,3]}}`)
	frame := ParseChannelFrame(raw)
	unknown, ok := frame.(*UnknownFrame)
	if !ok {
		t.Fatalf("expected *UnknownFrame, got %T", frame)
	}
	if unknown.T != "hologram" {
		t.Errorf("unknown t mismatch: %q", unknown.T)
	}
	// Round-trip: serialize returns the original bytes intact.
	out, err := SerializeFrame(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(raw) {
		t.Errorf("unknown frame did not survive round-trip: %s", out)
	}
}

func TestParseTotality(t *testing.T) {
	cases := [][]byte{
		[]byte(`not json at all`),
		[]byte(`42`),
		[]byte(`{"no_t": true}`),
		[]byte(`{"t": 7}`),
		[]byte(`{"t":"batch","msgs":"not-an-array"}`),
	}
	for _, raw := range cases {
		if frame := ParseChannelFrame(raw); frame != nil {
			t.Errorf("expected nil for %s, got %T", raw, frame)
		}
	}
}

func TestFrameFamiliesAreDisjoint(t *testing.T) {
	inboxReady := []byte(`{"t":"ready","entries":[],"items":[],"counter":3}`)
	frame := ParseInboxFrame(inboxReady)
	ready, ok := frame.(*InboxReadyFrame)
	if !ok {
		t.Fatalf("expected *InboxReadyFrame, got %T", frame)
	}
	if ready.Counter != 3 {
		t.Errorf("counter mismatch: %d", ready.Counter)
	}
}

func TestSerializeStampsDiscriminator(t *testing.T) {
	out, err := SerializeFrame(&WatermarkFrame{Seq: 12})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["t"] != "watermark" || decoded["seq"] != float64(12) {
		t.Errorf("bad serialization: %s", out)
	}
}

func TestClientFrameParsers(t *testing.T) {
	frame := ParseChannelClientFrame([]byte(`{"t":"ephemeral","cl":"cl_1","type":"cursor","content":{"x":4}}`))
	eph, ok := frame.(*EphemeralFrame)
	if !ok {
		t.Fatalf("expected *EphemeralFrame, got %T", frame)
	}
	if eph.Cl != "cl_1" || eph.Type != "cursor" {
		t.Errorf("ephemeral mismatch: %+v", eph)
	}
	frame = ParseInboxClientFrame([]byte(`{"t":"mute","channelId":"room-7","muted":true}`))
	mute, ok := frame.(*InboxMuteFrame)
	if !ok {
		t.Fatalf("expected *InboxMuteFrame, got %T", frame)
	}
	if mute.ChannelID != "room-7" || !mute.Muted {
		t.Errorf("mute mismatch: %+v", mute)
	}
}

func TestRefusalCodes(t *testing.T) {
	if !IsRefusalCode("token_expired") || IsRefusalCode("nonsense") {
		t.Error("IsRefusalCode misclassified")
	}
	if RefusalStatus[RefusalChannelAtCapacity] != 429 {
		t.Error("refusal status table wrong")
	}
}
