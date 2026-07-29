package main

import (
	"fmt"
	"image/color"
	"math"
	"sort"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
	"github.com/hajimehoshi/ebiten/v2/text/v2"
	"github.com/hajimehoshi/ebiten/v2/vector"

	portal "github.com/Jibaru/portal-go"
)

const (
	tankSpeed     = 1.6 // px per tick @60tps
	bulletSpeed   = 4.5
	shootCooldown = 400 * time.Millisecond
	maxOwnBullets = 2
	respawnDelay  = 3 * time.Second
	peerTimeout   = 5 * time.Second
	stateInterval = 50 * time.Millisecond
	heartbeat     = time.Second

	// Snapshot interpolation: remote tanks render this far in the past, so
	// their motion is a smooth lerp between two REAL samples instead of a
	// guess ahead of one. Two update intervals of delay absorbs network
	// jitter completely; 100ms of visual lag is imperceptible here.
	interpDelay = 100 * time.Millisecond
	// If the sample buffer runs dry (a late packet), extrapolate along the
	// heading at most this long, then freeze.
	maxExtrapTime = 200 * time.Millisecond
	// Two samples further apart than this are a teleport (spawn), not motion.
	teleportDist = 64.0
	// spawnProtection makes a freshly spawned tank briefly unhittable (drawn
	// blinking), so a respawn can never be an instant death.
	spawnProtection = 1500 * time.Millisecond
)

type tank struct {
	id     string
	name   string
	x, y   float64
	dir    int
	moving bool
	alive  bool
	score  int

	// Remote-only: the timed position samples the tank is rendered from.
	samples  []stateSample
	lastSeen time.Time
}

// stateSample is one received remote state, stamped with its arrival time.
type stateSample struct {
	at     time.Time
	x, y   float64
	dir    int
	moving bool
}

type bullet struct {
	id    string
	owner string
	x, y  float64
	dir   int
}

type explosion struct {
	x, y  float64
	start time.Time
}

type gameMode int

const (
	modeName gameMode = iota
	modePlay
)

type game struct {
	mode      gameMode
	nameInput string

	world *world
	net   *netClient
	me    *tank
	// others is keyed by player id; every field of a remote tank is driven by
	// its owner's state/shoot/hit events.
	others     map[string]*tank
	bullets    []*bullet
	explosions []explosion

	bulletSeq      int
	cooldownUntil  time.Time
	respawnAt      time.Time
	protectedUntil time.Time
	lastState      time.Time
	lastBeat       time.Time
	sentX, sentY   float64
	sentDir        int
	sentMoving     bool
	sentAlive      bool

	status portal.ChannelStatus
	ticks  int

	// smoke drives a scripted, windowless-ish run for CI-style verification:
	// auto-join, drive in circles, shoot, and terminate cleanly.
	smoke      bool
	smokeUntil int
}

func newGame(net *netClient, selfID, presetName string) *game {
	g := &game{
		world:  buildWorld(),
		net:    net,
		others: map[string]*tank{},
		me:     &tank{id: selfID, alive: false},
		status: net.status(),
	}
	net.onStatus(func(s portal.ChannelStatus, err error) { g.status = s })
	if presetName != "" {
		g.nameInput = presetName
	}
	return g
}

func (g *game) Layout(int, int) (int, int) { return screenW, screenH }

// ── Update ────────────────────────────────────────────────

func (g *game) Update() error {
	g.ticks++
	g.drainNetwork()
	if g.smoke {
		if g.mode == modeName && g.ticks > 10 {
			g.me.name = g.nameInput
			g.spawnSelf()
			g.mode = modePlay
		}
		if g.ticks >= g.smokeUntil {
			return ebiten.Termination
		}
	}
	switch g.mode {
	case modeName:
		g.updateNameEntry()
	case modePlay:
		g.updatePlay()
	}
	return nil
}

func (g *game) updateNameEntry() {
	for _, r := range ebiten.AppendInputChars(nil) {
		if len(g.nameInput) < 12 && r >= 32 && r != 127 {
			g.nameInput += string(r)
		}
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyBackspace) && g.nameInput != "" {
		g.nameInput = g.nameInput[:len(g.nameInput)-1]
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyEnter) && g.nameInput != "" {
		g.me.name = g.nameInput
		g.spawnSelf()
		g.mode = modePlay
	}
}

func (g *game) spawnSelf() {
	occupied := make([][2]float64, 0, len(g.others))
	for _, o := range g.others {
		occupied = append(occupied, [2]float64{o.x, o.y})
	}
	g.me.x, g.me.y = g.world.randomSpawn(occupied)
	g.me.alive = true
	g.me.dir = 0
	g.protectedUntil = time.Now().Add(spawnProtection)
	g.publishState(true)
}

func (g *game) updatePlay() {
	now := time.Now()

	if g.me.alive {
		g.updateInput(now)
	} else if !g.respawnAt.IsZero() && now.After(g.respawnAt) {
		g.respawnAt = time.Time{}
		g.spawnSelf()
	}

	g.updateBullets(now)
	g.interpolateRemotes(now)

	// Drop peers that stopped heartbeating (closed window, lost link).
	for id, o := range g.others {
		if now.Sub(o.lastSeen) > peerTimeout {
			delete(g.others, id)
		}
	}

	// Publish own state on change (rate-limited) and as a steady heartbeat so
	// late joiners discover us and timeouts don't fire.
	changed := math.Abs(g.me.x-g.sentX) > 0.5 || math.Abs(g.me.y-g.sentY) > 0.5 ||
		g.me.dir != g.sentDir || g.me.moving != g.sentMoving || g.me.alive != g.sentAlive
	if (changed && now.Sub(g.lastState) >= stateInterval) || now.Sub(g.lastBeat) >= heartbeat {
		g.publishState(false)
	}

	// Expire finished explosion animations.
	live := g.explosions[:0]
	for _, e := range g.explosions {
		if now.Sub(e.start) < 600*time.Millisecond {
			live = append(live, e)
		}
	}
	g.explosions = live
}

func (g *game) updateInput(now time.Time) {
	if g.smoke {
		g.me.dir = (g.ticks / 45) % 4
		dx, dy := dirVector(g.me.dir)
		g.me.moving = true
		if nx := g.me.x + dx*tankSpeed; !g.world.boxCollides(nx+1, g.me.y+1, tankSize-2) {
			g.me.x = nx
		}
		if ny := g.me.y + dy*tankSpeed; !g.world.boxCollides(g.me.x+1, ny+1, tankSize-2) {
			g.me.y = ny
		}
		if now.After(g.cooldownUntil) && g.ownBullets() < maxOwnBullets {
			g.cooldownUntil = now.Add(shootCooldown)
			g.shoot()
		}
		return
	}
	dx, dy := 0.0, 0.0
	moving := true
	switch {
	case ebiten.IsKeyPressed(ebiten.KeyArrowUp) || ebiten.IsKeyPressed(ebiten.KeyW):
		g.me.dir, dx, dy = 0, 0, -tankSpeed
	case ebiten.IsKeyPressed(ebiten.KeyArrowRight) || ebiten.IsKeyPressed(ebiten.KeyD):
		g.me.dir, dx, dy = 1, tankSpeed, 0
	case ebiten.IsKeyPressed(ebiten.KeyArrowDown) || ebiten.IsKeyPressed(ebiten.KeyS):
		g.me.dir, dx, dy = 2, 0, tankSpeed
	case ebiten.IsKeyPressed(ebiten.KeyArrowLeft) || ebiten.IsKeyPressed(ebiten.KeyA):
		g.me.dir, dx, dy = 3, -tankSpeed, 0
	default:
		moving = false
	}
	g.me.moving = moving
	if moving {
		// Axis-at-a-time movement with wall collision, Battle City feel.
		if nx := g.me.x + dx; !g.world.boxCollides(nx+1, g.me.y+1, tankSize-2) && !g.collidesTank(nx, g.me.y) {
			g.me.x = nx
		}
		if ny := g.me.y + dy; !g.world.boxCollides(g.me.x+1, ny+1, tankSize-2) && !g.collidesTank(g.me.x, ny) {
			g.me.y = ny
		}
	}

	if ebiten.IsKeyPressed(ebiten.KeySpace) && now.After(g.cooldownUntil) && g.ownBullets() < maxOwnBullets {
		g.cooldownUntil = now.Add(shootCooldown)
		g.shoot()
	}
}

func (g *game) collidesTank(x, y float64) bool {
	for _, o := range g.others {
		if !o.alive {
			continue
		}
		if math.Abs(x-o.x) < tankSize-2 && math.Abs(y-o.y) < tankSize-2 {
			return true
		}
	}
	return false
}

func (g *game) ownBullets() int {
	n := 0
	for _, b := range g.bullets {
		if b.owner == g.me.id {
			n++
		}
	}
	return n
}

func (g *game) shoot() {
	g.bulletSeq++
	vx, vy := dirVector(g.me.dir)
	cx, cy := g.me.x+tankSize/2, g.me.y+tankSize/2
	b := &bullet{
		id:    fmt.Sprintf("%s#%d", g.me.id, g.bulletSeq),
		owner: g.me.id,
		x:     cx + vx*10 - bulletSize/2,
		y:     cy + vy*10 - bulletSize/2,
		dir:   g.me.dir,
	}
	g.bullets = append(g.bullets, b)
	g.net.send(gameEvent{T: "shoot", BulletID: b.id, X: b.x, Y: b.y, Dir: b.dir})
}

func (g *game) updateBullets(now time.Time) {
	kept := g.bullets[:0]
	for _, b := range g.bullets {
		vx, vy := dirVector(b.dir)
		b.x += vx * bulletSpeed
		b.y += vy * bulletSpeed

		if g.world.boxCollides(b.x, b.y, bulletSize) {
			g.explosions = append(g.explosions, explosion{x: b.x - 6, y: b.y - 6, start: now})
			continue
		}

		// Victim-authoritative kills: each client judges ONLY hits on its own
		// tank, against its true position — if the bullet misses you on your
		// screen, you dodged, no matter what the shooter saw against your
		// delayed ghost. Bullets fly through remote tanks; the victim's `hit`
		// event is what settles a kill (and removes the bullet everywhere).
		if g.bulletHitsMe(b, now) {
			g.net.send(gameEvent{T: "hit", Shooter: b.owner, Victim: g.me.id, BulletID: b.id})
			g.applyHit(b.owner, g.me.id, now)
			continue
		}
		kept = append(kept, b)
	}
	g.bullets = kept
}

// bulletHitsMe reports whether a remote bullet overlaps my own (true) tank.
func (g *game) bulletHitsMe(b *bullet, now time.Time) bool {
	if b.owner == g.me.id || !g.me.alive || g.mode != modePlay {
		return false
	}
	if now.Before(g.protectedUntil) {
		return false
	}
	return b.x < g.me.x+tankSize && b.x+bulletSize > g.me.x &&
		b.y < g.me.y+tankSize && b.y+bulletSize > g.me.y
}

// applyHit settles a kill: +10 to the shooter, death (and later respawn) for
// the victim. Runs on every client from the same event, so scores converge.
func (g *game) applyHit(shooter, victim string, now time.Time) {
	if shooter == g.me.id {
		g.me.score += 10
	} else if s, ok := g.others[shooter]; ok {
		s.score += 10
	}
	switch victim {
	case g.me.id:
		if g.me.alive {
			g.me.alive = false
			g.explosions = append(g.explosions, explosion{x: g.me.x, y: g.me.y, start: now})
			g.respawnAt = now.Add(respawnDelay)
			g.publishState(true)
		}
	default:
		if v, ok := g.others[victim]; ok && v.alive {
			v.alive = false
			g.explosions = append(g.explosions, explosion{x: v.x, y: v.y, start: now})
		}
	}
}

func (g *game) publishState(force bool) {
	now := time.Now()
	g.lastState, g.lastBeat = now, now
	g.sentX, g.sentY, g.sentDir, g.sentMoving, g.sentAlive = g.me.x, g.me.y, g.me.dir, g.me.moving, g.me.alive
	ev := gameEvent{
		T: "state", Name: g.me.name,
		X: g.me.x, Y: g.me.y, Dir: g.me.dir,
		Moving: g.me.moving, Alive: g.me.alive, Score: g.me.score,
	}
	// Life transitions (spawn, death) must not be lost; movement rides the
	// fast lane where the next update supersedes a dropped one.
	if force {
		g.net.send(ev)
		return
	}
	g.net.sendState(ev)
}

func (g *game) drainNetwork() {
	now := time.Now()
	for {
		select {
		case ev := <-g.net.events:
			g.applyEvent(ev, now)
		default:
			return
		}
	}
}

func (g *game) applyEvent(ev gameEvent, now time.Time) {
	switch ev.T {
	case "state":
		o, ok := g.others[ev.ID]
		if !ok {
			o = &tank{id: ev.ID, x: ev.X, y: ev.Y, dir: ev.Dir}
			g.others[ev.ID] = o
		}
		o.name = ev.Name
		o.moving = ev.Moving
		o.alive = ev.Alive
		o.score = ev.Score // owner-reported; converges with applyHit
		o.lastSeen = now
		o.samples = append(o.samples, stateSample{at: now, x: ev.X, y: ev.Y, dir: ev.Dir, moving: ev.Moving})
		if len(o.samples) > 16 {
			o.samples = o.samples[len(o.samples)-16:]
		}
	case "shoot":
		g.bullets = append(g.bullets, &bullet{id: ev.BulletID, owner: ev.ID, x: ev.X, y: ev.Y, dir: ev.Dir})
	case "hit":
		// Remove the settled bullet if we still simulate it.
		for i, b := range g.bullets {
			if b.id == ev.BulletID {
				g.bullets = append(g.bullets[:i], g.bullets[i+1:]...)
				break
			}
		}
		shooter := ev.Shooter
		if shooter == "" {
			shooter = ev.ID // pre-victim-authoritative clients
		}
		g.applyHit(shooter, ev.Victim, now)
	}
}

// interpolateRemotes renders each remote tank interpDelay in the past, lerping
// between the two received samples that straddle that moment. Motion is then
// exactly as smooth as the sender's, regardless of network jitter — no
// rubber-banding, because the render never runs ahead of real data (except a
// short capped extrapolation when the buffer runs dry).
func (g *game) interpolateRemotes(now time.Time) {
	renderT := now.Add(-interpDelay)
	for _, o := range g.others {
		if len(o.samples) == 0 {
			continue
		}
		// Drop samples older than the straddling pair.
		idx := 0
		for idx < len(o.samples)-1 && !o.samples[idx+1].at.After(renderT) {
			idx++
		}
		if idx > 0 {
			o.samples = o.samples[idx:]
		}
		first := o.samples[0]

		// Not enough history yet: sit on the first sample.
		if renderT.Before(first.at) {
			o.x, o.y, o.dir = first.x, first.y, first.dir
			continue
		}

		// Newest sample is already older than the render time: extrapolate
		// along the heading briefly, then freeze until fresh data arrives.
		if len(o.samples) == 1 {
			last := first
			o.dir = last.dir
			o.x, o.y = last.x, last.y
			if last.moving {
				dt := renderT.Sub(last.at)
				if dt > maxExtrapTime {
					dt = maxExtrapTime
				}
				vx, vy := dirVector(last.dir)
				ticks := dt.Seconds() * 60
				nx, ny := last.x+vx*tankSpeed*ticks, last.y+vy*tankSpeed*ticks
				if !g.world.boxCollides(nx+1, ny+1, tankSize-2) {
					o.x, o.y = nx, ny
				}
			}
			continue
		}

		// The straddle: samples[0].at <= renderT < samples[1].at.
		next := o.samples[1]
		o.dir = next.dir
		if math.Hypot(next.x-first.x, next.y-first.y) > teleportDist {
			// A respawn jump — never glide across the map.
			if renderT.Sub(first.at) > next.at.Sub(renderT) {
				o.x, o.y = next.x, next.y
			} else {
				o.x, o.y = first.x, first.y
			}
			continue
		}
		span := next.at.Sub(first.at).Seconds()
		f := 0.0
		if span > 0 {
			f = renderT.Sub(first.at).Seconds() / span
		}
		o.x = first.x + (next.x-first.x)*f
		o.y = first.y + (next.y-first.y)*f
	}
}

// ── Draw ──────────────────────────────────────────────────

func (g *game) Draw(screen *ebiten.Image) {
	screen.Fill(color.RGBA{16, 16, 16, 255})
	g.drawWorld(screen)
	if g.mode == modeName {
		g.drawNameEntry(screen)
		return
	}
	for _, o := range g.others {
		g.drawTank(screen, o, false)
	}
	if g.me.alive {
		g.drawTank(screen, g.me, true)
	}
	g.drawBullets(screen)
	g.drawExplosions(screen)
	g.drawLeaderboard(screen)
	g.drawHUD(screen)
}

func (g *game) drawWorld(screen *ebiten.Image) {
	for y := 0; y < tilesY; y++ {
		for x := 0; x < tilesX; x++ {
			var sprite *ebiten.Image
			switch g.world.tiles[y][x] {
			case tileBrick:
				sprite = brickSprite
			case tileSteel:
				sprite = steelSprite
			default:
				continue
			}
			op := &ebiten.DrawImageOptions{}
			op.GeoM.Translate(float64(x*tileSize), float64(y*tileSize))
			screen.DrawImage(sprite, op)
		}
	}
}

func (g *game) drawTank(screen *ebiten.Image, t *tank, self bool) {
	if !t.alive {
		return
	}
	// Spawn protection blinks the own tank while it cannot be hit.
	if self && time.Now().Before(g.protectedUntil) && (g.ticks/5)%2 == 0 {
		return
	}
	op := &ebiten.DrawImageOptions{}
	op.GeoM.Translate(-tankSize/2, -tankSize/2)
	op.GeoM.Rotate(float64(t.dir) * math.Pi / 2)
	op.GeoM.Translate(t.x+tankSize/2, t.y+tankSize/2)
	if !self {
		tint := tintFor(t.id)
		op.ColorScale.Scale(tint[0], tint[1], tint[2], 1)
	}
	screen.DrawImage(tankSprite, op)

	label := t.name
	if label == "" {
		label = "?"
	}
	tw, _ := text.Measure(label, fontSmall, 0)
	top := &text.DrawOptions{}
	top.GeoM.Translate(t.x+tankSize/2-tw/2, t.y-11)
	if self {
		top.ColorScale.ScaleWithColor(color.RGBA{252, 228, 116, 255})
	} else {
		top.ColorScale.ScaleWithColor(color.RGBA{200, 200, 200, 255})
	}
	text.Draw(screen, label, fontSmall, top)
}

func (g *game) drawBullets(screen *ebiten.Image) {
	for _, b := range g.bullets {
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Translate(b.x, b.y)
		screen.DrawImage(bulletSprite, op)
	}
}

func (g *game) drawExplosions(screen *ebiten.Image) {
	now := time.Now()
	for _, e := range g.explosions {
		frame := 0
		if now.Sub(e.start) > 250*time.Millisecond {
			frame = 1
		}
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Translate(e.x, e.y)
		screen.DrawImage(explosionFrames[frame], op)
	}
}

func (g *game) drawLeaderboard(screen *ebiten.Image) {
	type row struct {
		name  string
		score int
		self  bool
	}
	rows := []row{{g.me.name, g.me.score, true}}
	for _, o := range g.others {
		rows = append(rows, row{o.name, o.score, false})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].score > rows[j].score })
	if len(rows) > 8 {
		rows = rows[:8]
	}

	w, x0, y0 := 176.0, float64(screenW)-192.0, 24.0
	h := 26 + float64(len(rows))*12
	vector.DrawFilledRect(screen, float32(x0-8), float32(y0-14), float32(w), float32(h), color.RGBA{0, 0, 0, 190}, false)

	head := &text.DrawOptions{}
	head.GeoM.Translate(x0, y0-6)
	head.ColorScale.ScaleWithColor(color.RGBA{252, 228, 116, 255})
	text.Draw(screen, "LEADERBOARD", fontSmall, head)

	for i, r := range rows {
		name := r.name
		if len(name) > 10 {
			name = name[:10]
		}
		line := fmt.Sprintf("%-10s %4d", name, r.score)
		op := &text.DrawOptions{}
		op.GeoM.Translate(x0, y0+10+float64(i)*12)
		if r.self {
			op.ColorScale.ScaleWithColor(color.RGBA{252, 228, 116, 255})
		} else {
			op.ColorScale.ScaleWithColor(color.White)
		}
		text.Draw(screen, line, fontSmall, op)
	}
}

func (g *game) drawHUD(screen *ebiten.Image) {
	status := &text.DrawOptions{}
	status.GeoM.Translate(8, 6)
	status.ColorScale.ScaleWithColor(color.RGBA{120, 200, 120, 255})
	text.Draw(screen, string(g.status), fontSmall, status)

	if !g.me.alive && g.mode == modePlay {
		remaining := time.Until(g.respawnAt)
		if remaining < 0 {
			remaining = 0
		}
		msg := fmt.Sprintf("DESTROYED! RESPAWN IN %d", int(remaining.Seconds())+1)
		tw, _ := text.Measure(msg, fontBig, 0)
		op := &text.DrawOptions{}
		op.GeoM.Translate(float64(screenW)/2-tw/2, float64(screenH)/2-8)
		op.ColorScale.ScaleWithColor(color.RGBA{216, 40, 0, 255})
		text.Draw(screen, msg, fontBig, op)
	}
}

func (g *game) drawNameEntry(screen *ebiten.Image) {
	vector.DrawFilledRect(screen, 0, 0, screenW, screenH, color.RGBA{0, 0, 0, 200}, false)

	center := func(s string, face *text.GoTextFace, y float64, clr color.Color) {
		tw, _ := text.Measure(s, face, 0)
		op := &text.DrawOptions{}
		op.GeoM.Translate(float64(screenW)/2-tw/2, y)
		op.ColorScale.ScaleWithColor(clr)
		text.Draw(screen, s, face, op)
	}
	center("TANK WAR", fontBig, 120, color.RGBA{252, 228, 116, 255})
	center("A PORTAL-GO DEMO", fontSmall, 150, color.RGBA{200, 76, 12, 255})
	center("TYPE YOUR NAME:", fontSmall, 210, color.White)

	name := g.nameInput
	if (g.ticks/30)%2 == 0 {
		name += "_"
	}
	center(name, fontBig, 240, color.White)
	center("PRESS ENTER TO JOIN", fontSmall, 300, color.RGBA{120, 200, 120, 255})
	center("ARROWS/WASD MOVE - SPACE SHOOT", fontSmall, 330, color.RGBA{150, 150, 150, 255})
	center(fmt.Sprintf("LINK: %s", g.status), fontSmall, 380, color.RGBA{120, 120, 200, 255})
}
