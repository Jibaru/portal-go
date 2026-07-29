// Command chat is a minimal terminal chat client on Portal — a smoke test for
// the SDK.
//
// Against the real service (needs a publishable key):
//
//	go run ./examples/chat -key pk_live_… -channel hello-world
//
// Against a built-in mock Portal server (no account, no network — a bot
// answers every message, proving the full publish → ack → fan-out loop):
//
//	go run ./examples/chat -mock
//
// Type a line and press enter to send. Ctrl+C (or EOF) exits. Anonymous mode
// works with just the publishable key; pass -token for an authenticated
// session.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	portal "github.com/Jibaru/portal-go"
)

func main() {
	key := flag.String("key", os.Getenv("PORTAL_KEY"), "publishable key (pk_…)")
	channelID := flag.String("channel", "hello-world", "channel id")
	token := flag.String("token", "", "signed user token (optional; anonymous if omitted)")
	mock := flag.Bool("mock", false, "run against a built-in mock Portal server (no key needed)")
	flag.Parse()

	config := portal.Config{APIKey: *key, Token: *token}
	if *mock {
		config.APIKey = "pk_mock"
		config.APIURL, config.RealtimeURL = startMockPortal()
		fmt.Println("· mock Portal server running in-process; a bot will answer you")
	} else if config.APIKey == "" {
		fmt.Fprintln(os.Stderr, "missing -key (or PORTAL_KEY); or use -mock to try it without an account")
		os.Exit(2)
	}

	client := portal.New(config)

	ch := client.Channel(*channelID)
	ch.OnStatus(func(status portal.ChannelStatus, err error) {
		if err != nil {
			fmt.Printf("· status %s: %v\n", status, err)
			return
		}
		fmt.Printf("· status %s\n", status)
	})
	ch.OnMessage(func(m portal.Message) {
		var content struct {
			Text string `json:"text"`
		}
		_ = m.DecodeContent(&content)
		name := m.Sender.Username
		if name == "" {
			name = m.Sender.ID
		}
		fmt.Printf("[%s] %s\n", name, content.Text)
	})
	ch.OnPresence(func(p portal.Presence) {
		fmt.Printf("· %d here\n", p.Count)
	})

	ch.Acquire()
	defer ch.Release()

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		text := scanner.Text()
		if text == "" {
			continue
		}
		ch.SendTyping()
		if _, err := ch.Send(context.Background(), portal.SendInput{
			Content: map[string]string{"text": text},
		}); err != nil {
			fmt.Fprintf(os.Stderr, "send failed: %v\n", err)
		}
	}
	// Give in-flight fan-out (e.g. the mock bot's reply) a moment to land
	// before exiting on EOF.
	time.Sleep(time.Second)
}
