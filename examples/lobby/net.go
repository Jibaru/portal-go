package main

import (
	"context"
	"crypto/rand"
	"log"
	"time"

	portal "github.com/Jibaru/portal-go"
)

// netEvent is the single message shape all lobby traffic speaks.
type netEvent struct {
	T    string `json:"t"` // "state" | "chat" | "leave" | "lobby"
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`

	// state (ephemeral lane)
	Skin   int     `json:"skin"`
	X      float64 `json:"x,omitempty"`
	Y      float64 `json:"y,omitempty"`
	Dir    int     `json:"dir"`
	Moving bool    `json:"moving,omitempty"`

	// chat (reliable). To == "" means the general room; otherwise it is the
	// recipient player id (a DM). The relay fans everything out; clients
	// filter DMs to the two participants.
	Text string `json:"text,omitempty"`
	To   string `json:"to,omitempty"`

	// lobby directory heartbeat (ephemeral)
	Code      string `json:"code,omitempty"`
	LobbyName string `json:"lobbyName,omitempty"`
	Count     int    `json:"count,omitempty"`

	// Wrapper-injected (not on the wire): the platform message id for chat
	// dedup between history backfill and live delivery.
	msgID string
}

// channelNet wraps one portal-go channel: reliable sends are queued to a
// goroutine, ephemeral sends fire straight over the socket, receives land on a
// buffered channel the scene drains each tick.
type channelNet struct {
	channel *portal.Channel
	events  chan netEvent
	outbox  chan netEvent
	selfID  string
	closed  chan struct{}
}

func joinChannel(client *portal.Client, channelID, selfID string, history int) *channelNet {
	opts := []portal.ChannelOption{portal.WithHistoryNone()}
	if history > 0 {
		opts = []portal.ChannelOption{portal.WithHistory(history)}
	}
	n := &channelNet{
		channel: client.Channel(channelID, opts...),
		events:  make(chan netEvent, 512),
		outbox:  make(chan netEvent, 128),
		selfID:  selfID,
		closed:  make(chan struct{}),
	}
	receive := func(m portal.Message) {
		var ev netEvent
		if err := m.DecodeContent(&ev); err != nil || ev.T == "" || ev.ID == "" {
			return
		}
		if ev.ID == n.selfID {
			return // own echo; applied locally at send time
		}
		ev.msgID = m.ID
		select {
		case n.events <- ev:
		default:
		}
	}
	n.channel.OnMessage(receive)
	n.channel.OnEphemeral(receive)
	n.channel.Acquire()
	go n.sendLoop()
	return n
}

func (n *channelNet) status() portal.ChannelStatus { return n.channel.Status() }

// sendReliable queues a persistent publish (chat, leave — must not be lost).
func (n *channelNet) sendReliable(ev netEvent) {
	ev.ID = n.selfID
	select {
	case n.outbox <- ev:
	default:
	}
}

// sendEphemeral fires a state/heartbeat over the fast lane; the next update
// supersedes a lost one.
func (n *channelNet) sendEphemeral(ev netEvent) {
	ev.ID = n.selfID
	_, _ = n.channel.Send(context.Background(), portal.SendInput{Ephemeral: true, Content: ev})
}

// history decodes the channel's backfilled window — how a late joiner reads
// the chat that happened before they arrived. Callers dedupe against live
// events by msgID.
func (n *channelNet) history() []netEvent {
	msgs := n.channel.Messages()
	out := make([]netEvent, 0, len(msgs))
	for _, m := range msgs {
		var ev netEvent
		if err := m.DecodeContent(&ev); err != nil || ev.T == "" || ev.ID == "" {
			continue
		}
		ev.msgID = m.ID
		out = append(out, ev)
	}
	return out
}

func (n *channelNet) sendLoop() {
	for {
		select {
		case ev := <-n.outbox:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := n.channel.Send(ctx, portal.SendInput{Content: ev})
			cancel()
			if err != nil {
				log.Printf("send %s failed: %v", ev.T, err)
			}
		case <-n.closed:
			return
		}
	}
}

// close flushes nothing further and releases the channel (the SDK's grace
// period keeps the socket briefly for an immediate rejoin).
func (n *channelNet) close() {
	close(n.closed)
	n.channel.Release()
}

// newPlayerID returns a random player identity for this session.
func newPlayerID() string {
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 8)
	for i, b := range raw {
		out[i*2] = hexdigits[b>>4]
		out[i*2+1] = hexdigits[b&0xf]
	}
	return "p_" + string(out)
}

// Lobby codes: 6 chars, unambiguous alphanumerics.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func newLobbyCode() string {
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	out := make([]byte, 6)
	for i, b := range raw {
		out[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	return string(out)
}

// isLobbyCode reports whether s looks like a 6-char lobby code.
func isLobbyCode(s string) bool {
	if len(s) != 6 {
		return false
	}
	for _, r := range s {
		found := false
		for _, a := range codeAlphabet {
			if r == a {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func lobbyChannelID(code string) string { return "lobby-room-" + code }

const directoryChannel = "lobby-directory"
