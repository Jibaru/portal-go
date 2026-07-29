package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Jibaru/portal-go/wire"
)

// InboxStatus is the connection status of the inbox.
type InboxStatus string

const (
	// InboxIdle is never produced by a live inbox — a real inbox is always at
	// least connecting from the moment it's created (Client.Inbox connects
	// immediately). It exists for consumers modelling a not-yet-created handle.
	InboxIdle         InboxStatus = "idle"
	InboxConnecting   InboxStatus = "connecting"
	InboxReady        InboxStatus = "ready"
	InboxReconnecting InboxStatus = "reconnecting"
)

// InboxLatest previews the most recent message of an inbox entry.
type InboxLatest struct {
	Text     string
	SenderID string
	At       int64
}

// InboxEntry is one conversation row: positional read state (Unread is
// latestSeq − my watermark). Muting silences aggregation, not data: the entry
// keeps updating and stops contributing to Counter, but items addressed to you
// still count and still land.
type InboxEntry struct {
	ID   string
	Name string
	Meta json.RawMessage
	// Latest is absent (nil) on >100-member channels (seq-only tier).
	Latest *InboxLatest
	Unread int
	Muted  bool
	// At is recency (the sort key), epoch milliseconds.
	At int64
}

// InboxItem is a targeted item: a mention, a to:-send, or a notify descriptor.
// Items carry PER-ITEM read state (not a watermark).
type InboxItem struct {
	// ID is the event id — the notification's idempotency key: whatever key was
	// supplied when the notification was sent arrives back unchanged, so a
	// redelivered id is the same event, not a new one.
	ID    string
	Type  string
	Title string
	// Data is the userland payload; decode with json.Unmarshal.
	Data json.RawMessage
	// ChannelID is set when channel-originated (mention, to-send).
	ChannelID string
	At        int64
	Read      bool
}

// InboxQuery scopes an InboxView.
type InboxQuery struct {
	// ChannelID scopes the entire view (items + entry) to one channel.
	ChannelID string
	// Where filters the item feed over the flattened record: scalar fields of
	// Data plus type, channelId, read, muted (envelope wins collisions).
	Where Where
}

// InboxSnapshot is a point-in-time copy of the inbox state.
type InboxSnapshot struct {
	// Channels is recency-sorted.
	Channels []InboxEntry
	// Items holds targeted items: mentions, to-sends, notify descriptors.
	Items []InboxItem
	// Counter is the global badge: Σ channel unreads + unseen items. Muted
	// entries excluded — EXCEPT items addressed to you (a mention in a muted
	// room still badges).
	Counter int
	Status  InboxStatus
}

type inboxDeps struct {
	hosts       hosts
	apiKey      string
	credentials *credentials
	httpc       *http.Client
}

type inboxEvents struct {
	item   listeners[func(InboxItem)]
	change listeners[func()]
}

// inboxConnection is the Go port of @portalsdk/core's InboxConnection.
type inboxConnection struct {
	mu sync.Mutex

	deps   inboxDeps
	events inboxEvents

	socket         *transport
	disposed       bool
	tokenRetryUsed bool
	// synthesized: once synthesized for an anonymous token, the store stays
	// empty and never reconnects.
	synthesized bool

	entries map[string]wire.InboxEntry
	items   map[string]wire.InboxItem
	counter int
	status  InboxStatus

	keepaliveStop chan struct{}
}

func newInboxConnection(deps inboxDeps) *inboxConnection {
	return &inboxConnection{
		deps:    deps,
		entries: map[string]wire.InboxEntry{},
		items:   map[string]wire.InboxItem{},
		status:  InboxConnecting,
	}
}

// ── Lifecycle ─────────────────────────────────────────────

func (c *inboxConnection) connect() {
	c.mu.Lock()
	if c.socket != nil || c.synthesized {
		c.mu.Unlock()
		return
	}
	c.disposed = false
	c.tokenRetryUsed = false
	c.socket = newTransport(c.buildURL, c.onEvent)
	c.mu.Unlock()
}

func (c *inboxConnection) teardown() {
	c.mu.Lock()
	c.disposed = true
	c.stopKeepaliveLocked()
	socket := c.socket
	c.socket = nil
	c.entries = map[string]wire.InboxEntry{}
	c.items = map[string]wire.InboxItem{}
	c.counter = 0
	c.synthesized = false
	c.status = InboxConnecting
	c.mu.Unlock()
	if socket != nil {
		socket.close()
	}
	c.emitChange()
}

// reauthenticate re-authenticates after an identity change (login/logout): drop
// the current session and reconnect so the inbox reflects the new user —
// including re-synthesizing an empty inbox when the new identity is anonymous.
func (c *inboxConnection) reauthenticate() {
	c.teardown()
	c.connect()
}

func (c *inboxConnection) buildURL(ctx context.Context) (string, error) {
	token, err := c.deps.credentials.resolve(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set(wire.ParamVersion, strconv.Itoa(wire.ProtocolVersion))
	q.Set(wire.ParamToken, token)
	q.Set(wire.ParamKey, c.deps.apiKey)
	return c.deps.hosts.realtimeURL + "/inbox?" + q.Encode(), nil
}

// ── Event handling ────────────────────────────────────────

func (c *inboxConnection) onEvent(event socketEvent) {
	c.mu.Lock()
	if c.disposed {
		c.mu.Unlock()
		return
	}
	switch event.kind {
	case socketOpen:
		c.startKeepaliveLocked()
		c.mu.Unlock()
	case socketMessage:
		c.mu.Unlock()
		c.onMessage(event.data)
	case socketRefused:
		c.mu.Unlock()
		c.onRefused(event.code, event.reason)
	case socketClosed:
		c.stopKeepaliveLocked()
		change := false
		if !c.synthesized && c.status != InboxReconnecting {
			c.status = InboxReconnecting
			change = true
		}
		c.mu.Unlock()
		if change {
			c.emitChange()
		}
	}
}

func (c *inboxConnection) onMessage(raw []byte) {
	frame := wire.ParseInboxFrame(raw)
	if frame == nil {
		return
	}
	switch f := frame.(type) {
	case *wire.InboxReadyFrame:
		c.mu.Lock()
		c.entries = map[string]wire.InboxEntry{}
		c.items = map[string]wire.InboxItem{}
		for _, entry := range f.Entries {
			c.entries[entry.ID] = entry
		}
		for _, item := range f.Items {
			c.items[item.ID] = item
		}
		c.counter = f.Counter
		c.tokenRetryUsed = false
		c.status = InboxReady
		c.mu.Unlock()
		c.emitChange()
	case *wire.InboxEntryFrame:
		c.mu.Lock()
		c.entries[f.Entry.ID] = f.Entry
		c.mu.Unlock()
		c.emitChange()
	case *wire.InboxItemFrame:
		c.mu.Lock()
		_, existed := c.items[f.Item.ID]
		c.items[f.Item.ID] = f.Item
		c.mu.Unlock()
		c.emitChange()
		if !existed {
			item := toInboxItem(f.Item)
			for _, fn := range c.events.item.snapshot() {
				fn(item)
			}
		}
	case *wire.InboxCounterFrame:
		c.mu.Lock()
		c.counter = f.N
		c.mu.Unlock()
		c.emitChange()
	}
}

func (c *inboxConnection) onRefused(code, reason string) {
	if code == string(wire.RefusalAnonymousNotAllowed) {
		c.synthesize()
		return
	}
	decision := classifyRefusal(code, reason)
	if decision.tokenExpired {
		credentials := c.deps.credentials
		if credentials.managed() || !credentials.userStatic() {
			c.mu.Lock()
			retryUsed := c.tokenRetryUsed
			socket := c.socket
			if !retryUsed {
				c.tokenRetryUsed = true
			}
			change := false
			if retryUsed && c.status != InboxReconnecting {
				c.status = InboxReconnecting
				change = true
			}
			c.mu.Unlock()
			if retryUsed {
				if change {
					c.emitChange()
				}
				return
			}
			if credentials.managed() {
				credentials.invalidate()
			}
			if socket != nil {
				socket.reconnect()
			}
			return
		}
	}
	c.mu.Lock()
	c.stopKeepaliveLocked()
	socket := c.socket
	c.mu.Unlock()
	if socket != nil {
		socket.close()
	}
}

// synthesize replaces the store with a permanently-empty, ready inbox for an
// anonymous token, so calling code needs no special case.
func (c *inboxConnection) synthesize() {
	c.mu.Lock()
	c.synthesized = true
	c.stopKeepaliveLocked()
	socket := c.socket
	c.socket = nil
	c.entries = map[string]wire.InboxEntry{}
	c.items = map[string]wire.InboxItem{}
	c.counter = 0
	c.status = InboxReady
	c.mu.Unlock()
	if socket != nil {
		socket.close()
	}
	c.emitChange()
}

// ── Read + mute actions (two read models) ─────────────────

// markEntryRead advances the inbox position for one channel — clears its badge.
func (c *inboxConnection) markEntryRead(channelID string) {
	c.mu.Lock()
	if entry, ok := c.entries[channelID]; ok {
		entry.Unread = 0
		c.entries[channelID] = entry
	}
	c.mu.Unlock()
	c.sendFrame(&wire.InboxReadFrame{ChannelID: channelID})
	c.emitChange()
}

// markItemRead flips one item's read flag — never cascades.
func (c *inboxConnection) markItemRead(id string) {
	c.mu.Lock()
	if item, ok := c.items[id]; ok {
		item.Read = true
		c.items[id] = item
	}
	c.mu.Unlock()
	c.sendFrame(&wire.InboxItemReadFrame{ID: id})
	c.emitChange()
}

// markAllRead marks every item read — global, zero-arg.
func (c *inboxConnection) markAllRead() {
	c.mu.Lock()
	for id, item := range c.items {
		item.Read = true
		c.items[id] = item
	}
	c.mu.Unlock()
	c.sendFrame(&wire.InboxReadAllFrame{})
	c.emitChange()
}

// setMute sets the durable per-channel mute preference.
func (c *inboxConnection) setMute(channelID string, muted bool) {
	c.mu.Lock()
	if entry, ok := c.entries[channelID]; ok {
		entry.Muted = muted
		c.entries[channelID] = entry
	}
	c.mu.Unlock()
	c.sendFrame(&wire.InboxMuteFrame{ChannelID: channelID, Muted: muted})
	c.emitChange()
}

// ── Snapshot ──────────────────────────────────────────────

func (c *inboxConnection) snapshot() InboxSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	channels := make([]InboxEntry, 0, len(c.entries))
	for _, entry := range c.entries {
		channels = append(channels, toInboxEntry(entry))
	}
	sort.Slice(channels, func(i, j int) bool { return channels[i].At > channels[j].At })
	items := make([]InboxItem, 0, len(c.items))
	for _, item := range c.items {
		items = append(items, toInboxItem(item))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].At > items[j].At })
	return InboxSnapshot{Channels: channels, Items: items, Counter: c.counter, Status: c.status}
}

func toInboxEntry(entry wire.InboxEntry) InboxEntry {
	out := InboxEntry{
		ID:     entry.ID,
		Name:   entry.Name,
		Meta:   json.RawMessage(entry.Meta),
		Unread: entry.Unread,
		Muted:  entry.Muted,
		At:     entry.At,
	}
	if entry.Latest != nil {
		out.Latest = &InboxLatest{Text: entry.Latest.Text, SenderID: entry.Latest.Sender.ID, At: entry.Latest.At}
	}
	return out
}

func toInboxItem(item wire.InboxItem) InboxItem {
	return InboxItem{
		ID:        item.ID,
		Type:      item.Type,
		Title:     item.Title,
		Data:      json.RawMessage(item.Data),
		ChannelID: item.ChannelID,
		At:        item.At,
		Read:      item.Read,
	}
}

// ── Keepalive + plumbing ──────────────────────────────────

func (c *inboxConnection) startKeepaliveLocked() {
	c.stopKeepaliveLocked()
	stop := make(chan struct{})
	c.keepaliveStop = stop
	go func() {
		ticker := time.NewTicker(keepaliveEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.sendFrame(&wire.PingFrame{})
			case <-stop:
				return
			}
		}
	}()
}

func (c *inboxConnection) stopKeepaliveLocked() {
	if c.keepaliveStop != nil {
		close(c.keepaliveStop)
		c.keepaliveStop = nil
	}
}

func (c *inboxConnection) sendFrame(frame wire.Frame) {
	c.mu.Lock()
	socket := c.socket
	c.mu.Unlock()
	if socket == nil {
		return
	}
	data, err := wire.SerializeFrame(frame)
	if err != nil {
		return
	}
	socket.send(data)
}

func (c *inboxConnection) emitChange() {
	for _, fn := range c.events.change.snapshot() {
		fn()
	}
}

// ── Public handle ─────────────────────────────────────────

// Inbox is the per-user inbox: conversation rows (positional read state) plus
// targeted items (per-item read state) and the global badge counter.
//
// Anonymous users get a permanently-empty ready inbox, so calling code needs no
// special case.
type Inbox struct {
	connection *inboxConnection
}

// Snapshot returns a point-in-time copy of the inbox state.
func (i *Inbox) Snapshot() InboxSnapshot { return i.connection.snapshot() }

// Channels is the recency-sorted conversation rows.
func (i *Inbox) Channels() []InboxEntry { return i.Snapshot().Channels }

// Channel looks up one entry by channel id — ALWAYS against the full registry,
// ignoring any view filter.
func (i *Inbox) Channel(id string) (InboxEntry, bool) {
	i.connection.mu.Lock()
	entry, ok := i.connection.entries[id]
	i.connection.mu.Unlock()
	if !ok {
		return InboxEntry{}, false
	}
	return toInboxEntry(entry), true
}

// Items is the targeted item feed: mentions, to-sends, notify descriptors.
func (i *Inbox) Items() []InboxItem { return i.Snapshot().Items }

// Counter is the global badge: Σ channel unreads + unseen items. Muted entries
// excluded — except items addressed to you.
func (i *Inbox) Counter() int { return i.Snapshot().Counter }

// Status is the inbox connection status.
func (i *Inbox) Status() InboxStatus { return i.Snapshot().Status }

// View derives a filtered lens over the same connection — one socket, N views.
func (i *Inbox) View(query InboxQuery) *InboxView {
	return &InboxView{connection: i.connection, query: query}
}

// MarkAllRead marks ALL items read. Global, zero-arg: it ignores any view
// filter. Scoped clearing = iterate a view.
func (i *Inbox) MarkAllRead() { i.connection.markAllRead() }

// MarkChannelRead advances the INBOX position for this channel only — clears
// the sidebar badge. Fully independent of the channel's own watermark.
func (i *Inbox) MarkChannelRead(channelID string) { i.connection.markEntryRead(channelID) }

// MarkItemRead flips THIS item only — never cascades to older items.
func (i *Inbox) MarkItemRead(itemID string) { i.connection.markItemRead(itemID) }

// Mute sets the durable per-user-per-channel mute preference.
func (i *Inbox) Mute(channelID string) { i.connection.setMute(channelID, true) }

// Unmute clears the durable per-user-per-channel mute preference.
func (i *Inbox) Unmute(channelID string) { i.connection.setMute(channelID, false) }

// OnItem fires when a new targeted item arrives (a redelivered id is the same
// event, not a new one — it does not re-fire).
func (i *Inbox) OnItem(fn func(InboxItem)) Unsubscribe {
	return i.connection.events.item.add(fn)
}

// OnChange fires on any inbox state change.
func (i *Inbox) OnChange(fn func()) Unsubscribe {
	return i.connection.events.change.add(fn)
}

// Subscribe registers a listener called whenever the snapshot changes.
func (i *Inbox) Subscribe(listener func()) Unsubscribe {
	return i.connection.events.change.add(listener)
}

// InboxView is a filtered lens over the inbox: scope to a channel and/or filter
// the item feed.
type InboxView struct {
	connection *inboxConnection
	query      InboxQuery
}

// Snapshot computes the filtered view.
func (v *InboxView) Snapshot() (channels []InboxEntry, items []InboxItem, unseen int) {
	source := v.connection.snapshot()
	muted := map[string]bool{}
	for _, entry := range source.Channels {
		muted[entry.ID] = entry.Muted
		if v.query.ChannelID == "" || entry.ID == v.query.ChannelID {
			channels = append(channels, entry)
		}
	}
	for _, item := range source.Items {
		if v.query.ChannelID != "" && item.ChannelID != v.query.ChannelID {
			continue
		}
		if v.query.Where != nil && !matchesWhere(itemRecord(item, muted[item.ChannelID]), v.query.Where) {
			continue
		}
		items = append(items, item)
		if !item.Read {
			unseen++
		}
	}
	return channels, items, unseen
}

// Channels is the entry rows within this view's scope.
func (v *InboxView) Channels() []InboxEntry { channels, _, _ := v.Snapshot(); return channels }

// Items is the filtered item feed.
func (v *InboxView) Items() []InboxItem { _, items, _ := v.Snapshot(); return items }

// Unseen counts unread items within THIS view's filter.
func (v *InboxView) Unseen() int { _, _, unseen := v.Snapshot(); return unseen }

// OnItem fires for new items on the underlying inbox (unfiltered).
func (v *InboxView) OnItem(fn func(InboxItem)) Unsubscribe {
	return v.connection.events.item.add(fn)
}

// Subscribe registers a listener called whenever the underlying inbox changes.
func (v *InboxView) Subscribe(listener func()) Unsubscribe {
	return v.connection.events.change.add(listener)
}

// itemRecord flattens an item for where-matching: scalar fields of Data, then
// the envelope fields (type, channelId, read, muted) — envelope wins collisions.
func itemRecord(item InboxItem, muted bool) map[string]any {
	record := map[string]any{}
	var data map[string]any
	if json.Unmarshal(item.Data, &data) == nil {
		for key, value := range data {
			switch value.(type) {
			case string, float64, bool:
				record[key] = value
			}
		}
	}
	record["type"] = item.Type
	if item.ChannelID != "" {
		record["channelId"] = item.ChannelID
	} else {
		record["channelId"] = nil
	}
	record["read"] = item.Read
	record["muted"] = muted
	return record
}
