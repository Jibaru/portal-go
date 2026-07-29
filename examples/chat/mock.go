package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Jibaru/portal-go/wire"
)

// mockPortal is a tiny in-process Portal server speaking protocol v1, so the
// chat client can be exercised end-to-end with no account and no network:
// anonymous mint, channel upgrade + ready, HTTP publish + batch fan-out,
// history (backfill and gap-fill), ping/pong — plus a bot that answers you.
type mockPortal struct {
	server   *httptest.Server
	upgrader websocket.Upgrader

	mu      sync.Mutex
	seq     int64
	history []wire.Message
	conns   []*mockConn
}

type mockConn struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (c *mockConn) write(frame wire.Frame) {
	data, err := wire.SerializeFrame(frame)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.WriteMessage(websocket.TextMessage, data)
}

// startMockPortal returns the API URL and realtime URL to point the client at.
func startMockPortal() (apiURL, realtimeURL string) {
	m := &mockPortal{}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	return m.server.URL, "ws://" + strings.TrimPrefix(m.server.URL, "http://")
}

func (m *mockPortal) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/v1/tokens/anonymous":
		m.handleMint(w, r)
	case strings.HasSuffix(path, "/messages"):
		m.handlePublish(w, r)
	case strings.HasSuffix(path, "/history"):
		m.handleHistory(w, r)
	case path == "/inbox":
		m.handleInbox(w, r)
	case strings.HasPrefix(path, "/v1/channels/"):
		m.handleChannel(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (m *mockPortal) handleMint(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AnonID string `json:"anonId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	sub := body.AnonID
	if sub == "" {
		sub = "anon_you"
	}
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"sub": sub, "exp": time.Now().Add(time.Hour).Unix()})
	token := head + "." + base64.RawURLEncoding.EncodeToString(payload) + ".mock"
	_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
}

func (m *mockPortal) handleChannel(w http.ResponseWriter, r *http.Request) {
	channelID := strings.TrimPrefix(r.URL.Path, "/v1/channels/")
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	mc := &mockConn{conn: conn}
	m.mu.Lock()
	head := m.seq
	m.conns = append(m.conns, mc)
	m.mu.Unlock()

	mc.write(&wire.ChannelReadyFrame{
		Channel: wire.ChannelInfo{ID: channelID, Mode: wire.ModeStandard, Name: "Mock room"},
		Me: wire.MeInfo{
			ID: "anon_you", Anon: true,
			Claims:       map[string]any{},
			Capabilities: wire.Capabilities{"publish": true},
		},
		Seq:  head,
		Leaf: "leaf-mock",
		Presence: wire.PresenceSnapshot{
			Mode: "detailed",
			Participants: []wire.PresenceParticipant{
				{ID: "anon_you", Anon: true},
				{ID: "bot", Username: "bot"},
			},
			Count: 2,
		},
	})

	go func() {
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.TextMessage {
				continue
			}
			switch wire.ParseChannelClientFrame(data).(type) {
			case *wire.PingFrame:
				mc.write(&wire.PongFrame{})
			}
		}
	}()
}

// handleInbox accepts the upgrade and serves an empty, ready inbox.
func (m *mockPortal) handleInbox(w http.ResponseWriter, r *http.Request) {
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	mc := &mockConn{conn: conn}
	mc.write(&wire.InboxReadyFrame{Entries: []wire.InboxEntry{}, Items: []wire.InboxItem{}})
	go func() {
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.TextMessage {
				continue
			}
			switch wire.ParseInboxClientFrame(data).(type) {
			case *wire.PingFrame:
				mc.write(&wire.PongFrame{})
			}
		}
	}()
}

func (m *mockPortal) handlePublish(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(wire.PublishErrorBody{Code: "not_permitted"})
		return
	}
	var body wire.PublishBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		return
	}
	content, _ := json.Marshal(body.Content)
	msg := m.append("anon_you", content)
	m.broadcast(&wire.BatchFrame{Msgs: []wire.Message{msg}})
	_ = json.NewEncoder(w).Encode(wire.SendAck{ID: msg.ID, Seq: *msg.Seq, Timestamp: msg.Timestamp})

	// The bot reads what you said and answers shortly after.
	go m.botReply(content)
}

func (m *mockPortal) botReply(content json.RawMessage) {
	time.Sleep(400 * time.Millisecond)
	var said struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(content, &said)
	reply, _ := json.Marshal(map[string]string{
		"text": "you said " + strconv.Quote(said.Text) + " — the round-trip works!",
	})
	msg := m.append("bot", reply)
	m.broadcast(&wire.BatchFrame{Msgs: []wire.Message{msg}})
}

func (m *mockPortal) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	parse := func(key string) (int64, bool) {
		v := q.Get(key)
		if v == "" {
			return 0, false
		}
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	from, hasFrom := parse("from")
	to, hasTo := parse("to")
	before, hasBefore := parse("before")
	m.mu.Lock()
	msgs := []wire.Message{}
	for _, msg := range m.history {
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
	m.mu.Unlock()
	_ = json.NewEncoder(w).Encode(wire.HistoryResponse{Msgs: msgs, HasMore: false})
}

func (m *mockPortal) append(senderID string, content json.RawMessage) wire.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	seq := m.seq
	sender := wire.Sender{ID: senderID, Anon: senderID != "bot"}
	if senderID == "bot" {
		sender.Username = "bot"
	}
	msg := wire.Message{
		ID: "m_" + strconv.FormatInt(seq, 10), Seq: &seq, Type: "message", Kind: "text",
		Content: wire.RawJSON(content), Sender: sender, Timestamp: time.Now().UnixMilli(),
	}
	m.history = append(m.history, msg)
	return msg
}

func (m *mockPortal) broadcast(frame wire.Frame) {
	m.mu.Lock()
	conns := append([]*mockConn(nil), m.conns...)
	m.mu.Unlock()
	for _, mc := range conns {
		mc.write(frame)
	}
}
