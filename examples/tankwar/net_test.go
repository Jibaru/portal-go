package main

import (
	"testing"
	"time"

	portal "github.com/Jibaru/portal-go"
)

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
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

// TestRelayRoundTrip drives two real portal-go clients through the relay:
// player A's state and hit events must arrive at player B, and vice versa —
// the exact path the game uses.
func TestRelayRoundTrip(t *testing.T) {
	addr, err := startRelay("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config := relayConfig(addr)

	a := newNetClient(config, "test-field", "pA")
	defer a.close()
	b := newNetClient(config, "test-field", "pB")
	defer b.close()

	waitUntil(t, 5*time.Second, "both clients ready", func() bool {
		return a.status() == portal.StatusReady && b.status() == portal.StatusReady
	})

	// A announces a state; B must see it (and never its own echoes).
	a.send(gameEvent{T: "state", Name: "Alice", X: 100, Y: 120, Dir: 1, Alive: true, Score: 0})
	var got gameEvent
	waitUntil(t, 5*time.Second, "state at B", func() bool {
		select {
		case got = <-b.events:
			return got.T == "state" && got.ID == "pA"
		default:
			return false
		}
	})
	if got.Name != "Alice" || got.X != 100 || got.Dir != 1 || !got.Alive {
		t.Fatalf("state mangled in transit: %+v", got)
	}

	// B shoots and claims a hit on A; A must receive both, in order.
	b.send(gameEvent{T: "shoot", BulletID: "pB#1", X: 50, Y: 60, Dir: 2})
	b.send(gameEvent{T: "hit", Victim: "pA", BulletID: "pB#1"})
	var seen []string
	waitUntil(t, 5*time.Second, "shoot+hit at A", func() bool {
		select {
		case ev := <-a.events:
			if ev.ID == "pB" {
				seen = append(seen, ev.T)
			}
		default:
		}
		return len(seen) >= 2
	})
	if seen[0] != "shoot" || seen[1] != "hit" {
		t.Fatalf("events out of order: %v", seen)
	}

	// A's own events never come back to A as remote events.
	select {
	case ev := <-a.events:
		if ev.ID == "pA" {
			t.Fatalf("client received its own echo: %+v", ev)
		}
	default:
	}
}
