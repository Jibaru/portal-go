// Command lobby is a 2D multiplayer lobby with chat, built on Ebitengine with
// all netcode over the portal-go SDK.
//
// Flow: type your name → browse the live lobby list (search, join by 6-char
// code, or create your own and wait) → walk around a Pokémon-style plaza with
// the other members. Chat floats over the room with tabs: GENERAL plus one
// per person you talk to (walk up + E, or reply to a DM); messages arriving
// on an inactive tab raise a badge. ESC leaves and notifies the room.
//
// Solo sandbox (starts an in-process relay):
//
//	go run ./examples/lobby
//
// Host on your LAN (relay + play):    go run ./examples/lobby -host :8090
// Join:                               go run ./examples/lobby -addr 192.168.1.20:8090
// Relay only (headless):              go run ./examples/lobby -serve :8090
// Real Portal service:                go run ./examples/lobby -key pk_live_…
//
// All characters and tiles are original GBA-style pixel art drawn in code.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/hajimehoshi/ebiten/v2"

	portal "github.com/Jibaru/portal-go"
)

func main() {
	serve := flag.String("serve", "", "run a headless relay server on this address (e.g. :8090)")
	host := flag.String("host", "", "host: run a relay on this address AND play")
	addr := flag.String("addr", "", "join a relay at this address (host:port)")
	key := flag.String("key", "", "publishable key for the real Portal service (pk_…)")
	name := flag.String("name", "", "skip the name screen")
	sprites := flag.String("sprites", "", "path to a custom character sheet PNG (see assets/CREDITS.md for the layout)")
	reliable := flag.Bool("reliable", false, "route movement/heartbeats over reliable publishes (forced on with -key)")
	smoke := flag.Bool("smoke", false, "scripted self-test: join/create a lobby, wander, chat, exit")
	flag.Parse()

	if *serve != "" {
		listening, err := startRelay(*serve)
		if err != nil {
			log.Fatalf("relay: %v", err)
		}
		fmt.Printf("lobby relay listening on %s\nplayers join with: lobby -addr <your-ip>%s\n",
			listening, portSuffix(listening))
		select {}
	}

	var config portal.Config
	sandboxNote := ""
	switch {
	case *key != "":
		config = portal.Config{APIKey: *key}
		// The hosted service's ephemeral fan-out is unverified: without this,
		// players would chat fine but never see each other move.
		*reliable = true
	case *addr != "":
		config = relayConfig(*addr)
	case *host != "":
		listening, err := startRelay(*host)
		if err != nil {
			log.Fatalf("relay: %v", err)
		}
		fmt.Printf("hosting: relay on %s — friends join with -addr <your-ip>%s\n", listening, portSuffix(listening))
		config = relayConfig(selfAddr(listening))
	default:
		listening, err := startRelay("127.0.0.1:0")
		if err != nil {
			log.Fatalf("relay: %v", err)
		}
		fmt.Printf("solo sandbox: in-process relay on %s — others CANNOT join this run\n", listening)
		fmt.Println("to play together: one player runs -host :8090, the rest -addr <host-ip>:8090")
		config = relayConfig(listening)
		sandboxNote = "SOLO SANDBOX - OTHERS CAN'T JOIN (USE -host / -addr)"
	}

	client := portal.New(config)
	selfID := newPlayerID()

	loadFonts()
	loadSprites(*sprites)

	g := newGame(client, selfID, *name)
	g.reliableNet = *reliable
	g.sandboxNote = sandboxNote
	options := &ebiten.RunGameOptions{}
	if *smoke {
		g.smoke = true
		g.smokeUntil = 420 // ~7s at 60 TPS
		if g.nameInput == "" {
			g.nameInput = "SMOKE"
		}
		ebiten.SetWindowSize(64, 48)
		ebiten.SetWindowPosition(-4000, -4000)
		ebiten.SetWindowDecorated(false)
		options.SkipTaskbar = true
		options.InitUnfocused = true
	} else {
		ebiten.SetWindowSize(screenW*2, screenH*2)
	}
	ebiten.SetWindowTitle("LOBBY PLAZA — portal-go demo")
	if err := ebiten.RunGameWithOptions(g, options); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func relayConfig(addr string) portal.Config {
	return portal.Config{
		APIKey:      "pk_lobby",
		APIURL:      "http://" + addr,
		RealtimeURL: "ws://" + addr,
	}
}

func selfAddr(listening string) string {
	if strings.HasPrefix(listening, "[::]") || strings.HasPrefix(listening, "0.0.0.0") {
		return "127.0.0.1" + portSuffix(listening)
	}
	return listening
}

func portSuffix(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return addr
}
