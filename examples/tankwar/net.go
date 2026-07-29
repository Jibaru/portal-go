package main

import (
	"context"
	"log"
	"time"

	portal "github.com/Jibaru/portal-go"
)

// gameEvent is the single message shape the game speaks over Portal. T
// discriminates; unused fields are zero.
type gameEvent struct {
	T string `json:"t"` // "state" | "shoot" | "hit"
	// ID is the sender's player id (not the transport identity).
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`

	// state
	X      float64 `json:"x,omitempty"`
	Y      float64 `json:"y,omitempty"`
	Dir    int     `json:"dir"`
	Moving bool    `json:"moving,omitempty"`
	Alive  bool    `json:"alive,omitempty"`
	Score  int     `json:"score"`

	// shoot
	BulletID string `json:"bulletId,omitempty"`

	// hit — announced by the VICTIM (victim-authoritative): you only die if
	// the bullet touched you on your own screen, so dodges always count.
	Victim  string `json:"victim,omitempty"`
	Shooter string `json:"shooter,omitempty"`
}

// netClient wraps a portal-go channel for the game loop: sends are queued to a
// goroutine (Send blocks on the HTTP round-trip), receives are decoded onto a
// buffered channel the game drains once per tick.
type netClient struct {
	channel *portal.Channel
	events  chan gameEvent
	outbox  chan gameEvent
	selfID  string
	// reliableState routes state updates over persistent publishes instead of
	// the ephemeral lane — for backends whose ephemeral fan-out is unknown
	// (the hosted service); the relay always fans ephemeral out.
	reliableState bool
}

func newNetClient(config portal.Config, channelID, selfID string, reliableState bool) *netClient {
	client := portal.New(config)
	n := &netClient{
		// Live-only: a joining tank learns the field from heartbeats within a
		// second; replaying old history would ghost-drive dead matches.
		channel:       client.Channel(channelID, portal.WithHistoryNone()),
		events:        make(chan gameEvent, 512),
		outbox:        make(chan gameEvent, 128),
		selfID:        selfID,
		reliableState: reliableState,
	}
	receive := func(m portal.Message) {
		var ev gameEvent
		if err := m.DecodeContent(&ev); err != nil || ev.T == "" || ev.ID == "" {
			return
		}
		// Own events were applied locally at send time; the echo is skipped.
		if ev.ID == n.selfID {
			return
		}
		select {
		case n.events <- ev:
		default: // never stall the SDK's dispatch goroutine
		}
	}
	n.channel.OnMessage(receive)
	n.channel.OnEphemeral(receive)
	n.channel.Acquire()
	go n.sendLoop()
	return n
}

func (n *netClient) status() portal.ChannelStatus { return n.channel.Status() }

func (n *netClient) onStatus(fn func(portal.ChannelStatus, error)) {
	n.channel.OnStatus(fn)
}

// send queues a reliable event (shoot, hit — order matters, delivery matters);
// drops rather than blocking the game loop.
func (n *netClient) send(ev gameEvent) {
	ev.ID = n.selfID
	select {
	case n.outbox <- ev:
	default:
	}
}

// sendState publishes a state update on the ephemeral lane: a fire-and-forget
// WebSocket frame with no HTTP round-trip and no ack to wait behind, so a slow
// link can never queue movement updates into lag. Loss is fine — the next
// update supersedes it.
func (n *netClient) sendState(ev gameEvent) {
	ev.ID = n.selfID
	if n.reliableState {
		n.send(ev)
		return
	}
	_, _ = n.channel.Send(context.Background(), portal.SendInput{Ephemeral: true, Content: ev})
}

func (n *netClient) sendLoop() {
	for ev := range n.outbox {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := n.channel.Send(ctx, portal.SendInput{Content: ev})
		cancel()
		if err != nil {
			log.Printf("send %s failed: %v", ev.T, err)
		}
	}
}

func (n *netClient) close() {
	close(n.outbox)
	n.channel.Release()
}
