package main

import (
	"fmt"
	"image/color"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
	"github.com/hajimehoshi/ebiten/v2/text/v2"
)

const (
	walkSpeed     = 1.4 // px per tick @60tps
	stateInterval = 50 * time.Millisecond
	heartbeat     = time.Second
	dirHeartbeat  = 2 * time.Second
	peerTimeout   = 6 * time.Second

	// Snapshot interpolation (same technique as the tankwar example): remotes
	// render slightly in the past, lerping between real samples.
	interpDelay   = 100 * time.Millisecond
	maxExtrapTime = 200 * time.Millisecond
	teleportDist  = 64.0

	interactRange = 26.0
)

// person is a remote player in the room.
type person struct {
	id     string
	name   string
	skin   int
	x, y   float64
	dir    int
	moving bool

	samples  []stateSample
	lastSeen time.Time
}

type stateSample struct {
	at     time.Time
	x, y   float64
	dir    int
	moving bool
}

type lobbyScene struct {
	code      string
	lobbyName string
	net       *channelNet

	x, y   float64
	dir    int
	moving bool
	skin   int

	others map[string]*person
	chat   *chatModel

	lastState    time.Time
	lastBeat     time.Time
	lastDirBeat  time.Time
	sentX, sentY float64
	sentDir      int
	sentMoving   bool

	animTick int
}

func newLobbyScene(g *game, code, lobbyName string) *lobbyScene {
	s := &lobbyScene{
		code:      code,
		lobbyName: lobbyName,
		net:       joinChannel(g.client, lobbyChannelID(code), g.selfID, 50),
		others:    map[string]*person{},
		chat:      newChatModel(),
		skin:      skinFor(g.selfID),
	}
	s.x, s.y = g.world.randomSpawn()
	s.dir = 2
	s.chat.system("YOU JOINED " + lobbyName + " [" + code + "]")
	s.publishState(g, true)
	return s
}

// leave announces the departure and releases the channel.
func (s *lobbyScene) leave() {
	s.net.sendReliable(netEvent{T: "leave"})
	// Give the queued leave a moment to publish before release.
	time.AfterFunc(500*time.Millisecond, s.net.close)
}

// ── Update ────────────────────────────────────────────────

func (s *lobbyScene) update(g *game) {
	now := time.Now()
	s.animTick++
	s.drainEvents(g, now)
	if g.ticks%60 == 0 {
		s.seedHistory(g)
	}

	if g.smoke {
		s.updateSmokeInput(g, now)
	} else if s.chat.focused {
		s.updateChatInput(g)
	} else {
		s.updateWalkInput(g, now)
	}

	s.interpolateRemotes(g, now)

	// Timed-out peers left without saying goodbye.
	for id, o := range s.others {
		if now.Sub(o.lastSeen) > peerTimeout {
			s.chat.system(o.name + " LEFT (CONNECTION LOST)")
			s.chat.dropPeerTab(id)
			delete(s.others, id)
		}
	}

	// Movement on the fast lane + steady heartbeat.
	changed := math.Abs(s.x-s.sentX) > 0.5 || math.Abs(s.y-s.sentY) > 0.5 ||
		s.dir != s.sentDir || s.moving != s.sentMoving
	if (changed && now.Sub(s.lastState) >= stateInterval) || now.Sub(s.lastBeat) >= heartbeat {
		s.publishState(g, false)
	}

	// Every member advertises the lobby to the directory; the browser
	// dedupes by code, so the list stays alive as long as anyone is inside.
	if now.Sub(s.lastDirBeat) >= dirHeartbeat {
		s.lastDirBeat = now
		g.directory.sendEphemeral(netEvent{
			T: "lobby", Code: s.code, LobbyName: s.lobbyName, Count: len(s.others) + 1,
		})
	}
}

func (s *lobbyScene) drainEvents(g *game, now time.Time) {
	for {
		select {
		case ev := <-s.net.events:
			s.applyEvent(g, ev, now)
		default:
			return
		}
	}
}

func (s *lobbyScene) applyEvent(g *game, ev netEvent, now time.Time) {
	switch ev.T {
	case "state":
		o, ok := s.others[ev.ID]
		if !ok {
			o = &person{id: ev.ID, x: ev.X, y: ev.Y, dir: ev.Dir}
			s.others[ev.ID] = o
			name := ev.Name
			if name == "" {
				name = "SOMEONE"
			}
			s.chat.system(name + " JOINED")
		}
		o.name = ev.Name
		o.skin = ev.Skin
		o.lastSeen = now
		o.moving = ev.Moving
		o.samples = append(o.samples, stateSample{at: now, x: ev.X, y: ev.Y, dir: ev.Dir, moving: ev.Moving})
		if len(o.samples) > 16 {
			o.samples = o.samples[len(o.samples)-16:]
		}
		// Keep an open DM tab's label in sync with the peer's name.
		if t := s.chat.tab(ev.ID); t != nil && ev.Name != "" {
			t.label = ev.Name
		}
	case "chat":
		if s.chat.addIncoming(ev, g.selfID) {
			g.smokeStats.chatRecv++
		}
	case "leave":
		if o, ok := s.others[ev.ID]; ok {
			s.chat.system(o.name + " LEFT")
			s.chat.dropPeerTab(ev.ID)
			delete(s.others, ev.ID)
		}
	}
}

// seedHistory folds the channel's backfilled window into the chat — how a
// late joiner reads what was said before they arrived. Idempotent via msgID.
func (s *lobbyScene) seedHistory(g *game) {
	for _, ev := range s.net.history() {
		if ev.T == "chat" && ev.ID != g.selfID {
			s.chat.addIncoming(ev, g.selfID)
		}
	}
}

func (s *lobbyScene) updateWalkInput(g *game, now time.Time) {
	dx, dy := 0.0, 0.0
	moving := true
	switch {
	case ebiten.IsKeyPressed(ebiten.KeyArrowUp) || ebiten.IsKeyPressed(ebiten.KeyW):
		s.dir, dx, dy = 0, 0, -walkSpeed
	case ebiten.IsKeyPressed(ebiten.KeyArrowRight) || ebiten.IsKeyPressed(ebiten.KeyD):
		s.dir, dx, dy = 1, walkSpeed, 0
	case ebiten.IsKeyPressed(ebiten.KeyArrowDown) || ebiten.IsKeyPressed(ebiten.KeyS):
		s.dir, dx, dy = 2, 0, walkSpeed
	case ebiten.IsKeyPressed(ebiten.KeyArrowLeft) || ebiten.IsKeyPressed(ebiten.KeyA):
		s.dir, dx, dy = 3, -walkSpeed, 0
	default:
		moving = false
	}
	s.moving = moving
	if moving {
		if nx := s.x + dx; !g.world.boxCollides(nx, s.y) {
			s.x = nx
		}
		if ny := s.y + dy; !g.world.boxCollides(s.x, ny) {
			s.y = ny
		}
	}

	if inpututil.IsKeyJustPressed(ebiten.KeyE) {
		if o := s.nearestPerson(); o != nil {
			s.chat.ensureTab(o.id, o.name)
			s.chat.openTab(o.id)
			s.chat.focused = true
		}
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyEnter) {
		s.chat.focused = true
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyTab) {
		s.chat.nextTab()
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyEscape) {
		g.leaveLobby()
	}
}

func (s *lobbyScene) updateChatInput(g *game) {
	for _, r := range ebiten.AppendInputChars(nil) {
		if len(s.chat.input) < 48 && r >= 32 && r != 127 {
			s.chat.input += string(r)
		}
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyBackspace) && s.chat.input != "" {
		s.chat.input = s.chat.input[:len(s.chat.input)-1]
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyTab) {
		s.chat.nextTab()
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyEscape) {
		s.chat.focused = false
		s.chat.input = ""
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyEnter) {
		text := s.chat.input
		s.chat.input = ""
		if text == "" {
			s.chat.focused = false
			return
		}
		s.sendChat(g, text)
	}
}

func (s *lobbyScene) sendChat(g *game, text string) {
	tab := s.chat.activeTab()
	to := ""
	if tab.key != generalTab {
		to = tab.key
	}
	s.net.sendReliable(netEvent{T: "chat", Name: g.name, Text: text, To: to})
	s.chat.addOwn(tab.key, g.name, text)
}

func (s *lobbyScene) nearestPerson() *person {
	var best *person
	bestDist := interactRange
	for _, o := range s.others {
		d := math.Hypot(o.x-s.x, o.y-s.y)
		if d < bestDist {
			best, bestDist = o, d
		}
	}
	return best
}

func (s *lobbyScene) publishState(g *game, force bool) {
	now := time.Now()
	s.lastState, s.lastBeat = now, now
	s.sentX, s.sentY, s.sentDir, s.sentMoving = s.x, s.y, s.dir, s.moving
	ev := netEvent{
		T: "state", Name: g.name, Skin: s.skin,
		X: s.x, Y: s.y, Dir: s.dir, Moving: s.moving,
	}
	if force {
		s.net.sendReliable(ev) // the join announcement must not be lost
		return
	}
	s.net.sendEphemeral(ev)
}

func (s *lobbyScene) interpolateRemotes(g *game, now time.Time) {
	renderT := now.Add(-interpDelay)
	for _, o := range s.others {
		if len(o.samples) == 0 {
			continue
		}
		idx := 0
		for idx < len(o.samples)-1 && !o.samples[idx+1].at.After(renderT) {
			idx++
		}
		if idx > 0 {
			o.samples = o.samples[idx:]
		}
		first := o.samples[0]
		if renderT.Before(first.at) {
			o.x, o.y, o.dir = first.x, first.y, first.dir
			continue
		}
		if len(o.samples) == 1 {
			o.x, o.y, o.dir = first.x, first.y, first.dir
			o.moving = false
			if first.moving {
				dt := renderT.Sub(first.at)
				if dt > maxExtrapTime {
					dt = maxExtrapTime
				}
				vx, vy := dirVector(first.dir)
				ticks := dt.Seconds() * 60
				nx, ny := first.x+vx*walkSpeed*ticks, first.y+vy*walkSpeed*ticks
				if !g.world.boxCollides(nx, ny) {
					o.x, o.y = nx, ny
				}
			}
			continue
		}
		next := o.samples[1]
		o.dir = next.dir
		o.moving = next.moving
		if math.Hypot(next.x-first.x, next.y-first.y) > teleportDist {
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

// updateSmokeInput wanders and chats on a script (verification runs).
func (s *lobbyScene) updateSmokeInput(g *game, now time.Time) {
	if g.ticks%120 == 0 {
		s.dir = rand.Intn(4)
	}
	s.moving = true
	vx, vy := dirVector(s.dir)
	if nx, ny := s.x+vx*walkSpeed, s.y+vy*walkSpeed; !g.world.boxCollides(nx, ny) {
		s.x, s.y = nx, ny
	} else {
		s.dir = rand.Intn(4)
	}
	if g.ticks%90 == 0 {
		s.sendChat(g, fmt.Sprintf("hi from %s (tick %d)", g.name, g.ticks))
	}
}

// ── Draw ──────────────────────────────────────────────────

func (s *lobbyScene) draw(g *game, screen *ebiten.Image) {
	for y := 0; y < tilesY; y++ {
		for x := 0; x < tilesX; x++ {
			op := &ebiten.DrawImageOptions{}
			op.GeoM.Translate(float64(x*tileSize), float64(y*tileSize))
			screen.DrawImage(tileSprites[g.world.tiles[y][x]], op)
		}
	}

	// Painter's order: people sorted by feet position.
	type drawn struct {
		p    *person
		self bool
	}
	people := make([]drawn, 0, len(s.others)+1)
	for _, o := range s.others {
		people = append(people, drawn{p: o})
	}
	people = append(people, drawn{p: &person{name: g.name, skin: s.skin, x: s.x, y: s.y, dir: s.dir, moving: s.moving}, self: true})
	sort.SliceStable(people, func(i, j int) bool { return people[i].p.y < people[j].p.y })
	for _, d := range people {
		s.drawPerson(screen, d.p, d.self)
	}

	// Interaction hint.
	if !s.chat.focused {
		if o := s.nearestPerson(); o != nil {
			centerText(screen, "E: TALK TO "+o.name, fontSmall, 20, uiHighlight)
		}
	}

	s.drawTopBar(g, screen)
	s.drawChat(g, screen)
}

func (s *lobbyScene) drawPerson(screen *ebiten.Image, p *person, self bool) {
	set := charSets[p.skin%len(charSets)]
	frame := 0
	if p.moving && (s.animTick/8)%2 == 1 {
		frame = 1
	}
	img := set.frames[p.dir][frame]
	op := &ebiten.DrawImageOptions{}
	if p.dir == 1 { // right = mirrored left
		op.GeoM.Scale(-1, 1)
		op.GeoM.Translate(charW, 0)
	}
	op.GeoM.Translate(p.x, p.y-4)
	screen.DrawImage(img, op)

	label := p.name
	if label == "" {
		label = "?"
	}
	tw, _ := text.Measure(label, fontSmall, 0)
	clr := color.RGBA{255, 255, 255, 255}
	if self {
		clr = uiHighlight
	}
	top := &text.DrawOptions{}
	top.GeoM.Translate(p.x+charW/2-tw/2, p.y-15)
	top.ColorScale.ScaleWithColor(clr)
	text.Draw(screen, label, fontSmall, top)
}

func (s *lobbyScene) drawTopBar(g *game, screen *ebiten.Image) {
	fillRect(screen, 0, 0, screenW, 14, color.RGBA{0, 0, 0, 170})
	drawText(screen, truncate(s.lobbyName, 20)+" ["+s.code+"]  "+fmt.Sprintf("%d HERE", len(s.others)+1), fontSmall, 6, 3, color.White)
	right := "ESC: LEAVE"
	tw, _ := text.Measure(right, fontSmall, 0)
	drawText(screen, right, fontSmall, float64(screenW)-tw-6, 3, uiDim)
}

const (
	chatW = 300.0
	chatH = 168.0
)

func (s *lobbyScene) drawChat(g *game, screen *ebiten.Image) {
	x0 := 8.0
	y0 := float64(screenH) - chatH - 8
	fillRect(screen, x0, y0, chatW, chatH, uiPanel)

	// Tabs.
	tx := x0 + 4
	for i, t := range s.chat.tabs {
		label := truncate(t.label, 8)
		tw, _ := text.Measure(label, fontSmall, 0)
		w := tw + 10
		bg := color.RGBA{40, 56, 72, 255}
		fg := color.RGBA{180, 180, 180, 255}
		if i == s.chat.active {
			bg = color.RGBA{72, 104, 136, 255}
			fg = color.RGBA{255, 255, 255, 255}
		}
		fillRect(screen, tx, y0+4, w, 14, bg)
		drawText(screen, label, fontSmall, tx+5, y0+7, fg)
		if t.unread > 0 {
			// Notification badge.
			fillRect(screen, tx+w-4, y0+2, 10, 10, color.RGBA{216, 64, 56, 255})
			drawText(screen, fmt.Sprintf("%d", min(t.unread, 9)), fontSmall, tx+w-3, y0+3, color.White)
		}
		tx += w + 4
	}

	// Log: last lines that fit.
	tab := s.chat.activeTab()
	lineH := 11.0
	maxLines := 11
	start := 0
	if len(tab.log) > maxLines {
		start = len(tab.log) - maxLines
	}
	y := y0 + 24
	for _, line := range tab.log[start:] {
		if line.system {
			drawText(screen, "* "+truncate(line.text, 34), fontSmall, x0+6, y, uiDim)
		} else {
			drawText(screen, truncate(line.from+": "+line.text, 36), fontSmall, x0+6, y, color.White)
		}
		y += lineH
	}

	// Input line.
	fillRect(screen, x0+2, y0+chatH-16, chatW-4, 14, color.RGBA{0, 0, 0, 120})
	if s.chat.focused {
		input := s.chat.input
		if (g.ticks/30)%2 == 0 {
			input += "_"
		}
		drawText(screen, "> "+input, fontSmall, x0+6, y0+chatH-13, color.White)
	} else {
		hint := "ENTER: CHAT  TAB: SWITCH  E: TALK"
		if s.chat.totalUnread() > 0 {
			hint = fmt.Sprintf("%d UNREAD - TAB TO SWITCH", s.chat.totalUnread())
		}
		drawText(screen, hint, fontSmall, x0+6, y0+chatH-13, uiDim)
	}
}
