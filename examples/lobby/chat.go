package main

// The chat model is pure state (no ebiten), so the tab/unread/routing rules
// are unit-testable headlessly.

const generalTab = "general"

type chatLine struct {
	from   string
	text   string
	system bool
}

type chatTab struct {
	key    string // generalTab, or the peer's player id for a DM tab
	label  string
	unread int
	log    []chatLine
}

type chatModel struct {
	tabs    []*chatTab
	active  int
	focused bool
	input   string
	seen    map[string]struct{} // message ids (dedupes history reseeds vs live)
}

func newChatModel() *chatModel {
	return &chatModel{
		tabs: []*chatTab{{key: generalTab, label: "GENERAL"}},
		seen: map[string]struct{}{},
	}
}

func (c *chatModel) tab(key string) *chatTab {
	for _, t := range c.tabs {
		if t.key == key {
			return t
		}
	}
	return nil
}

// ensureTab returns the tab for key, creating it (with label) if missing.
func (c *chatModel) ensureTab(key, label string) *chatTab {
	if t := c.tab(key); t != nil {
		if label != "" {
			t.label = label
		}
		return t
	}
	t := &chatTab{key: key, label: label}
	c.tabs = append(c.tabs, t)
	return t
}

func (c *chatModel) activeTab() *chatTab { return c.tabs[c.active] }

// openTab activates the tab and clears its notification badge.
func (c *chatModel) openTab(key string) {
	for i, t := range c.tabs {
		if t.key == key {
			c.active = i
			t.unread = 0
			return
		}
	}
}

// nextTab cycles to the following tab (wraps), clearing its badge.
func (c *chatModel) nextTab() {
	c.active = (c.active + 1) % len(c.tabs)
	c.tabs[c.active].unread = 0
}

// push appends a line to a tab; a message landing on a non-active tab raises
// its notification badge.
func (c *chatModel) push(t *chatTab, line chatLine) {
	t.log = append(t.log, line)
	if len(t.log) > 100 {
		t.log = t.log[len(t.log)-100:]
	}
	if c.activeTab() != t {
		t.unread++
	}
}

// addIncoming routes a received chat event. General messages land on the
// general tab; a DM addressed to me lands on (and creates) the sender's tab.
// A DM between two other players is not mine to read. Returns whether the
// message was applied (false: duplicate or not addressed to me).
func (c *chatModel) addIncoming(ev netEvent, selfID string) bool {
	if ev.msgID != "" {
		if _, dup := c.seen[ev.msgID]; dup {
			return false
		}
		c.seen[ev.msgID] = struct{}{}
	}
	name := ev.Name
	if name == "" {
		name = "?"
	}
	switch {
	case ev.To == "":
		c.push(c.tab(generalTab), chatLine{from: name, text: ev.Text})
	case ev.To == selfID:
		c.push(c.ensureTab(ev.ID, name), chatLine{from: name, text: ev.Text})
	default:
		return false
	}
	return true
}

// addOwn records my own sent message on the tab it was typed into.
func (c *chatModel) addOwn(tabKey, myName, text string) {
	t := c.tab(tabKey)
	if t == nil {
		t = c.ensureTab(tabKey, tabKey)
	}
	t.log = append(t.log, chatLine{from: myName, text: text})
	if len(t.log) > 100 {
		t.log = t.log[len(t.log)-100:]
	}
}

// system adds a gray notice (joins/leaves) to the general tab.
func (c *chatModel) system(text string) {
	c.push(c.tab(generalTab), chatLine{text: text, system: true})
}

// dropPeerTab removes a DM tab when its peer is gone, keeping focus sane.
func (c *chatModel) dropPeerTab(key string) {
	for i, t := range c.tabs {
		if t.key == key {
			c.tabs = append(c.tabs[:i], c.tabs[i+1:]...)
			if c.active >= len(c.tabs) {
				c.active = len(c.tabs) - 1
			}
			return
		}
	}
}

// totalUnread sums every tab's badge (for a global indicator).
func (c *chatModel) totalUnread() int {
	n := 0
	for _, t := range c.tabs {
		n += t.unread
	}
	return n
}
