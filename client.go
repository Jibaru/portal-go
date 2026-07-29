// Package portal is a Go client for Portal — realtime infrastructure: live
// chat, presence, and in-app notifications.
//
// It is a from-the-wire port of the official JavaScript client
// (@portalsdk/core), speaking the same protocol: persistent publishes over
// HTTP, everything live over a WebSocket per channel plus one for the inbox.
//
//	client := portal.New(portal.Config{APIKey: "pk_…"})
//	ch := client.Channel("room-7")
//	ch.Acquire()
//	defer ch.Release()
//	ch.OnMessage(func(m portal.Message) { … })
//	ch.Send(ctx, portal.SendInput{Content: map[string]any{"text": "hello"}})
package portal

import (
	"log"
	"net/http"
	"strconv"
	"sync"
)

// ChannelOption configures a channel handle at first creation.
type ChannelOption func(*channelOptions)

type channelOptions struct {
	history     int
	historyNone bool
	metadata    map[string]any
}

// WithHistory sets the initial backfill size on connect (default 50).
func WithHistory(limit int) ChannelOption {
	return func(o *channelOptions) { o.history = limit; o.historyNone = false }
}

// WithHistoryNone starts live-only: no initial backfill.
func WithHistoryNone() ChannelOption {
	return func(o *channelOptions) { o.historyNone = true }
}

// WithMetadata sets the initial presence metadata for this session.
func WithMetadata(metadata map[string]any) ChannelOption {
	return func(o *channelOptions) { o.metadata = metadata }
}

// Client is the Portal client (§1).
//
// Construction is synchronous and passive: it stores config and creates empty
// registries, with no network, no token fetch, and no validation. The first
// Acquire (or the first Inbox call) is the first network moment — safe to
// construct at program start before any user exists.
type Client struct {
	config      Config
	hosts       hosts
	credentials *credentials
	httpc       *http.Client

	mu       sync.Mutex
	channels map[string]*channelCell
	inbox    *Inbox
}

type channelCell struct {
	handle     *Channel
	optionsKey string
}

// New creates a Client. Only Config.APIKey is required; omit Token/TokenFunc
// for anonymous mode.
func New(config Config) *Client {
	httpc := config.HTTPClient
	if httpc == nil {
		httpc = http.DefaultClient
	}
	h := resolveHosts(config)
	return &Client{
		config:      config,
		hosts:       h,
		credentials: newCredentials(h, config.APIKey, config.Token, config.Token != "", config.TokenFunc, httpc),
		httpc:       httpc,
		channels:    map[string]*channelCell{},
	}
}

// Channel is a registry lookup-or-create: the same handle for the same id, so
// many views of a room share one socket. No network until Acquire. Options
// apply at first creation — a later call with different options returns the
// existing handle and ignores them (with a logged warning).
func (c *Client) Channel(channelID string, opts ...ChannelOption) *Channel {
	options := channelOptions{history: 50}
	for _, opt := range opts {
		opt(&options)
	}
	key := optionsKey(options)

	c.mu.Lock()
	defer c.mu.Unlock()
	if cell, ok := c.channels[channelID]; ok {
		if len(opts) > 0 && cell.optionsKey != key {
			log.Printf("[portal] channel(%q) was already created with different options; the original options are kept and these are ignored", channelID)
		}
		return cell.handle
	}
	handle := newChannel(channelDeps{
		channelID:   channelID,
		hosts:       c.hosts,
		apiKey:      c.config.APIKey,
		credentials: c.credentials,
		httpc:       c.httpc,
		metadata:    options.metadata,
		history:     options.history,
		historyNone: options.historyNone,
	})
	c.channels[channelID] = &channelCell{handle: handle, optionsKey: key}
	return handle
}

// Inbox is a lazy singleton — created and connected on first use, never at
// construction.
func (c *Client) Inbox() *Inbox {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inbox == nil {
		connection := newInboxConnection(inboxDeps{
			hosts:       c.hosts,
			apiKey:      c.config.APIKey,
			credentials: c.credentials,
			httpc:       c.httpc,
		})
		c.inbox = &Inbox{connection: connection}
		connection.connect()
	}
	return c.inbox
}

// SetToken replaces the token source with a static string — e.g. on login.
// When the identity changes, any live channels and the inbox re-authenticate so
// no stale-identity session lingers; idle handles pick up the new credential on
// their next use.
func (c *Client) SetToken(token string) {
	if c.credentials.setToken(token, true) {
		c.reauthenticateAll()
	}
}

// SetTokenFunc replaces the token source with a callback, re-invoked on
// connect, reconnect and expiry.
func (c *Client) SetTokenFunc(fn TokenFunc) {
	if c.credentials.setTokenFunc(fn) {
		c.reauthenticateAll()
	}
}

// SetAnonymous returns to anonymous mode (e.g. on logout): the SDK mints and
// manages its own anonymous credential again.
func (c *Client) SetAnonymous() {
	if c.credentials.setToken("", false) {
		c.reauthenticateAll()
	}
}

func (c *Client) reauthenticateAll() {
	c.mu.Lock()
	handles := make([]*Channel, 0, len(c.channels))
	for _, cell := range c.channels {
		handles = append(handles, cell.handle)
	}
	inbox := c.inbox
	c.mu.Unlock()
	for _, handle := range handles {
		handle.reauthenticate()
	}
	if inbox != nil {
		inbox.connection.reauthenticate()
	}
}

func optionsKey(options channelOptions) string {
	key := "h:"
	if options.historyNone {
		key += "none"
	} else {
		key += strconv.Itoa(options.history)
	}
	key += ";m:"
	if options.metadata != nil {
		key += "set"
	}
	return key
}
