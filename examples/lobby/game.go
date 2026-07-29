package main

import (
	"fmt"
	"image/color"
	"sort"
	"strings"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
	"github.com/hajimehoshi/ebiten/v2/text/v2"
	"github.com/hajimehoshi/ebiten/v2/vector"

	portal "github.com/Jibaru/portal-go"
)

type scene int

const (
	sceneName scene = iota
	sceneBrowser
	sceneLobby
)

// lobbyInfo is one row of the live directory, built from heartbeats and
// expired by absence.
type lobbyInfo struct {
	code     string
	name     string
	count    int
	lastSeen time.Time
}

const lobbyExpiry = 6 * time.Second

// browserRow is one selectable row of the browser list.
type browserRow struct {
	kind  int // 0 create, 1 join lobby, 2 join by code
	lobby *lobbyInfo
	code  string
}

type game struct {
	scene     scene
	client    *portal.Client
	selfID    string
	name      string
	nameInput string

	world *world

	// directory is held from the browser on — members keep heartbeating their
	// lobby into it so the list stays live for everyone else.
	directory *channelNet
	lobbies   map[string]*lobbyInfo
	search    string
	selected  int

	lobby *lobbyScene

	// reliableNet reroutes ephemeral traffic (walking, lobby heartbeats) over
	// reliable publishes — needed on the hosted service.
	reliableNet bool
	// sandboxNote warns that this run's relay is private to this process.
	sandboxNote string

	ticks int

	smoke      bool
	smokeUntil int
	smokeStats smokeStats
}

type smokeStats struct {
	chatRecv    int
	lobbiesSeen int
}

func newGame(client *portal.Client, selfID, presetName string) *game {
	g := &game{
		client:  client,
		selfID:  selfID,
		world:   buildWorld(),
		lobbies: map[string]*lobbyInfo{},
	}
	if presetName != "" {
		g.nameInput = presetName
	}
	return g
}

func (g *game) Layout(int, int) (int, int) { return screenW, screenH }

func (g *game) Update() error {
	g.ticks++
	if g.smoke {
		if done := g.updateSmoke(); done {
			return ebiten.Termination
		}
	}
	switch g.scene {
	case sceneName:
		g.updateName()
	case sceneBrowser:
		g.updateBrowser()
	case sceneLobby:
		g.lobby.update(g)
	}
	return nil
}

func (g *game) Draw(screen *ebiten.Image) {
	switch g.scene {
	case sceneName:
		g.drawName(screen)
	case sceneBrowser:
		g.drawBrowser(screen)
	case sceneLobby:
		g.lobby.draw(g, screen)
	}
}

// ── Name entry ────────────────────────────────────────────

func (g *game) updateName() {
	for _, r := range ebiten.AppendInputChars(nil) {
		if len(g.nameInput) < 12 && r >= 32 && r != 127 {
			g.nameInput += string(r)
		}
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyBackspace) && g.nameInput != "" {
		g.nameInput = g.nameInput[:len(g.nameInput)-1]
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyEnter) && g.nameInput != "" {
		g.enterBrowser()
	}
}

func (g *game) enterBrowser() {
	g.name = g.nameInput
	if g.directory == nil {
		g.directory = joinChannel(g.client, directoryChannel, g.selfID, 0, g.reliableNet)
	}
	g.scene = sceneBrowser
}

func (g *game) drawName(screen *ebiten.Image) {
	screen.Fill(color.RGBA{24, 40, 32, 255})
	centerText(screen, "LOBBY PLAZA", fontBig, 130, uiHighlight)
	centerText(screen, "A PORTAL-GO DEMO", fontSmall, 158, uiDim)
	centerText(screen, "TYPE YOUR NAME:", fontSmall, 214, color.White)
	name := g.nameInput
	if (g.ticks/30)%2 == 0 {
		name += "_"
	}
	centerText(screen, name, fontBig, 244, color.White)
	centerText(screen, "PRESS ENTER", fontSmall, 300, color.RGBA{120, 200, 120, 255})
}

// ── Lobby browser ─────────────────────────────────────────

func (g *game) updateBrowser() {
	// Directory heartbeats build the live list.
	for {
		select {
		case ev := <-g.directory.events:
			if ev.T == "lobby" && ev.Code != "" {
				info, ok := g.lobbies[ev.Code]
				if !ok {
					info = &lobbyInfo{code: ev.Code}
					g.lobbies[ev.Code] = info
					g.smokeStats.lobbiesSeen++
				}
				info.name = ev.LobbyName
				info.count = ev.Count
				info.lastSeen = time.Now()
			}
		default:
			goto drained
		}
	}
drained:
	for code, info := range g.lobbies {
		if time.Since(info.lastSeen) > lobbyExpiry {
			delete(g.lobbies, code)
		}
	}

	for _, r := range ebiten.AppendInputChars(nil) {
		if len(g.search) < 16 && r >= 32 && r != 127 {
			g.search += strings.ToUpper(string(r))
			g.selected = 0
		}
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyBackspace) && g.search != "" {
		g.search = g.search[:len(g.search)-1]
		g.selected = 0
	}
	rows := g.browserRows()
	if inpututil.IsKeyJustPressed(ebiten.KeyArrowDown) && g.selected < len(rows)-1 {
		g.selected++
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyArrowUp) && g.selected > 0 {
		g.selected--
	}
	if inpututil.IsKeyJustPressed(ebiten.KeyEnter) && len(rows) > 0 {
		g.activateRow(rows[g.selected])
	}
}

// browserRows builds the visible list: create-new first, then a join-by-code
// row when the search text IS a code, then matching lobbies (busiest first).
func (g *game) browserRows() []browserRow {
	rows := []browserRow{{kind: 0}}
	if isLobbyCode(g.search) {
		if _, listed := g.lobbies[g.search]; !listed {
			rows = append(rows, browserRow{kind: 2, code: g.search})
		}
	}
	list := make([]*lobbyInfo, 0, len(g.lobbies))
	needle := strings.ToUpper(g.search)
	for _, info := range g.lobbies {
		if needle == "" || strings.Contains(strings.ToUpper(info.name), needle) ||
			strings.Contains(info.code, needle) {
			list = append(list, info)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		return list[i].code < list[j].code
	})
	for _, info := range list {
		rows = append(rows, browserRow{kind: 1, lobby: info})
	}
	if g.selected >= len(rows) {
		g.selected = len(rows) - 1
	}
	return rows
}

func (g *game) activateRow(row browserRow) {
	switch row.kind {
	case 0:
		code := newLobbyCode()
		g.enterLobby(code, g.name+"'S LOBBY")
	case 1:
		g.enterLobby(row.lobby.code, row.lobby.name)
	case 2:
		g.enterLobby(row.code, "LOBBY "+row.code)
	}
}

func (g *game) enterLobby(code, name string) {
	g.lobby = newLobbyScene(g, code, name)
	g.search = ""
	g.selected = 0
	g.scene = sceneLobby
}

func (g *game) leaveLobby() {
	if g.lobby != nil {
		g.lobby.leave()
		g.lobby = nil
	}
	g.scene = sceneBrowser
}

func (g *game) drawBrowser(screen *ebiten.Image) {
	screen.Fill(color.RGBA{24, 40, 32, 255})
	centerText(screen, "LOBBIES", fontBig, 40, uiHighlight)

	search := g.search
	if (g.ticks/30)%2 == 0 {
		search += "_"
	}
	drawText(screen, "SEARCH OR CODE: "+search, fontSmall, 60, 70, color.White)

	rows := g.browserRows()
	y := 100.0
	for i, row := range rows {
		clr := color.RGBA{200, 200, 200, 255}
		prefix := "  "
		if i == g.selected {
			clr = uiHighlight
			prefix = "> "
		}
		var label string
		switch row.kind {
		case 0:
			label = prefix + "+ CREATE NEW LOBBY"
		case 1:
			label = fmt.Sprintf("%s%-16s [%s]  %d HERE", prefix, truncate(row.lobby.name, 16), row.lobby.code, row.lobby.count)
		case 2:
			label = prefix + "JOIN BY CODE: " + row.code
		}
		drawText(screen, label, fontSmall, 60, y, clr)
		y += 16
		if y > screenH-80 {
			break
		}
	}
	if len(rows) == 1 && g.search == "" {
		drawText(screen, "NO LOBBIES YET - CREATE ONE!", fontSmall, 60, y+8, uiDim)
	}
	drawText(screen, "TYPE TO SEARCH - ARROWS + ENTER TO JOIN", fontSmall, 60, screenH-40, uiDim)
	drawText(screen, "LINK: "+string(g.directory.status()), fontSmall, 60, screenH-24, color.RGBA{120, 120, 200, 255})
	if g.sandboxNote != "" {
		drawText(screen, g.sandboxNote, fontSmall, 60, screenH-56, color.RGBA{240, 140, 60, 255})
	}
}

// ── Smoke scripting ───────────────────────────────────────

func (g *game) updateSmoke() (done bool) {
	switch g.scene {
	case sceneName:
		if g.ticks > 10 {
			g.enterBrowser()
		}
	case sceneBrowser:
		rows := g.browserRows()
		// Join the first live lobby seen; create one if none appears in time.
		if len(rows) > 1 && rows[1].kind == 1 {
			g.activateRow(rows[1])
		} else if g.ticks > 150 {
			g.activateRow(rows[0])
		}
	}
	if g.ticks >= g.smokeUntil {
		fmt.Printf("smoke ok: scene=%d peers=%d chatRecv=%d lobbiesSeen=%d unread=%d\n",
			g.scene, g.smokePeers(), g.smokeStats.chatRecv, g.smokeStats.lobbiesSeen, g.smokeUnread())
		if g.lobby != nil {
			g.lobby.leave()
		}
		return true
	}
	return false
}

func (g *game) smokePeers() int {
	if g.lobby == nil {
		return 0
	}
	return len(g.lobby.others)
}

func (g *game) smokeUnread() int {
	if g.lobby == nil {
		return 0
	}
	return g.lobby.chat.totalUnread()
}

// ── Text helpers ──────────────────────────────────────────

func centerText(screen *ebiten.Image, s string, face *text.GoTextFace, y float64, clr color.Color) {
	tw, _ := text.Measure(s, face, 0)
	drawText(screen, s, face, float64(screenW)/2-tw/2, y, clr)
}

func drawText(screen *ebiten.Image, s string, face *text.GoTextFace, x, y float64, clr color.Color) {
	op := &text.DrawOptions{}
	op.GeoM.Translate(x, y)
	op.ColorScale.ScaleWithColor(clr)
	text.Draw(screen, s, face, op)
}

func fillRect(screen *ebiten.Image, x, y, w, h float64, clr color.Color) {
	vector.DrawFilledRect(screen, float32(x), float32(y), float32(w), float32(h), clr, false)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
