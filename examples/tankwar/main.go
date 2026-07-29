// Command tankwar is a Battle City-inspired 2D multiplayer war game built on
// Ebitengine, with all netcode running through the portal-go SDK.
//
// Solo sandbox (starts an in-process relay):
//
//	go run ./examples/tankwar
//
// Host a match on your LAN (relay + play):
//
//	go run ./examples/tankwar -host :8089
//
// Join a match:
//
//	go run ./examples/tankwar -addr 192.168.1.20:8089
//
// Relay only (headless server):
//
//	go run ./examples/tankwar -serve :8089
//
// Real Portal service:
//
//	go run ./examples/tankwar -key pk_live_… -channel tankwar
//
// Controls: arrows/WASD to move, space to shoot. Every kill is +10 on the
// leaderboard; getting shot means a 3s respawn at a random location.
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"

	"github.com/hajimehoshi/ebiten/v2"

	portal "github.com/Jibaru/portal-go"
)

func main() {
	serve := flag.String("serve", "", "run a headless relay server on this address (e.g. :8089) and exit on Ctrl+C")
	host := flag.String("host", "", "host a match: run a relay on this address AND play")
	addr := flag.String("addr", "", "join a relay at this address (host:port)")
	key := flag.String("key", "", "publishable key for the real Portal service (pk_…)")
	channelID := flag.String("channel", "tankwar", "channel id (one channel = one battlefield)")
	name := flag.String("name", "", "skip the name screen")
	smoke := flag.Bool("smoke", false, "scripted self-test: auto-join, drive and shoot, exit after ~4s")
	flag.Parse()

	if *serve != "" {
		listening, err := startRelay(*serve)
		if err != nil {
			log.Fatalf("relay: %v", err)
		}
		fmt.Printf("tankwar relay listening on %s\nplayers join with: tankwar -addr <your-ip>%s\n",
			listening, portSuffix(listening))
		select {}
	}

	var config portal.Config
	switch {
	case *key != "":
		config = portal.Config{APIKey: *key}
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
		fmt.Printf("solo sandbox: in-process relay on %s\n", listening)
		config = relayConfig(listening)
	}

	selfID := fmt.Sprintf("p_%08x", rand.Uint32())
	net := newNetClient(config, *channelID, selfID)
	defer net.close()

	loadFonts()
	loadSprites()

	ebiten.SetWindowSize(screenW*2, screenH*2)
	ebiten.SetWindowTitle("TANK WAR — portal-go demo")

	g := newGame(net, selfID, *name)
	options := &ebiten.RunGameOptions{}
	if *smoke {
		g.smoke = true
		g.smokeUntil = 240 // ~4s at 60 TPS
		if g.nameInput == "" {
			g.nameInput = "SMOKE"
		}
		// Keep the self-test out of the way: tiny window parked offscreen.
		ebiten.SetWindowSize(64, 48)
		ebiten.SetWindowPosition(-4000, -4000)
		ebiten.SetWindowDecorated(false)
		options.SkipTaskbar = true
		options.InitUnfocused = true
	}
	if err := ebiten.RunGameWithOptions(g, options); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *smoke {
		fmt.Printf("smoke ok: ticks=%d bullets_fired=%d score=%d peers=%d\n",
			g.ticks, g.bulletSeq, g.me.score, len(g.others))
	}
}

func relayConfig(addr string) portal.Config {
	return portal.Config{
		APIKey:      "pk_tankwar",
		APIURL:      "http://" + addr,
		RealtimeURL: "ws://" + addr,
	}
}

// selfAddr rewrites a wildcard listen address into something dialable locally.
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
