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
	net := newNetClient(relayConfig(addr), "rules-test", "me", false)
	t.Cleanup(net.close)
	g := newGame(net, "me", "ME")
	g.me.name = "ME"
	g.me.alive = true
	g.me.x, g.me.y = 48, 48 // an open tile, clear of the brick pillars
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

	// The rival announces its own death by my bullet (victim-authoritative):
	// +10 for me, rival dies.
	g.applyEvent(gameEvent{T: "hit", ID: "rival", Shooter: "me", Victim: "rival"}, now)
	if g.me.score != 10 {
		t.Fatalf("shooter score = %d, want 10", g.me.score)
	}
	if rival.alive {
		t.Fatal("victim should be dead")
	}

	// Their bullet touches my true position: I die, they score.
	g.protectedUntil = time.Time{}
	b := &bullet{id: "rival#1", owner: "rival", x: g.me.x + 4, y: g.me.y + 4, dir: 1}
	if !g.bulletHitsMe(b, now) {
		t.Fatal("overlapping remote bullet should hit me")
	}
	g.applyHit("rival", "me", now)
	if rival.score != 40 {
		t.Fatalf("rival score = %d, want 40", rival.score)
	}
	if g.me.alive || g.respawnAt.IsZero() {
		t.Fatalf("local death not applied: alive=%v respawnAt=%v", g.me.alive, g.respawnAt)
	}
}

func TestBulletHitsMe(t *testing.T) {
	g := testGame(t)
	now := time.Now()
	g.protectedUntil = time.Time{}

	// A remote bullet overlapping my true position hits me.
	b := &bullet{id: "rival#1", owner: "rival", x: g.me.x + 4, y: g.me.y + 4, dir: 1}
	if !g.bulletHitsMe(b, now) {
		t.Fatal("overlapping remote bullet should hit")
	}
	// My own bullet never hits me.
	own := &bullet{id: "me#1", owner: "me", x: g.me.x + 4, y: g.me.y + 4, dir: 1}
	if g.bulletHitsMe(own, now) {
		t.Fatal("own bullet hit self")
	}
	// A bullet that missed my true position is a dodge — no hit, ever.
	miss := &bullet{id: "rival#2", owner: "rival", x: g.me.x + 40, y: g.me.y, dir: 1}
	if g.bulletHitsMe(miss, now) {
		t.Fatal("missing bullet reported a hit")
	}
	// Spawn protection: freshly spawned tanks cannot be hit.
	g.protectedUntil = now.Add(time.Second)
	if g.bulletHitsMe(b, now) {
		t.Fatal("protected tank was hit")
	}
	// Dead tanks are not targets.
	g.protectedUntil = time.Time{}
	g.me.alive = false
	if g.bulletHitsMe(b, now) {
		t.Fatal("dead tank was hit")
	}
}

func TestVictimAnnouncesOwnDeath(t *testing.T) {
	g := testGame(t)
	now := time.Now()
	g.protectedUntil = time.Time{}
	g.applyEvent(gameEvent{T: "state", ID: "rival", Name: "Rival", X: 300, Y: 300, Alive: true}, now)

	// A remote bullet crosses my tank during updateBullets: I die locally and
	// the shooter is credited — the full victim-authoritative path.
	g.bullets = append(g.bullets, &bullet{id: "rival#9", owner: "rival", x: g.me.x - 2, y: g.me.y + 6, dir: 1})
	g.updateBullets(now)
	if g.me.alive {
		t.Fatal("bullet crossing my tank should kill me")
	}
	if g.others["rival"].score != 10 {
		t.Fatalf("shooter score = %d, want 10", g.others["rival"].score)
	}
	if len(g.bullets) != 0 {
		t.Fatalf("settled bullet still simulated: %d", len(g.bullets))
	}
}

func TestSnapshotInterpolation(t *testing.T) {
	g := testGame(t)
	now := time.Now()

	// Two samples 100ms apart; the render time (now - interpDelay) falls
	// exactly halfway between them → position lerps halfway.
	o := &tank{id: "r", alive: true}
	o.samples = []stateSample{
		{at: now.Add(-150 * time.Millisecond), x: 100, y: 100, dir: 1, moving: true},
		{at: now.Add(-50 * time.Millisecond), x: 132, y: 100, dir: 1, moving: true},
	}
	g.others["r"] = o
	g.interpolateRemotes(now)
	if o.x < 115 || o.x > 117 || o.y != 100 {
		t.Fatalf("lerp position = (%v,%v), want (~116,100)", o.x, o.y)
	}

	// A respawn jump (> teleportDist) must snap, never glide.
	o.samples = []stateSample{
		{at: now.Add(-150 * time.Millisecond), x: 100, y: 100, dir: 1},
		{at: now.Add(-50 * time.Millisecond), x: 400, y: 300, dir: 1},
	}
	g.interpolateRemotes(now)
	if !(o.x == 100 && o.y == 100) && !(o.x == 400 && o.y == 300) {
		t.Fatalf("teleport glided to (%v,%v)", o.x, o.y)
	}

	// Buffer dry + moving: extrapolates along the heading, capped.
	o.samples = []stateSample{{at: now.Add(-400 * time.Millisecond), x: 200, y: 200, dir: 1, moving: true}}
	g.interpolateRemotes(now)
	maxAhead := 200.0 + tankSpeed*60*maxExtrapTime.Seconds() + 0.001
	if o.x <= 200 || o.x > maxAhead {
		t.Fatalf("extrapolation x = %v, want in (200, %v]", o.x, maxAhead)
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
