# portal-go

A Go client for [Portal](https://useportal.co) — realtime infrastructure: live
chat, presence, and in-app notifications.

This is an unofficial, from-the-wire port of the official JavaScript client
(`@portalsdk/core` + `@portalsdk/wire-protocol`), speaking the same protocol
(v1):

- **HTTP plane** — persistent publishes (`POST /v1/channels/{id}/messages`),
  history (initial backfill, scroll-back paging, gap-fill ranges), the member
  directory, and the anonymous token mint.
- **WebSocket plane** — one socket per channel
  (`wss://realtime.useportal.co/v1/channels/{id}`) plus one for the per-user
  inbox (`/inbox`). JSON text frames discriminated on `t`.

```
portal-go/          the client runtime (the @portalsdk/core equivalent)
portal-go/wire/     frame types + total parsers (the @portalsdk/wire-protocol equivalent)
portal-go/examples/ separate Go modules (via go.work) — their deps never touch the SDK
```

## Install

```sh
go get github.com/Jibaru/portal-go
```

## Quickstart

```go
package main

import (
    "context"
    "fmt"

    portal "github.com/Jibaru/portal-go"
)

func main() {
    // Construction is synchronous and passive — no network until the first Acquire().
    client := portal.New(portal.Config{
        APIKey: "pk_live_…", // publishable key; safe to embed
        // TokenFunc: func(ctx context.Context) (string, error) { return fetchJWT(ctx) },
    })

    ch := client.Channel("room-7")

    // Refcounted: the first Acquire opens the connection, the last Release
    // (plus a ~3s grace) tears it down.
    ch.Acquire()
    defer ch.Release()

    ch.OnMessage(func(m portal.Message) {
        var content struct{ Text string `json:"text"` }
        _ = m.DecodeContent(&content)
        fmt.Printf("[%s] %s\n", m.Sender.ID, content.Text)
    })

    _, err := ch.Send(context.Background(), portal.SendInput{
        Content: map[string]string{"text": "hello"},
    })
    if err != nil {
        panic(err)
    }

    select {} // keep listening
}
```

`client.Channel(id)` returns the same handle for the same id, so many views of
a room share one socket.

## Anonymous mode & auth

`Token`/`TokenFunc` are optional. Omit both and the client runs anonymously: it
mints and manages its own anonymous credential on first use and keeps one
stable anonymous identity (`anonId`) across refreshes.

```go
client := portal.New(portal.Config{APIKey: "pk_live_…"}) // anonymous

// Later, on login — live channels and the inbox re-authenticate cleanly:
client.SetTokenFunc(func(ctx context.Context) (string, error) { return fetchJWT(ctx) })

// On logout — back to anonymous:
client.SetAnonymous()
```

Anonymous users get `Me().Anon == true`, a permanently-empty ready inbox, and
are refused from channels marked `anonymous: false`
(`portal.IsAnonymousNotAllowed(err)`).

## Channels

A `*portal.Channel` exposes the reactive window and the operations over it:

- `Messages()` — the seq-ordered message window; retractions apply in place
  (tombstoned envelopes with stripped content).
- `Send(ctx, input)` — one form. A persistent send resolves once the edge
  accepts it (optimistic insert → ack, rolled back on rejection); an ephemeral
  send (`SendInput{Ephemeral: true}`) rides the socket, resolves immediately,
  and is fire-and-forget.
- `LoadPrevious(ctx)` / `HasPrevious()` / `IsLoadingPrevious()` — backwards
  history paging.
- `Presence()` — detailed (roster) on standard channels, aggregate (count) on
  broadcast channels.
- `Activity()` / `SendActivity(kind)` / `Typing()` / `SendTyping()` — transient
  per-user signals (throttled ~3s, expired by absence ~5s, no-op on broadcast).
- `Unread()` / `MarkAsRead()` — the channel read position (watermark).
- `Members(ctx)` — the fetched member directory (standard channels).
- `Status()` — `idle | connecting | ready | reconnecting | degraded |
  degraded-http | blocked`.
- `OnMessage / OnMention / OnRetract / OnPresence / OnActivity / OnStatus` —
  event listeners; every registration returns an `Unsubscribe`.
- `Subscribe(func())` / `Snapshot()` — the store contract, for binding UIs.

Delivery guarantees mirror the JS client: messages are ordered and deduped by
`seq`; a live gap (dropped frame) is detected and range-fetched from history
with 0–2s jitter; on reconnect the client sends the last contiguous seq
(`?last=`) and the sticky `?leaf=` token, and heals any missed span with the
same gap-fill path. Keepalive pings every 25s; reconnect backoff runs
1s → 30s ×1.5 with infinite retries.

## Inbox

```go
inbox := client.Inbox() // lazy singleton, connects on first use

inbox.OnChange(func() {
    fmt.Println("badge:", inbox.Counter())
})

// A filtered view — scope to a channel and/or filter the item feed.
mentions := inbox.View(portal.InboxQuery{
    Where: portal.Where{"type": {Eq: []any{"mention"}}},
})
fmt.Println("unseen mentions:", mentions.Unseen())
```

Channels are positional (each entry has a watermark and `Unread`); items are
per-item (`Read`, `MarkItemRead`). `Counter()` is the global badge. An
`InboxItem.ID` **is** the notification's idempotency key — a redelivered id is
the same event, not a new one (`OnItem` does not re-fire for it).

The two read models are independent: `Channel.MarkAsRead()` advances the
channel watermark (*reading*), `Inbox.MarkChannelRead(id)` clears the sidebar
badge (*noticing*), and the two may legitimately disagree.

## Errors

Every failure is a `*portal.Error` with a stable `Code`. Branch with the
helpers — `IsInvalidAPIKey`, `IsTokenExpired`, `IsNotMember`,
`IsChannelAtCapacity`, `IsAnonymousNotAllowed`, `IsBlocked` (a rejected send;
`Reason` carries user-visible copy), `IsNotYetSupported`, `IsDegraded` — or
`errors.As` + a switch on `Code`. `Send` returns the relevant one;
connection-level refusals arrive on `OnStatus` and move the status to
`blocked`.

Token expiry is retried once per session with a re-resolved credential
(callback tokens and managed anonymous tokens); a static string token cannot be
re-resolved and blocks instead.

## The wire package

`github.com/Jibaru/portal-go/wire` is usable standalone — e.g. to build a
mock server or a bot framework: every channel/inbox frame in both directions,
`ParseChannelFrame` / `ParseInboxFrame` / `ParseChannelClientFrame` /
`ParseInboxClientFrame` (total, non-panicking, with unknown-frame passthrough
for forward compatibility) and `SerializeFrame`.

## Deviations from the JS client

Where Go idiom forced a different shape:

- Callbacks + `Snapshot()` replace `useSyncExternalStore`; contexts replace
  promises; `json.RawMessage` + `DecodeContent` replace the `channel<M>()`
  generic.
- Entry/item actions live on the `Inbox` handle (`MarkChannelRead`,
  `MarkItemRead`, `Mute`, `Unmute`) instead of on the row objects.
- The channel registry holds strong references (Go has no WeakRef); a
  long-lived process that touches unbounded channel ids should hold onto and
  reuse handles deliberately.
- Frames sent while the socket is down are dropped (partysocket buffers them);
  keepalive is periodic and read-state re-syncs from the next `ready`, so
  nothing user-visible is lost.
- Incoming ephemeral deliveries are surfaced via `OnEphemeral` (the JS client
  drops them). They never join `Messages()` — no seq, no ordering, no history —
  which makes the ephemeral lane usable end-to-end for live cursors and
  game-state streams.
- `channel.view(where)` is reserved in v1 upstream (typed but rejected); here
  it is likewise present and always returns `IsNotYetSupported`.

## Examples

A terminal chat client lives in [`examples/chat`](examples/chat/main.go) —
`-mock` runs it against a built-in mock server with a bot, no account needed:

```sh
go run ./examples/chat -mock
go run ./examples/chat -key pk_live_… -channel hello-world
```

[`examples/tankwar`](examples/tankwar) is a Battle City-inspired 2D
multiplayer war game on [Ebitengine](https://ebitengine.org), with all netcode
running through this SDK — type your name, spawn at a random spot, drive with
arrows/WASD, shoot with space; every kill is +10 on the live leaderboard, and
getting shot means a 3s respawn:

```sh
go run ./examples/tankwar                      # solo sandbox (in-process relay)
go run ./examples/tankwar -host :8089          # host a LAN match and play
go run ./examples/tankwar -addr 192.168.1.20:8089   # join that match
go run ./examples/tankwar -serve :8089         # headless relay only
go run ./examples/tankwar -key pk_live_…       # over the real Portal service
```

The bundled relay speaks the Portal wire protocol (built on the `wire`
package), so every match exercises the same SDK code path as the hosted
service: movement streams over the ephemeral lane (fire-and-forget WebSocket
frames + dead reckoning, so lag never queues up), shots and kills are
reliable publishes with shooter-authoritative `hit` events, and late joiners
discover the field from 1s heartbeats.

## Testing

```sh
go test ./...                       # SDK + wire protocol
go test -C examples/tankwar ./...   # game rules, relay, interpolation
```

Unit tests cover the frame parsers, the message buffer (ordering, dedup, gaps,
tombstones, watermarks, optimistic lifecycle) and the where-filter grammar. An
integration suite drives the full client against an in-process mock Portal
server (WebSocket + HTTP) covering connect/ready, publish + echo dedup,
middleware rejection with rollback, terminal refusals, live gap-fill, retract
tombstones, watermarks, ephemeral sends, and the inbox.

## License

MIT
