package portal

import (
	"context"
	"sync"
	"time"
)

// graceDelay keeps the connection alive briefly after the last Release, so an
// immediate re-Acquire (a page transition, a re-render) reuses the socket.
const graceDelay = 3 * time.Second

// Channel is a refcounted handle to one realtime channel.
//
// The registry returns the same handle for the same id. No network happens
// until the first Acquire (that is where the token is resolved); the last
// Release plus a ~3s grace tears the connection down. Pair every Acquire with a
// Release.
type Channel struct {
	connection *channelConnection

	mu         sync.Mutex
	count      int
	active     bool
	graceTimer *time.Timer
}

func newChannel(deps channelDeps) *Channel {
	return &Channel{connection: newChannelConnection(deps)}
}

// ── Refcounted lifecycle ──────────────────────────────────

// Acquire increments the refcount; the first acquire opens the connection.
func (c *Channel) Acquire() {
	c.mu.Lock()
	c.count++
	if c.graceTimer != nil {
		c.graceTimer.Stop()
		c.graceTimer = nil
	}
	start := !c.active
	if start {
		c.active = true
	}
	c.mu.Unlock()
	if start {
		c.connection.connect()
	}
}

// Release decrements the refcount; at zero, a ~3s grace period runs and then
// the connection is torn down.
func (c *Channel) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count == 0 {
		return
	}
	c.count--
	if c.count > 0 {
		return
	}
	c.graceTimer = time.AfterFunc(graceDelay, func() {
		c.mu.Lock()
		if c.count > 0 {
			c.mu.Unlock()
			return
		}
		c.graceTimer = nil
		c.active = false
		c.mu.Unlock()
		c.connection.teardown()
	})
}

// reauthenticate re-authenticates a live connection after the identity changed
// (login/logout). Only a held connection reconnects — an idle handle simply
// picks up the new credential on its next Acquire. The refcount is untouched;
// the socket is torn down and reopened so it re-auths cleanly with no
// stale-identity session lingering.
func (c *Channel) reauthenticate() {
	c.mu.Lock()
	held := c.count > 0
	c.mu.Unlock()
	if !held {
		return
	}
	c.connection.teardown()
	c.connection.connect()
}

// ── Store contract ────────────────────────────────────────

// Subscribe registers a listener called whenever the snapshot changes.
func (c *Channel) Subscribe(listener func()) Unsubscribe {
	return c.connection.events.store.add(listener)
}

// Snapshot returns a point-in-time copy of the channel's public state.
func (c *Channel) Snapshot() ChannelSnapshot {
	return c.connection.snapshot()
}

// ── Events ────────────────────────────────────────────────

// OnMessage fires for every newly delivered persistent message.
func (c *Channel) OnMessage(fn func(Message)) Unsubscribe {
	return c.connection.events.message.add(fn)
}

// OnEphemeral fires for incoming ephemeral deliveries (live cursors, transient
// signals). Ephemeral messages have no seq, no ordering or gap guarantees, and
// never appear in Messages — this event is their only surface. (A Go extension:
// the JS client drops incoming ephemeral traffic.)
func (c *Channel) OnEphemeral(fn func(Message)) Unsubscribe {
	return c.connection.events.ephemeral.add(fn)
}

// OnMention fires when a message's mentions include your user id.
func (c *Channel) OnMention(fn func(Message)) Unsubscribe {
	return c.connection.events.mention.add(fn)
}

// OnRetract fires with the retracted message's id.
func (c *Channel) OnRetract(fn func(messageID string)) Unsubscribe {
	return c.connection.events.retract.add(fn)
}

// OnPresence fires on every presence change.
func (c *Channel) OnPresence(fn func(Presence)) Unsubscribe {
	return c.connection.events.presence.add(fn)
}

// OnActivity fires when the live activity set changes.
func (c *Channel) OnActivity(fn func([]ActivityEntry)) Unsubscribe {
	return c.connection.events.activity.add(fn)
}

// OnStatus fires on status transitions, and additionally — without a status
// change — to carry an in-session error.
func (c *Channel) OnStatus(fn func(ChannelStatus, error)) Unsubscribe {
	return c.connection.events.status.add(fn)
}

// ── State reads ───────────────────────────────────────────

// Messages is the reactive, seq-ordered window; mutations (retractions) are
// applied in place.
func (c *Channel) Messages() []Message { return c.Snapshot().Messages }

// Presence is the current presence, or nil before ready.
func (c *Channel) Presence() *Presence { return c.Snapshot().Presence }

// Activity is the transient per-user activity, never self.
func (c *Channel) Activity() []ActivityEntry { return c.Snapshot().Activity }

// Typing is sugar: activity filtered to kind "typing", as user ids.
func (c *Channel) Typing() []string {
	var out []string
	for _, a := range c.Snapshot().Activity {
		if a.Kind == "typing" {
			out = append(out, a.UserID)
		}
	}
	return out
}

// Unread counts messages beyond the channel watermark.
func (c *Channel) Unread() int { return c.Snapshot().Unread }

// Status is the current connection status.
func (c *Channel) Status() ChannelStatus { return c.Snapshot().Status }

// Info describes the channel, from the connect snapshot; nil before ready.
func (c *Channel) Info() *ChannelInfo { return c.Snapshot().Info }

// Me is your own verified identity, post-connect; nil before ready.
func (c *Channel) Me() *Me { return c.Snapshot().Me }

// Ext holds extension snapshots from the connect frame, keyed by handle; nil
// before ready. A degraded extension is key-absent rather than empty.
func (c *Channel) Ext() map[string]RawJSON { return c.Snapshot().Ext }

// IsLoadingPrevious reports whether a LoadPrevious page is in flight.
func (c *Channel) IsLoadingPrevious() bool { return c.Snapshot().IsLoadingPrevious }

// HasPrevious starts true (optimistic — including under WithHistoryNone, before
// any page is fetched) and flips to false once LoadPrevious reaches the
// beginning of the channel.
func (c *Channel) HasPrevious() bool { return c.Snapshot().HasPrevious }

// ── Write plane ───────────────────────────────────────────

// Send sends one message. A persistent send resolves once the edge accepts it;
// an ephemeral send (SendInput.Ephemeral) resolves immediately and is
// fire-and-forget.
func (c *Channel) Send(ctx context.Context, input SendInput) (SendAck, error) {
	return c.connection.send(ctx, input)
}

// LoadPrevious loads the next older history page (backwards only). Returns
// whether more history remains. Concurrent calls share one in-flight fetch.
func (c *Channel) LoadPrevious(ctx context.Context) (bool, error) {
	return c.connection.loadPreviousPage(ctx)
}

// SendActivity announces transient activity ("typing", "thinking",
// "uploading", …). Throttled client-side (~3s); NO-OP on broadcast channels.
func (c *Channel) SendActivity(kind string) { c.connection.sendActivity(kind) }

// SendTyping is sugar for SendActivity("typing").
func (c *Channel) SendTyping() { c.connection.sendActivity("typing") }

// MarkAsRead advances the CHANNEL watermark (independent of inbox read state).
func (c *Channel) MarkAsRead() { c.connection.markAsRead() }

// SetMetadata replaces own presence metadata mid-session; the server
// re-announces it via presence deltas. Presentation only — never authz.
func (c *Channel) SetMetadata(metadata map[string]any) { c.connection.setMetadata(metadata) }

// Members fetches the member directory (standard channels: incl. offline,
// online merged). Not live state.
func (c *Channel) Members(ctx context.Context) ([]MemberRow, error) {
	return c.connection.members(ctx)
}

// View is a reserved surface in v1 — the where grammar is typed but rejected
// loudly, never silently ignored.
func (c *Channel) View(where any) (any, error) {
	return nil, newError(CodeNotYetSupported, "Filtering a channel with a view is reserved and not supported in v1.")
}
