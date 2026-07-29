package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Jibaru/portal-go/wire"
)

// relay is a self-hosted Portal-protocol server (v1) with PER-CHANNEL routing:
// each channel id has its own seq counter, history and connection set, so the
// lobby directory and every lobby room are fully isolated — exactly how the
// hosted service scopes channels.
type relay struct {
	upgrader websocket.Upgrader

	mu       sync.Mutex
	channels map[string]*relayChannel
}

type relayChannel struct {
	seq     int64
	history []wire.Message
	conns   []*relayConn
}

const relayHistoryCap = 300

type relayConn struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (c *relayConn) write(frame wire.Frame) {
	data, err := wire.SerializeFrame(frame)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = c.conn.WriteMessage(websocket.TextMessage, data)
}

// startRelay binds a relay to addr and returns the address it listens on.
func startRelay(addr string) (string, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	r := &relay{channels: map[string]*relayChannel{}}
	server := &http.Server{Handler: http.HandlerFunc(r.handle)}
	go func() { _ = server.Serve(listener) }()
	return listener.Addr().String(), nil
}

func (r *relay) channelState(id string) *relayChannel {
	if ch, ok := r.channels[id]; ok {
		return ch
	}
	ch := &relayChannel{}
	r.channels[id] = ch
	return ch
}

func (r *relay) handle(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	switch {
	case path == "/v1/tokens/anonymous":
		r.handleMint(w, req)
	case strings.HasSuffix(path, "/messages"):
		r.handlePublish(w, req)
	case strings.HasSuffix(path, "/history"):
		r.handleHistory(w, req)
	case strings.HasPrefix(path, "/v1/channels/"):
		r.handleChannel(w, req)
	default:
		http.NotFound(w, req)
	}
}

func channelIDFromPath(path, suffix string) string {
	id := strings.TrimPrefix(path, "/v1/channels/")
	id = strings.TrimSuffix(id, suffix)
	if unescaped, err := url.PathUnescape(id); err == nil {
		return unescaped
	}
	return id
}

func (r *relay) handleMint(w http.ResponseWriter, req *http.Request) {
	var body struct {
		AnonID string `json:"anonId"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	sub := body.AnonID
	if sub == "" {
		var raw [4]byte
		_, _ = rand.Read(raw[:])
		sub = "anon_" + hex.EncodeToString(raw[:])
	}
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"sub": sub, "exp": time.Now().Add(12 * time.Hour).Unix()})
	token := head + "." + base64.RawURLEncoding.EncodeToString(payload) + ".relay"
	_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
}

func subFromToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "anon_unknown"
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "anon_unknown"
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	_ = json.Unmarshal(payload, &claims)
	if claims.Sub == "" {
		return "anon_unknown"
	}
	return claims.Sub
}

func (r *relay) handleChannel(w http.ResponseWriter, req *http.Request) {
	channelID := channelIDFromPath(req.URL.Path, "")
	sub := subFromToken(req.URL.Query().Get(wire.ParamToken))
	conn, err := r.upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	rc := &relayConn{conn: conn}
	r.mu.Lock()
	ch := r.channelState(channelID)
	head := ch.seq
	ch.conns = append(ch.conns, rc)
	count := len(ch.conns)
	r.mu.Unlock()

	rc.write(&wire.ChannelReadyFrame{
		Channel: wire.ChannelInfo{ID: channelID, Mode: wire.ModeStandard},
		Me: wire.MeInfo{
			ID: sub, Anon: true,
			Claims:       map[string]any{},
			Capabilities: wire.Capabilities{"publish": true},
		},
		Seq:  head,
		Leaf: "leaf-" + sub,
		Presence: wire.PresenceSnapshot{
			Mode:  "detailed",
			Count: count,
		},
	})

	go r.readLoop(channelID, sub, rc)
}

func (r *relay) readLoop(channelID, sub string, rc *relayConn) {
	defer r.drop(channelID, rc)
	for {
		kind, data, err := rc.conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.TextMessage {
			continue
		}
		switch f := wire.ParseChannelClientFrame(data).(type) {
		case *wire.PingFrame:
			rc.write(&wire.PongFrame{})
		case *wire.EphemeralFrame:
			// Fast lane: fan out within THIS channel only, skipping the sender.
			msg := wire.Message{
				ID: "e_" + sub + "_" + f.Cl, Type: f.Type, Kind: "text",
				Content: f.Content, Sender: wire.Sender{ID: sub, Anon: true},
				Timestamp: time.Now().UnixMilli(), Ephemeral: true,
			}
			frame := &wire.BatchFrame{Msgs: []wire.Message{msg}}
			r.mu.Lock()
			conns := append([]*relayConn(nil), r.channelState(channelID).conns...)
			r.mu.Unlock()
			for _, other := range conns {
				if other != rc {
					other.write(frame)
				}
			}
		}
	}
}

func (r *relay) drop(channelID string, rc *relayConn) {
	r.mu.Lock()
	ch := r.channelState(channelID)
	for i, held := range ch.conns {
		if held == rc {
			ch.conns = append(ch.conns[:i], ch.conns[i+1:]...)
			break
		}
	}
	r.mu.Unlock()
	_ = rc.conn.Close()
}

func (r *relay) handlePublish(w http.ResponseWriter, req *http.Request) {
	if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(wire.PublishErrorBody{Code: "not_permitted"})
		return
	}
	channelID := channelIDFromPath(req.URL.Path, "/messages")
	var body wire.PublishBody
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		return
	}
	content, _ := json.Marshal(body.Content)
	msgType := body.Type
	if msgType == "" {
		msgType = "message"
	}
	sub := subFromToken(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))

	r.mu.Lock()
	ch := r.channelState(channelID)
	ch.seq++
	seq := ch.seq
	msg := wire.Message{
		ID: "m_" + channelID + "_" + strconv.FormatInt(seq, 10), Seq: &seq, Type: msgType, Kind: "text",
		Content: wire.RawJSON(content), Sender: wire.Sender{ID: sub, Anon: true},
		Timestamp: time.Now().UnixMilli(),
	}
	ch.history = append(ch.history, msg)
	if len(ch.history) > relayHistoryCap {
		ch.history = ch.history[len(ch.history)-relayHistoryCap:]
	}
	conns := append([]*relayConn(nil), ch.conns...)
	r.mu.Unlock()

	frame := &wire.BatchFrame{Msgs: []wire.Message{msg}}
	for _, rc := range conns {
		rc.write(frame)
	}
	_ = json.NewEncoder(w).Encode(wire.SendAck{ID: msg.ID, Seq: seq, Timestamp: msg.Timestamp})
}

func (r *relay) handleHistory(w http.ResponseWriter, req *http.Request) {
	channelID := channelIDFromPath(req.URL.Path, "/history")
	q := req.URL.Query()
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
	limit, hasLimit := parse("limit")
	r.mu.Lock()
	source := r.channelState(channelID).history
	msgs := []wire.Message{}
	for _, msg := range source {
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
	if hasLimit && int64(len(msgs)) > limit {
		msgs = msgs[int64(len(msgs))-limit:]
	}
	r.mu.Unlock()
	_ = json.NewEncoder(w).Encode(wire.HistoryResponse{Msgs: msgs, HasMore: false})
}
