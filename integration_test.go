package portal_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	portal "github.com/Jibaru/portal-go"
	"github.com/Jibaru/portal-go/wire"
)

// ── Mock Portal server ────────────────────────────────────
//
// Speaks the wire protocol as reverse-engineered from @portalsdk/core: ws
// upgrades at /v1/channels/{id} and /inbox, publish/history over HTTP on the
// same origin, anonymous mint on the API origin.

type mockServer struct {
	t        *testing.T
	server   *httptest.Server
	upgrader websocket.Upgrader

	mu       sync.Mutex
	seq      int64
	history  []wire.Message
	channels []*mockConn
	inboxes  []*mockConn

	publishAuth   []string // bearer tokens seen on publish
	watermarks    []int64
	inboxRead     []string // channelIds from read frames
	itemReads     []string // ids from item.read frames
	refuseChannel map[string]wire.RefusalCode
}

type mockConn struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (c *mockConn) write(frame wire.Frame) error {
	data, err := wire.SerializeFrame(frame)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func fakeJWT(sub string, exp int64) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"sub": sub, "exp": exp})
	return head + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func newMockServer(t *testing.T) *mockServer {
	s := &mockServer{t: t, refuseChannel: map[string]wire.RefusalCode{}}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

func (s *mockServer) clientConfig() portal.Config {
	return portal.Config{
		APIKey:      "pk_test",
		APIURL:      s.server.URL,
		RealtimeURL: "ws://" + strings.TrimPrefix(s.server.URL, "http://"),
	}
}

func (s *mockServer) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/v1/tokens/anonymous":
		var body struct {
			AnonID string `json:"anonId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sub := body.AnonID
		if sub == "" {
			sub = "anon_42"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token": fakeJWT(sub, time.Now().Add(time.Hour).Unix()),
		})
	case path == "/inbox":
		s.handleInbox(w, r)
	case strings.HasSuffix(path, "/messages"):
		s.handlePublish(w, r)
	case strings.HasSuffix(path, "/history"):
		s.handleHistory(w, r)
	case strings.HasPrefix(path, "/v1/channels/"):
		s.handleChannel(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *mockServer) handleChannel(w http.ResponseWriter, r *http.Request) {
	channelID := strings.TrimPrefix(r.URL.Path, "/v1/channels/")
	s.mu.Lock()
	refusal, refuse := s.refuseChannel[channelID]
	s.mu.Unlock()
	if refuse {
		w.Header().Set(wire.ErrorHeader, string(refusal))
		w.WriteHeader(wire.RefusalStatus[refusal])
		_ = json.NewEncoder(w).Encode(wire.RefusalBody{Code: refusal})
		return
	}
	token := r.URL.Query().Get(wire.ParamToken)
	if token == "" || r.URL.Query().Get(wire.ParamKey) != "pk_test" {
		w.Header().Set(wire.ErrorHeader, string(wire.RefusalInvalidAPIKey))
		w.WriteHeader(403)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	mc := &mockConn{conn: conn}
	s.mu.Lock()
	head := s.seq
	s.channels = append(s.channels, mc)
	s.mu.Unlock()

	_ = mc.write(&wire.ChannelReadyFrame{
		Channel: wire.ChannelInfo{ID: channelID, Mode: wire.ModeStandard, Name: "Mock room"},
		Me: wire.MeInfo{
			ID: "anon_42", Anon: true,
			Claims:       map[string]any{},
			Capabilities: wire.Capabilities{"publish": true},
		},
		Seq:  head,
		Leaf: "leaf-1",
		Presence: wire.PresenceSnapshot{
			Mode:         "detailed",
			Participants: []wire.PresenceParticipant{{ID: "anon_42", Anon: true}},
			Count:        1,
		},
	})

	go s.readLoop(mc, false)
}

func (s *mockServer) handleInbox(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	mc := &mockConn{conn: conn}
	s.mu.Lock()
	s.inboxes = append(s.inboxes, mc)
	s.mu.Unlock()
	_ = mc.write(&wire.InboxReadyFrame{
		Entries: []wire.InboxEntry{{ID: "room-7", Name: "Room 7", Unread: 2, At: 1700000000000}},
		Items:   []wire.InboxItem{},
		Counter: 2,
	})
	go s.readLoop(mc, true)
}

func (s *mockServer) readLoop(mc *mockConn, inbox bool) {
	for {
		kind, data, err := mc.conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.TextMessage {
			continue
		}
		if inbox {
			switch f := wire.ParseInboxClientFrame(data).(type) {
			case *wire.PingFrame:
				_ = mc.write(&wire.PongFrame{})
			case *wire.InboxReadFrame:
				s.mu.Lock()
				s.inboxRead = append(s.inboxRead, f.ChannelID)
				s.mu.Unlock()
			case *wire.InboxItemReadFrame:
				s.mu.Lock()
				s.itemReads = append(s.itemReads, f.ID)
				s.mu.Unlock()
			}
			continue
		}
		switch f := wire.ParseChannelClientFrame(data).(type) {
		case *wire.PingFrame:
			_ = mc.write(&wire.PongFrame{})
		case *wire.WatermarkFrame:
			s.mu.Lock()
			s.watermarks = append(s.watermarks, f.Seq)
			s.mu.Unlock()
		}
	}
}

func (s *mockServer) handlePublish(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || r.Header.Get(wire.APIKeyHeader) != "pk_test" {
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(wire.PublishErrorBody{Code: "not_permitted"})
		return
	}
	var body wire.PublishBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		return
	}
	if s.isBlockedContent(body) {
		w.WriteHeader(403)
		_ = json.NewEncoder(w).Encode(wire.PublishErrorBody{
			Code: wire.PublishBlockedByMiddleware, Reason: "watch your language",
		})
		return
	}
	content, _ := json.Marshal(body.Content)
	msg := s.appendMessage("anon_42", body.Type, content)
	s.mu.Lock()
	s.publishAuth = append(s.publishAuth, strings.TrimPrefix(auth, "Bearer "))
	s.mu.Unlock()
	s.broadcast(&wire.BatchFrame{Msgs: []wire.Message{msg}})
	_ = json.NewEncoder(w).Encode(wire.SendAck{ID: msg.ID, Seq: *msg.Seq, Timestamp: msg.Timestamp})
}

func (s *mockServer) isBlockedContent(body wire.PublishBody) bool {
	raw, _ := json.Marshal(body.Content)
	return strings.Contains(string(raw), "BLOCKME")
}

func (s *mockServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, hasFrom := parseIntParam(q.Get("from"))
	to, hasTo := parseIntParam(q.Get("to"))
	before, hasBefore := parseIntParam(q.Get("before"))
	s.mu.Lock()
	var msgs []wire.Message
	for _, msg := range s.history {
		seq := *msg.Seq
		if hasFrom && seq < from {
			continue
		}
		if hasTo && seq > to {
			continue
		}
		if hasBefore && seq >= before {
			continue
		}
		msgs = append(msgs, msg)
	}
	s.mu.Unlock()
	_ = json.NewEncoder(w).Encode(wire.HistoryResponse{Msgs: msgs, HasMore: false})
}

func parseIntParam(v string) (int64, bool) {
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

// appendMessage stores a new message (assigning the next seq) without notifying.
func (s *mockServer) appendMessage(senderID, msgType string, content json.RawMessage) wire.Message {
	if msgType == "" {
		msgType = "message"
	}
	s.mu.Lock()
	s.seq++
	seq := s.seq
	msg := wire.Message{
		ID: "m_" + strconv.FormatInt(seq, 10), Seq: &seq, Type: msgType, Kind: "text",
		Content: wire.RawJSON(content), Sender: wire.Sender{ID: senderID, Anon: senderID != "u_human"},
		Timestamp: time.Now().UnixMilli(),
	}
	s.history = append(s.history, msg)
	s.mu.Unlock()
	return msg
}

func (s *mockServer) broadcast(frame wire.Frame) {
	s.mu.Lock()
	conns := append([]*mockConn(nil), s.channels...)
	s.mu.Unlock()
	for _, mc := range conns {
		_ = mc.write(frame)
	}
}

func (s *mockServer) broadcastInbox(frame wire.Frame) {
	s.mu.Lock()
	conns := append([]*mockConn(nil), s.inboxes...)
	s.mu.Unlock()
	for _, mc := range conns {
		_ = mc.write(frame)
	}
}

// ── Helpers ───────────────────────────────────────────────

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ── Tests ─────────────────────────────────────────────────

func TestConnectReadyAndReceive(t *testing.T) {
	srv := newMockServer(t)
	client := portal.New(srv.clientConfig())

	ch := client.Channel("room-7")
	var received []portal.Message
	var mu sync.Mutex
	ch.OnMessage(func(m portal.Message) {
		mu.Lock()
		received = append(received, m)
		mu.Unlock()
	})
	ch.Acquire()
	defer ch.Release()

	waitFor(t, 5*time.Second, "status ready", func() bool { return ch.Status() == portal.StatusReady })

	me := ch.Me()
	if me == nil || me.ID != "anon_42" || !me.Anon {
		t.Fatalf("me mismatch: %+v", me)
	}
	if info := ch.Info(); info == nil || info.Name != "Mock room" {
		t.Fatalf("info mismatch: %+v", info)
	}
	if p := ch.Presence(); p == nil || p.Kind != portal.PresenceDetailed || p.Count != 1 {
		t.Fatalf("presence mismatch: %+v", p)
	}

	// Another participant speaks.
	msg := srv.appendMessage("u_human", "message", json.RawMessage(`{"text":"hello from the server"}`))
	srv.broadcast(&wire.BatchFrame{Msgs: []wire.Message{msg}})

	waitFor(t, 5*time.Second, "message delivery", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 1
	})
	mu.Lock()
	got := received[0]
	mu.Unlock()
	var content struct {
		Text string `json:"text"`
	}
	if err := got.DecodeContent(&content); err != nil || content.Text != "hello from the server" {
		t.Fatalf("content mismatch: %s", got.Content)
	}
	if got.Sender.ID != "u_human" || got.Status != portal.MessageSent {
		t.Fatalf("envelope mismatch: %+v", got)
	}
}

func TestSendPersistent(t *testing.T) {
	srv := newMockServer(t)
	client := portal.New(srv.clientConfig())
	ch := client.Channel("room-7")
	ch.Acquire()
	defer ch.Release()
	waitFor(t, 5*time.Second, "ready", func() bool { return ch.Status() == portal.StatusReady })

	ack, err := ch.Send(context.Background(), portal.SendInput{Content: map[string]string{"text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if ack.ID == "" {
		t.Fatal("empty ack id")
	}

	msgs := ch.Messages()
	if len(msgs) != 1 || msgs[0].Status != portal.MessageSent || msgs[0].ID != ack.ID {
		t.Fatalf("window after send: %+v", msgs)
	}

	// The publish authenticated with the minted anonymous token.
	srv.mu.Lock()
	auth := append([]string(nil), srv.publishAuth...)
	srv.mu.Unlock()
	if len(auth) != 1 || !strings.Contains(auth[0], ".") {
		t.Fatalf("publish auth not seen: %v", auth)
	}

	// The broadcast echo of our own message must dedup, not duplicate.
	time.Sleep(200 * time.Millisecond)
	if n := len(ch.Messages()); n != 1 {
		t.Fatalf("expected 1 message after echo, got %d", n)
	}
}

func TestSendBlockedByMiddleware(t *testing.T) {
	srv := newMockServer(t)
	client := portal.New(srv.clientConfig())
	ch := client.Channel("room-7")
	ch.Acquire()
	defer ch.Release()
	waitFor(t, 5*time.Second, "ready", func() bool { return ch.Status() == portal.StatusReady })

	_, err := ch.Send(context.Background(), portal.SendInput{Content: map[string]string{"text": "BLOCKME"}})
	if !portal.IsBlocked(err) {
		t.Fatalf("expected blocked error, got %v", err)
	}
	var perr *portal.Error
	if !asPortalErr(err, &perr) || perr.Reason != "watch your language" {
		t.Fatalf("reason not carried: %v", err)
	}
	// The optimistic insert rolled back.
	if n := len(ch.Messages()); n != 0 {
		t.Fatalf("expected rollback, window has %d", n)
	}
}

func asPortalErr(err error, target **portal.Error) bool {
	e, ok := err.(*portal.Error)
	if ok {
		*target = e
	}
	return ok
}

func TestRefusalBlocksTerminally(t *testing.T) {
	srv := newMockServer(t)
	srv.refuseChannel["locked"] = wire.RefusalNotMember

	client := portal.New(srv.clientConfig())
	ch := client.Channel("locked")
	var mu sync.Mutex
	var lastErr error
	ch.OnStatus(func(status portal.ChannelStatus, err error) {
		if status == portal.StatusBlocked {
			mu.Lock()
			lastErr = err
			mu.Unlock()
		}
	})
	ch.Acquire()
	defer ch.Release()

	waitFor(t, 5*time.Second, "blocked status", func() bool { return ch.Status() == portal.StatusBlocked })
	mu.Lock()
	err := lastErr
	mu.Unlock()
	if !portal.IsNotMember(err) {
		t.Fatalf("expected NotMember, got %v", err)
	}
}

func TestGapFill(t *testing.T) {
	srv := newMockServer(t)
	client := portal.New(srv.clientConfig())
	ch := client.Channel("room-7")
	ch.Acquire()
	defer ch.Release()
	waitFor(t, 5*time.Second, "ready", func() bool { return ch.Status() == portal.StatusReady })

	// seq 1 delivered; seq 2 dropped (stored only); seq 3 delivered → gap [2,2].
	m1 := srv.appendMessage("u_human", "message", json.RawMessage(`{"n":1}`))
	srv.broadcast(&wire.BatchFrame{Msgs: []wire.Message{m1}})
	_ = srv.appendMessage("u_human", "message", json.RawMessage(`{"n":2}`))
	m3 := srv.appendMessage("u_human", "message", json.RawMessage(`{"n":3}`))
	srv.broadcast(&wire.BatchFrame{Msgs: []wire.Message{m3}})

	// Gap fill fires after 0–2s jitter and fetches history?from=2&to=2.
	waitFor(t, 6*time.Second, "gap-filled window", func() bool { return len(ch.Messages()) == 3 })
	msgs := ch.Messages()
	for i, m := range msgs {
		var content struct{ N int }
		_ = m.DecodeContent(&content)
		if content.N != i+1 {
			t.Fatalf("window out of order at %d: %+v", i, msgs)
		}
	}
}

func TestRetractTombstones(t *testing.T) {
	srv := newMockServer(t)
	client := portal.New(srv.clientConfig())
	ch := client.Channel("room-7")
	ch.Acquire()
	defer ch.Release()
	waitFor(t, 5*time.Second, "ready", func() bool { return ch.Status() == portal.StatusReady })

	msg := srv.appendMessage("u_human", "message", json.RawMessage(`{"text":"oops"}`))
	srv.broadcast(&wire.BatchFrame{Msgs: []wire.Message{msg}})
	waitFor(t, 5*time.Second, "delivery", func() bool { return len(ch.Messages()) == 1 })

	var retracted []string
	var mu sync.Mutex
	ch.OnRetract(func(id string) {
		mu.Lock()
		retracted = append(retracted, id)
		mu.Unlock()
	})
	srv.broadcast(&wire.RetractFrame{ID: msg.ID, Seq: *msg.Seq})

	waitFor(t, 5*time.Second, "tombstone", func() bool {
		msgs := ch.Messages()
		return len(msgs) == 1 && msgs[0].Retracted
	})
	if ch.Messages()[0].Content != nil {
		t.Fatal("tombstone content should be stripped")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(retracted) != 1 || retracted[0] != msg.ID {
		t.Fatalf("retract event mismatch: %v", retracted)
	}
}

func TestMarkAsReadSendsWatermark(t *testing.T) {
	srv := newMockServer(t)
	client := portal.New(srv.clientConfig())
	ch := client.Channel("room-7")
	ch.Acquire()
	defer ch.Release()
	waitFor(t, 5*time.Second, "ready", func() bool { return ch.Status() == portal.StatusReady })

	msg := srv.appendMessage("u_human", "message", json.RawMessage(`{"text":"unread me"}`))
	srv.broadcast(&wire.BatchFrame{Msgs: []wire.Message{msg}})
	waitFor(t, 5*time.Second, "unread", func() bool { return ch.Unread() == 1 })

	ch.MarkAsRead()
	if ch.Unread() != 0 {
		t.Fatalf("unread after MarkAsRead: %d", ch.Unread())
	}
	waitFor(t, 5*time.Second, "watermark frame", func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.watermarks) == 1 && srv.watermarks[0] == *msg.Seq
	})
}

func TestInbox(t *testing.T) {
	srv := newMockServer(t)
	client := portal.New(srv.clientConfig())

	inbox := client.Inbox()
	waitFor(t, 5*time.Second, "inbox ready", func() bool { return inbox.Status() == portal.InboxReady })

	snap := inbox.Snapshot()
	if snap.Counter != 2 || len(snap.Channels) != 1 || snap.Channels[0].ID != "room-7" {
		t.Fatalf("inbox snapshot mismatch: %+v", snap)
	}

	var items []portal.InboxItem
	var mu sync.Mutex
	inbox.OnItem(func(item portal.InboxItem) {
		mu.Lock()
		items = append(items, item)
		mu.Unlock()
	})
	srv.broadcastInbox(&wire.InboxItemFrame{Item: wire.InboxItem{
		ID: "evt_1", Type: "mention", Data: wire.RawJSON(`{"messageId":"m_9"}`),
		ChannelID: "room-7", At: time.Now().UnixMilli(),
	}})
	waitFor(t, 5*time.Second, "item event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(items) == 1
	})

	// Redelivery of the same id is the same event — no second OnItem.
	srv.broadcastInbox(&wire.InboxItemFrame{Item: wire.InboxItem{
		ID: "evt_1", Type: "mention", Data: wire.RawJSON(`{"messageId":"m_9"}`),
		ChannelID: "room-7", At: time.Now().UnixMilli(),
	}})
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	if len(items) != 1 {
		mu.Unlock()
		t.Fatal("redelivered id re-fired OnItem")
	}
	mu.Unlock()

	// Views filter the item feed.
	mentions := inbox.View(portal.InboxQuery{Where: portal.Where{"type": {Eq: []any{"mention"}}}})
	if got := len(mentions.Items()); got != 1 {
		t.Fatalf("view should hold the mention, got %d", got)
	}
	other := inbox.View(portal.InboxQuery{Where: portal.Where{"type": {Eq: []any{"ticket.assigned"}}}})
	if got := len(other.Items()); got != 0 {
		t.Fatalf("view should be empty, got %d", got)
	}

	// Read actions reach the server.
	inbox.MarkItemRead("evt_1")
	inbox.MarkChannelRead("room-7")
	waitFor(t, 5*time.Second, "read frames", func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.itemReads) == 1 && len(srv.inboxRead) == 1
	})
}

func TestEphemeralSendGoesOverSocket(t *testing.T) {
	srv := newMockServer(t)
	client := portal.New(srv.clientConfig())
	ch := client.Channel("room-7")
	ch.Acquire()
	defer ch.Release()
	waitFor(t, 5*time.Second, "ready", func() bool { return ch.Status() == portal.StatusReady })

	ack, err := ch.Send(context.Background(), portal.SendInput{
		Ephemeral: true,
		Type:      "cursor",
		Content:   map[string]int{"x": 10, "y": 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ack.ID, "cl_") {
		t.Fatalf("ephemeral ack should carry the client tag, got %q", ack.ID)
	}
	// Ephemeral sends never join the persistent window.
	if n := len(ch.Messages()); n != 0 {
		t.Fatalf("ephemeral leaked into window: %d", n)
	}
	// And no HTTP publish happened.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.publishAuth) != 0 {
		t.Fatal("ephemeral send must not hit the publish endpoint")
	}
}
