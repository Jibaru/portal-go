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

// ── Chat model rules ──────────────────────────────────────

func TestChatRoutingAndBadges(t *testing.T) {
	c := newChatModel()

	// General message while general is active: no badge.
	if !c.addIncoming(netEvent{T: "chat", ID: "pB", Name: "Bea", Text: "hello all", msgID: "m1"}, "pA") {
		t.Fatal("general message rejected")
	}
	if c.tab(generalTab).unread != 0 {
		t.Fatal("active tab must not badge")
	}

	// A DM to me creates the sender's tab and badges it (I'm on general).
	if !c.addIncoming(netEvent{T: "chat", ID: "pB", Name: "Bea", Text: "psst", To: "pA", msgID: "m2"}, "pA") {
		t.Fatal("DM to me rejected")
	}
	tab := c.tab("pB")
	if tab == nil || tab.unread != 1 || tab.label != "Bea" {
		t.Fatalf("DM tab wrong: %+v", tab)
	}

	// A DM between two other players is not mine.
	if c.addIncoming(netEvent{T: "chat", ID: "pB", Name: "Bea", Text: "secret", To: "pC", msgID: "m3"}, "pA") {
		t.Fatal("foreign DM must be ignored")
	}

	// Opening the tab clears the badge; a general message now badges general.
	c.openTab("pB")
	if c.tab("pB").unread != 0 {
		t.Fatal("openTab must clear the badge")
	}
	c.addIncoming(netEvent{T: "chat", ID: "pC", Name: "Cy", Text: "hi", msgID: "m4"}, "pA")
	if c.tab(generalTab).unread != 1 {
		t.Fatal("inactive general must badge")
	}
	if c.totalUnread() != 1 {
		t.Fatalf("totalUnread = %d, want 1", c.totalUnread())
	}

	// Duplicate message ids (history reseed vs live) apply once.
	if c.addIncoming(netEvent{T: "chat", ID: "pC", Name: "Cy", Text: "hi", msgID: "m4"}, "pA") {
		t.Fatal("duplicate msgID applied twice")
	}

	// Tab cycles back to general and clears its badge.
	c.nextTab()
	c.nextTab()
	if c.activeTab().key != generalTab && c.tab(generalTab).unread != 0 {
		t.Fatal("tab cycling broken")
	}
}

func TestChatDropPeerTab(t *testing.T) {
	c := newChatModel()
	c.ensureTab("pB", "Bea")
	c.openTab("pB")
	c.dropPeerTab("pB")
	if c.tab("pB") != nil || c.activeTab().key != generalTab {
		t.Fatal("dropPeerTab must remove the tab and refocus general")
	}
}

// ── Lobby codes ───────────────────────────────────────────

func TestLobbyCodes(t *testing.T) {
	for i := 0; i < 50; i++ {
		code := newLobbyCode()
		if !isLobbyCode(code) {
			t.Fatalf("generated code invalid: %q", code)
		}
	}
	for _, bad := range []string{"", "ABC", "ABCDEFG", "ABC-12", "abc123", "ABCDE0"} {
		if isLobbyCode(bad) {
			t.Fatalf("%q accepted as code", bad)
		}
	}
}

// ── Relay channel isolation + directory flow ──────────────

func TestRelayChannelIsolation(t *testing.T) {
	addr, err := startRelay("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client := portal.New(relayConfig(addr))

	roomA := joinChannel(client, "lobby-room-AAAAAA", "pA", 0, false)
	defer roomA.close()
	roomB := joinChannel(client, "lobby-room-BBBBBB", "pB", 0, false)
	defer roomB.close()
	waitUntil(t, 5*time.Second, "both rooms ready", func() bool {
		return roomA.status() == portal.StatusReady && roomB.status() == portal.StatusReady
	})

	// A speaks in room A; room B must hear nothing.
	roomA.sendReliable(netEvent{T: "chat", Name: "Alice", Text: "only for room A"})
	time.Sleep(300 * time.Millisecond)
	select {
	case ev := <-roomB.events:
		t.Fatalf("channel leak: room B received %+v", ev)
	default:
	}
}

func TestDirectoryHeartbeatReachesBrowser(t *testing.T) {
	addr, err := startRelay("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Two separate clients, as in real life.
	inLobby := joinChannel(portal.New(relayConfig(addr)), directoryChannel, "pHost", 0, false)
	defer inLobby.close()
	browsing := joinChannel(portal.New(relayConfig(addr)), directoryChannel, "pGuest", 0, false)
	defer browsing.close()
	waitUntil(t, 5*time.Second, "directory ready", func() bool {
		return inLobby.status() == portal.StatusReady && browsing.status() == portal.StatusReady
	})

	var got netEvent
	waitUntil(t, 5*time.Second, "lobby heartbeat at browser", func() bool {
		inLobby.sendEphemeral(netEvent{T: "lobby", Code: "QWERTY", LobbyName: "HOST'S LOBBY", Count: 3})
		select {
		case got = <-browsing.events:
			return got.T == "lobby" && got.Code == "QWERTY"
		default:
			return false
		}
	})
	if got.LobbyName != "HOST'S LOBBY" || got.Count != 3 {
		t.Fatalf("heartbeat mangled: %+v", got)
	}
}

// TestChatHistoryForLateJoiner: messages published before a member joins
// arrive via the channel's history backfill and seed the chat exactly once.
func TestChatHistoryForLateJoiner(t *testing.T) {
	addr, err := startRelay("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	room := "lobby-room-CCCCCC"

	early := joinChannel(portal.New(relayConfig(addr)), room, "pEarly", 50, false)
	defer early.close()
	waitUntil(t, 5*time.Second, "early ready", func() bool { return early.status() == portal.StatusReady })
	early.sendReliable(netEvent{T: "chat", Name: "Early", Text: "before you arrived"})
	time.Sleep(300 * time.Millisecond)

	late := joinChannel(portal.New(relayConfig(addr)), room, "pLate", 50, false)
	defer late.close()
	waitUntil(t, 5*time.Second, "late ready", func() bool { return late.status() == portal.StatusReady })

	chat := newChatModel()
	waitUntil(t, 5*time.Second, "history seeded", func() bool {
		for _, ev := range late.history() {
			if ev.T == "chat" {
				chat.addIncoming(ev, "pLate")
			}
		}
		return len(chat.tab(generalTab).log) == 1
	})
	// Reseeding is idempotent.
	for _, ev := range late.history() {
		if ev.T == "chat" {
			chat.addIncoming(ev, "pLate")
		}
	}
	if n := len(chat.tab(generalTab).log); n != 1 {
		t.Fatalf("history reseed duplicated chat: %d lines", n)
	}
	if chat.tab(generalTab).log[0].text != "before you arrived" {
		t.Fatalf("wrong seeded line: %+v", chat.tab(generalTab).log[0])
	}
}
