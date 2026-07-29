package main

import (
	"testing"
	"time"
)

func testGame(t *testing.T) *game {
	t.Helper()
	addr, err := startRelay("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	net := newNetClient(relayConfig(addr), "rules-test", "me")
	t.Cleanup(net.close)
	g := newGame(net, "me", "ME")
	g.me.name = "ME"
	g.me.alive = true
	g.me.x, g.me.y = 100, 100
	g.mode = modePlay
	return g
}

func TestKillAwardsTenAndDeathChangesLeaderboard(t *testing.T) {
	g := testGame(t)
	now := time.Now()

	// A remote tank appears via its state event.
	g.applyEvent(gameEvent{T: "state", ID: "rival", Name: "Rival", X: 200, Y: 100, Alive: true, Score: 30}, now)
	rival := g.others["rival"]
	if rival == nil || rival.score != 30 || !rival.alive {
		t.Fatalf("remote state not applied: %+v", rival)
	}

	// My hit settles: +10 for me, rival dies.
	g.applyHit("me", "rival", now)
	if g.me.score != 10 {
		t.Fatalf("shooter score = %d, want 10", g.me.score)
	}
	if rival.alive {
		t.Fatal("victim should be dead")
	}

	// Their hit on me: they score, I die and get a respawn timer.
	g.applyEvent(gameEvent{T: "hit", ID: "rival", Victim: "me"}, now)
	if rival.score != 40 {
		t.Fatalf("rival score = %d, want 40", rival.score)
	}
	if g.me.alive || g.respawnAt.IsZero() {
		t.Fatalf("local death not applied: alive=%v respawnAt=%v", g.me.alive, g.respawnAt)
	}
}

func TestBulletVictimDetection(t *testing.T) {
	g := testGame(t)
	now := time.Now()
	g.applyEvent(gameEvent{T: "state", ID: "rival", Name: "Rival", X: 130, Y: 100, Alive: true}, now)
	g.others["rival"].x = 130 // skip interpolation for the test

	// My bullet flying right from my muzzle, overlapping the rival's left edge.
	b := &bullet{id: "me#1", owner: "me", x: 128, y: 106, dir: 1}
	if v := g.bulletVictim(b); v != "rival" {
		t.Fatalf("victim = %q, want rival", v)
	}
	// A bullet never hits its own shooter.
	own := &bullet{id: "me#2", owner: "me", x: g.me.x + 4, y: g.me.y + 4, dir: 1}
	if v := g.bulletVictim(own); v != "" {
		t.Fatalf("own bullet hit self: %q", v)
	}
	// Dead tanks are not targets.
	g.others["rival"].alive = false
	if v := g.bulletVictim(b); v != "" {
		t.Fatalf("dead tank was hit: %q", v)
	}
}

func TestRemoteTimeout(t *testing.T) {
	g := testGame(t)
	past := time.Now().Add(-10 * time.Second)
	g.applyEvent(gameEvent{T: "state", ID: "ghost", Name: "Ghost", Alive: true}, past)
	g.updatePlay()
	if _, still := g.others["ghost"]; still {
		t.Fatal("silent peer was not dropped after the timeout")
	}
}
