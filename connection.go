package portal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/Jibaru/portal-go/wire"
)

const (
	gapFillMaxJitter = 2 * time.Second
	activityThrottle = 3 * time.Second
	activityExpiry   = 5 * time.Second
	keepaliveEvery   = 25 * time.Second
)

// channelEvents are the per-channel listener registries (the `on(...)` surface).
type channelEvents struct {
	message  listeners[func(Message)]
	mention  listeners[func(Message)]
	retract  listeners[func(string)]
	presence listeners[func(Presence)]
	activity listeners[func([]ActivityEntry)]
	status   listeners[func(ChannelStatus, error)]
	store    listeners[func()]
}

type channelDeps struct {
	channelID   string
	hosts       hosts
	apiKey      string
	credentials *credentials
	httpc       *http.Client
	metadata    map[string]any
	// history is the initial backfill size; historyNone means live-only start.
	history     int
	historyNone bool
}

// channelConnection is the Go port of @portalsdk/core's ChannelConnection: it
// owns the socket, the message buffer, presence, activity, read state and the
// HTTP write plane for one channel.
//
// All mutable state is guarded by mu. Listener callbacks are always invoked
// with mu released, so callbacks may freely call back into the SDK.
type channelConnection struct {
	mu sync.Mutex

	deps     channelDeps
	events   channelEvents
	buffer   *messageBuffer
	presence *presenceTracker

	socket *transport
	http   *httpClient
	ctx    context.Context
	cancel context.CancelFunc

	status   ChannelStatus
	info     *ChannelInfo
	me       *Me
	ext      map[string]json.RawMessage
	lastErr  error
	disposed bool

	// leaf is the sticky reconnect hint from the last ready; echoed on the next upgrade.
	leaf string
	// tokenRetryUsed: whether this session's one token-refresh retry has been spent.
	tokenRetryUsed bool
	// bindings maps extension namespace → transport ("ws"/"http"), from ready.bindings.
	bindings map[string]string
	// degraded holds namespaces whose extension is currently degraded.
	degraded map[string]struct{}
	// canPublish drives the degraded-http fallback status.
	canPublish bool
	// metadata is the current presence metadata; re-sent on reconnect and
	// replaced by SetMetadata.
	metadata map[string]any

	clientTag int

	loadingPrevious bool
	loadPrevious    *loadPreviousFlight
	inflightGaps    map[string]struct{}

	// activity holds live peer activity, keyed "userId:kind", each on its own
	// absence-expiry timer.
	activity map[string]activityCell
	// activityThrottle records the last send time per activity kind.
	activityThrottleAt map[string]time.Time

	keepaliveStop chan struct{}
}

type activityCell struct {
	entry ActivityEntry
	timer *time.Timer
}

type loadPreviousFlight struct {
	done    chan struct{}
	hasMore bool
	err     error
}

func newChannelConnection(deps channelDeps) *channelConnection {
	return &channelConnection{
		deps:               deps,
		buffer:             newMessageBuffer(deps.channelID),
		presence:           newPresenceTracker(),
		status:             StatusIdle,
		metadata:           deps.metadata,
		inflightGaps:       map[string]struct{}{},
		activity:           map[string]activityCell{},
		activityThrottleAt: map[string]time.Time{},
		degraded:           map[string]struct{}{},
	}
}

// ── Lifecycle ─────────────────────────────────────────────

func (c *channelConnection) connect() {
	c.mu.Lock()
	if c.socket != nil {
		c.mu.Unlock()
		return
	}
	c.disposed = false
	c.tokenRetryUsed = false
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx, c.cancel = ctx, cancel
	c.status = StatusConnecting
	c.socket = newTransport(c.buildURL, c.onEvent)
	managed := c.deps.credentials.managed()
	backfill := !c.deps.historyNone
	limit := c.deps.history
	c.mu.Unlock()

	c.emitStatus(StatusConnecting, nil)
	if backfill {
		go c.backfill(ctx, limit)
	}
	if managed {
		// Surface a mint failure eagerly instead of letting the socket retry
		// against a credential that will never exist.
		go func() {
			if _, err := c.deps.credentials.resolve(ctx); err != nil && ctx.Err() == nil {
				var perr *Error
				if !errors.As(err, &perr) {
					perr = newError(CodeMintFailed, "Failed to obtain an anonymous token.")
				}
				c.fail(perr)
			}
		}()
	}
}

func (c *channelConnection) teardown() {
	c.mu.Lock()
	c.disposed = true
	if c.cancel != nil {
		c.cancel()
	}
	socket := c.socket
	c.socket = nil
	c.http = nil
	c.leaf = ""
	c.bindings = nil
	c.canPublish = false
	c.metadata = c.deps.metadata
	c.loadingPrevious = false
	c.loadPrevious = nil
	c.inflightGaps = map[string]struct{}{}
	c.stopKeepaliveLocked()
	for _, cell := range c.activity {
		cell.timer.Stop()
	}
	c.activity = map[string]activityCell{}
	c.activityThrottleAt = map[string]time.Time{}
	c.buffer.reset()
	c.presence.reset()
	c.status = StatusIdle
	c.info = nil
	c.me = nil
	c.ext = nil
	c.lastErr = nil
	c.mu.Unlock()
	if socket != nil {
		socket.close()
	}
	c.publishState()
}

// ── URL construction ──────────────────────────────────────

func (c *channelConnection) buildURL(ctx context.Context) (string, error) {
	token, err := c.deps.credentials.resolve(ctx)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	leaf := c.leaf
	metadata := c.metadata
	last := c.buffer.contiguousSeq()
	c.mu.Unlock()

	q := url.Values{}
	q.Set(wire.ParamVersion, strconv.Itoa(wire.ProtocolVersion))
	q.Set(wire.ParamToken, token)
	q.Set(wire.ParamKey, c.deps.apiKey)
	if leaf != "" {
		q.Set(wire.ParamLeaf, leaf)
	}
	if metadata != nil {
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return "", err
		}
		q.Set(wire.ParamMeta, base64.StdEncoding.EncodeToString(encoded))
	}
	if last != nil {
		q.Set(wire.ParamLast, strconv.FormatInt(*last, 10))
	}
	return c.deps.hosts.realtimeURL + "/v1/channels/" + url.PathEscape(c.deps.channelID) + "?" + q.Encode(), nil
}

// ── Event handling ────────────────────────────────────────

func (c *channelConnection) onEvent(event socketEvent) {
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
		var next ChannelStatus
		if c.status != StatusBlocked {
			if c.canPublish {
				next = StatusDegradedHTTP
			} else {
				next = StatusReconnecting
			}
		}
		c.mu.Unlock()
		if next != "" {
			c.setStatus(next)
		}
	}
}

func (c *channelConnection) onMessage(raw []byte) {
	frame := wire.ParseChannelFrame(raw)
	if frame == nil {
		return
	}
	switch f := frame.(type) {
	case *wire.ChannelReadyFrame:
		c.onReady(f)
	case *wire.BatchFrame:
		c.deliver(f.Msgs)
	case *wire.DirectFrame:
		c.deliver([]wire.Message{f.Msg})
	case *wire.RetractFrame:
		c.onRetract(f.ID, f.Seq)
	case *wire.ErrorFrame:
		c.emitInSessionError(f.Code, f.Reason)
	case *wire.ActivityFrame:
		c.onActivity(f.UserID, f.Kind, f.Since)
	case *wire.PresenceFrame:
		c.onPresence(f)
	case *wire.ReassignFrame:
		c.mu.Lock()
		c.leaf = f.Leaf
		socket := c.socket
		c.mu.Unlock()
		if socket != nil {
			socket.reconnect()
		}
	}
}

func (c *channelConnection) onPresence(frame *wire.PresenceFrame) {
	c.mu.Lock()
	c.presence.applyDelta(frame)
	presence := c.presence.current()
	c.mu.Unlock()
	c.publishState()
	if presence != nil {
		for _, fn := range c.events.presence.snapshot() {
			fn(*presence)
		}
	}
}

func (c *channelConnection) onReady(frame *wire.ChannelReadyFrame) {
	c.mu.Lock()
	c.leaf = frame.Leaf
	c.bindings = frame.Bindings
	c.canPublish = frame.Me.Capabilities.Publish()
	c.tokenRetryUsed = false
	heldBefore := c.buffer.contiguousSeq()
	c.buffer.setMe(frame.Me.ID, frame.Me.Anon)
	c.buffer.setBaseline(frame.Seq)
	if frame.Watermark != nil {
		c.buffer.setWatermark(*frame.Watermark)
	} else {
		c.buffer.setWatermark(frame.Seq)
	}
	c.presence.seed(frame.Presence)
	var gaps []gapRange
	if heldBefore != nil && frame.Seq > *heldBefore {
		gaps = []gapRange{{from: *heldBefore + 1, to: frame.Seq}}
	}
	c.info = &ChannelInfo{
		ID:   frame.Channel.ID,
		Mode: string(frame.Channel.Mode),
		Name: frame.Channel.Name,
		Meta: json.RawMessage(frame.Channel.Meta),
	}
	c.me = &Me{ID: frame.Me.ID, Anon: frame.Me.Anon, Claims: frame.Me.Claims}
	// Replaced wholesale, never merged: a handle absent from this frame is a
	// handle whose extension is degraded or detached, and a stale blob would
	// read as live state.
	if frame.Ext != nil {
		ext := make(map[string]json.RawMessage, len(frame.Ext))
		for k, v := range frame.Ext {
			ext[k] = json.RawMessage(v)
		}
		c.ext = ext
	} else {
		c.ext = nil
	}
	c.status = StatusReady
	presence := c.presence.current()
	c.mu.Unlock()

	c.scheduleGapFills(gaps)
	c.publishState()
	if presence != nil {
		for _, fn := range c.events.presence.snapshot() {
			fn(*presence)
		}
	}
	for _, fn := range c.events.status.snapshot() {
		fn(StatusReady, nil)
	}
}

func (c *channelConnection) deliver(msgs []wire.Message) {
	c.mu.Lock()
	delivered, gaps := c.buffer.ingest(msgs)
	var meID string
	if c.me != nil {
		meID = c.me.ID
	}
	c.mu.Unlock()

	for _, msg := range delivered {
		for _, fn := range c.events.message.snapshot() {
			fn(msg)
		}
		if meID != "" {
			for _, mention := range msg.Mentions {
				if mention.UserID == meID {
					for _, fn := range c.events.mention.snapshot() {
						fn(msg)
					}
					break
				}
			}
		}
	}
	c.publishState()
	c.scheduleGapFills(gaps)
}

func (c *channelConnection) onRetract(id string, seq int64) {
	c.mu.Lock()
	c.buffer.retract(seq)
	c.mu.Unlock()
	for _, fn := range c.events.retract.snapshot() {
		fn(id)
	}
	c.publishState()
}

func (c *channelConnection) onRefused(code, reason string) {
	decision := classifyRefusal(code, reason)
	if decision.tokenExpired {
		credentials := c.deps.credentials
		c.mu.Lock()
		retryUsed := c.tokenRetryUsed
		socket := c.socket
		c.mu.Unlock()
		if credentials.managed() {
			if retryUsed {
				c.setStatus(StatusReconnecting)
				return
			}
			c.setTokenRetryUsed()
			credentials.invalidate()
			if socket != nil {
				socket.reconnect()
			}
			return
		}
		if credentials.userStatic() || retryUsed {
			c.fail(decision.err)
			return
		}
		c.setTokenRetryUsed()
		if socket != nil {
			socket.reconnect()
		}
		return
	}
	c.fail(decision.err)
}

func (c *channelConnection) setTokenRetryUsed() {
	c.mu.Lock()
	c.tokenRetryUsed = true
	c.mu.Unlock()
}

// ── Sending ───────────────────────────────────────────────

func (c *channelConnection) send(ctx context.Context, input SendInput) (SendAck, error) {
	if input.Kind != "" && input.Kind != "text" {
		return SendAck{}, newError(CodeNotYetSupported,
			fmt.Sprintf("media kind %q is reserved and not supported in v1", input.Kind))
	}
	if route, ok := c.extensionRoute(input.Type); ok {
		c.mu.Lock()
		_, isDegraded := c.degraded[route.namespace]
		c.mu.Unlock()
		if isDegraded {
			return SendAck{}, newError(CodeDegraded, fmt.Sprintf("The %q extension is degraded.", route.namespace))
		}
		if route.transport == "ws" {
			return c.sendEphemeralFrame(input.Type, input.Content)
		}
		return c.publishOnce(ctx, input)
	}
	if input.Ephemeral {
		return c.sendEphemeralFrame(input.Type, input.Content)
	}
	return c.sendPersistent(ctx, input)
}

func (c *channelConnection) sendPersistent(ctx context.Context, input SendInput) (SendAck, error) {
	content, err := json.Marshal(input.Content)
	if err != nil {
		return SendAck{}, err
	}
	msgType := input.Type
	if msgType == "" {
		msgType = "message"
	}
	tempID := c.nextTag()
	c.mu.Lock()
	c.buffer.addOptimistic(optimisticSend{
		tempID:    tempID,
		msgType:   msgType,
		content:   content,
		to:        input.To,
		mentions:  input.Mentions,
		timestamp: time.Now().UnixMilli(),
	})
	c.mu.Unlock()
	c.publishState()

	outcome, err := c.httpClient().publish(ctx, c.deps.channelID, c.body(input))
	if err != nil {
		c.mu.Lock()
		c.buffer.rollback(tempID)
		c.mu.Unlock()
		c.publishState()
		return SendAck{}, newError(CodeNetworkError, "The publish request failed: "+err.Error())
	}
	if !outcome.ok {
		c.mu.Lock()
		c.buffer.rollback(tempID)
		c.mu.Unlock()
		c.publishState()
		return SendAck{}, publishError(outcome.code, outcome.reason)
	}
	c.mu.Lock()
	c.buffer.ack(tempID, outcome.ack)
	c.mu.Unlock()
	c.publishState()
	return SendAck{ID: outcome.ack.ID, Timestamp: outcome.ack.Timestamp}, nil
}

// publishOnce is an HTTP-routed extension send: a publish with no optimistic
// channel-message insert.
func (c *channelConnection) publishOnce(ctx context.Context, input SendInput) (SendAck, error) {
	outcome, err := c.httpClient().publish(ctx, c.deps.channelID, c.body(input))
	if err != nil {
		return SendAck{}, newError(CodeNetworkError, "The publish request failed: "+err.Error())
	}
	if !outcome.ok {
		return SendAck{}, publishError(outcome.code, outcome.reason)
	}
	return SendAck{ID: outcome.ack.ID, Timestamp: outcome.ack.Timestamp}, nil
}

func (c *channelConnection) sendEphemeralFrame(msgType string, content any) (SendAck, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return SendAck{}, err
	}
	if msgType == "" {
		msgType = "message"
	}
	cl := c.nextTag()
	frame := &wire.EphemeralFrame{Cl: cl, Type: msgType, Content: wire.RawJSON(encoded)}
	c.sendFrame(frame)
	return SendAck{ID: cl, Timestamp: time.Now().UnixMilli()}, nil
}

// ── Read state ────────────────────────────────────────────

// markAsRead advances the channel watermark to the head, clearing unread.
func (c *channelConnection) markAsRead() {
	c.mu.Lock()
	head, ok := c.buffer.headSeq()
	if !ok {
		c.mu.Unlock()
		return
	}
	c.buffer.setWatermark(head)
	c.mu.Unlock()
	c.sendFrame(&wire.WatermarkFrame{Seq: head})
	c.publishState()
}

// ── Activity ──────────────────────────────────────────────

func (c *channelConnection) sendActivity(kind string) {
	c.mu.Lock()
	if c.info != nil && c.info.Mode == string(wire.ModeBroadcast) {
		c.mu.Unlock()
		return
	}
	now := time.Now()
	if last, ok := c.activityThrottleAt[kind]; ok && now.Sub(last) < activityThrottle {
		c.mu.Unlock()
		return
	}
	c.activityThrottleAt[kind] = now
	c.mu.Unlock()
	c.sendFrame(&wire.ActivityUpFrame{Kind: kind})
}

func (c *channelConnection) onActivity(userID, kind string, since int64) {
	key := userID + ":" + kind
	c.mu.Lock()
	if existing, ok := c.activity[key]; ok {
		existing.timer.Stop()
	}
	timer := time.AfterFunc(activityExpiry, func() {
		c.mu.Lock()
		delete(c.activity, key)
		entries := c.activityEntriesLocked()
		c.mu.Unlock()
		c.publishState()
		for _, fn := range c.events.activity.snapshot() {
			fn(entries)
		}
	})
	c.activity[key] = activityCell{entry: ActivityEntry{UserID: userID, Kind: kind, Since: since}, timer: timer}
	entries := c.activityEntriesLocked()
	c.mu.Unlock()
	c.publishState()
	for _, fn := range c.events.activity.snapshot() {
		fn(entries)
	}
}

func (c *channelConnection) activityEntriesLocked() []ActivityEntry {
	entries := make([]ActivityEntry, 0, len(c.activity))
	for _, cell := range c.activity {
		entries = append(entries, cell.entry)
	}
	return entries
}

// ── Presence metadata ─────────────────────────────────────

// setMetadata replaces this session's presence metadata; the server re-announces
// it via presence deltas. Presentation only — never authz.
func (c *channelConnection) setMetadata(metadata map[string]any) {
	c.mu.Lock()
	c.metadata = metadata
	c.mu.Unlock()
	c.sendFrame(&wire.MetaFrame{Metadata: metadata})
}

// ── Members ───────────────────────────────────────────────

// members fetches the full member directory, following the pagination cursor.
func (c *channelConnection) members(ctx context.Context) ([]MemberRow, error) {
	var rows []MemberRow
	cursor := ""
	for {
		page, err := c.httpClient().members(ctx, c.deps.channelID, cursor)
		if err != nil {
			return nil, err
		}
		for _, row := range page.Members {
			rows = append(rows, MemberRow{UserID: row.UserID, Online: row.Online, Claims: row.Claims})
		}
		if page.Cursor == "" {
			return rows, nil
		}
		cursor = page.Cursor
	}
}

// ── History ───────────────────────────────────────────────

func (c *channelConnection) loadPreviousPage(ctx context.Context) (bool, error) {
	c.mu.Lock()
	if flight := c.loadPrevious; flight != nil {
		c.mu.Unlock()
		select {
		case <-flight.done:
			return flight.hasMore, flight.err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if !c.buffer.hasPrevious {
		c.mu.Unlock()
		return false, nil
	}
	flight := &loadPreviousFlight{done: make(chan struct{})}
	c.loadPrevious = flight
	c.loadingPrevious = true
	pageSize := c.deps.history
	if c.deps.historyNone {
		pageSize = 50
	}
	before := c.buffer.lowestSeq()
	fetchCtx := c.ctx
	c.mu.Unlock()
	c.publishState()

	if fetchCtx == nil {
		fetchCtx = context.Background()
	}
	page, err := c.httpClient().history(fetchCtx, c.deps.channelID, historyQuery{before: before, limit: &pageSize})

	c.mu.Lock()
	if err == nil {
		c.buffer.ingestHistory(page.Msgs)
		c.buffer.hasPrevious = page.HasMore
		flight.hasMore = page.HasMore
	} else {
		flight.err = err
	}
	c.loadingPrevious = false
	c.loadPrevious = nil
	c.mu.Unlock()
	close(flight.done)
	c.publishState()
	return flight.hasMore, flight.err
}

func (c *channelConnection) backfill(ctx context.Context, limit int) {
	page, err := c.httpClient().history(ctx, c.deps.channelID, historyQuery{limit: &limit})
	if err != nil {
		return
	}
	c.mu.Lock()
	if c.disposed {
		c.mu.Unlock()
		return
	}
	c.buffer.ingestHistory(page.Msgs)
	c.buffer.hasPrevious = page.HasMore
	c.mu.Unlock()
	c.publishState()
}

func (c *channelConnection) scheduleGapFills(gaps []gapRange) {
	c.mu.Lock()
	ctx := c.ctx
	var scheduled []gapRange
	for _, gap := range gaps {
		key := fmt.Sprintf("%d-%d", gap.from, gap.to)
		if _, inflight := c.inflightGaps[key]; inflight {
			continue
		}
		c.inflightGaps[key] = struct{}{}
		scheduled = append(scheduled, gap)
	}
	c.mu.Unlock()
	for _, gap := range scheduled {
		gap := gap
		jitter := time.Duration(rand.Int63n(int64(gapFillMaxJitter)))
		time.AfterFunc(jitter, func() { c.fillGap(ctx, gap) })
	}
}

func (c *channelConnection) fillGap(ctx context.Context, gap gapRange) {
	key := fmt.Sprintf("%d-%d", gap.from, gap.to)
	defer func() {
		c.mu.Lock()
		delete(c.inflightGaps, key)
		c.mu.Unlock()
	}()
	if ctx == nil || ctx.Err() != nil {
		return
	}
	page, err := c.httpClient().history(ctx, c.deps.channelID, historyQuery{from: &gap.from, to: &gap.to})
	if err != nil {
		return
	}
	c.mu.Lock()
	if c.disposed {
		c.mu.Unlock()
		return
	}
	c.buffer.ingestHistory(page.Msgs)
	c.mu.Unlock()
	c.publishState()
}

// ── Keepalive ─────────────────────────────────────────────

func (c *channelConnection) startKeepaliveLocked() {
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

func (c *channelConnection) stopKeepaliveLocked() {
	if c.keepaliveStop != nil {
		close(c.keepaliveStop)
		c.keepaliveStop = nil
	}
}

// ── Helpers ───────────────────────────────────────────────

type extensionRoute struct {
	namespace string
	transport string // "ws" | "http"
}

func (c *channelConnection) extensionRoute(msgType string) (extensionRoute, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if msgType == "" || c.bindings == nil {
		return extensionRoute{}, false
	}
	for namespace, transport := range c.bindings {
		if len(msgType) >= len(namespace) && msgType[:len(namespace)] == namespace {
			if transport != "ws" {
				transport = "http"
			}
			return extensionRoute{namespace: namespace, transport: transport}, true
		}
	}
	return extensionRoute{}, false
}

func (c *channelConnection) body(input SendInput) wire.PublishBody {
	return wire.PublishBody{
		Content:  input.Content,
		Type:     input.Type,
		Kind:     input.Kind,
		To:       input.To,
		Mentions: toWireMentions(input.Mentions),
	}
}

func publishError(code, reason string) *Error {
	if code == wire.PublishBlockedByMiddleware {
		if reason == "" {
			reason = "The message was blocked."
		}
		return blockedError(reason)
	}
	message := reason
	if message == "" {
		message = "The message was rejected."
	}
	return &Error{Code: code, Message: message}
}

func (c *channelConnection) sendFrame(frame wire.Frame) {
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

func (c *channelConnection) httpClient() *httpClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.http == nil {
		c.http = &httpClient{
			baseURL: c.deps.hosts.realtimeHTTPURL,
			apiKey:  c.deps.apiKey,
			token:   c.deps.credentials.resolve,
			client:  c.deps.httpc,
		}
	}
	return c.http
}

func (c *channelConnection) nextTag() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clientTag++
	return "cl_" + strconv.Itoa(c.clientTag)
}

// snapshot builds a point-in-time public copy of the state.
func (c *channelConnection) snapshot() ChannelSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ChannelSnapshot{
		Messages:          c.buffer.messages(),
		Presence:          c.presence.current(),
		Activity:          c.activityEntriesLocked(),
		Status:            c.status,
		Unread:            c.buffer.channelUnread(),
		Info:              c.info,
		Me:                c.me,
		Ext:               c.ext,
		IsLoadingPrevious: c.loadingPrevious,
		HasPrevious:       c.buffer.hasPrevious,
	}
}

// publishState notifies store subscribers that the snapshot changed.
func (c *channelConnection) publishState() {
	for _, fn := range c.events.store.snapshot() {
		fn()
	}
}

// emitInSessionError delivers an in-session error through the status event's
// error argument — the only error-carrying event in the contract — without
// changing the status value.
func (c *channelConnection) emitInSessionError(code, reason string) {
	var err *Error
	if code == wire.PublishBlockedByMiddleware {
		if reason == "" {
			reason = "The message was blocked."
		}
		err = blockedError(reason)
	} else {
		message := reason
		if message == "" {
			message = "The request was rejected."
		}
		err = &Error{Code: code, Message: message}
	}
	c.mu.Lock()
	status := c.status
	c.mu.Unlock()
	for _, fn := range c.events.status.snapshot() {
		fn(status, err)
	}
}

func (c *channelConnection) fail(err *Error) {
	c.mu.Lock()
	c.stopKeepaliveLocked()
	socket := c.socket
	c.status = StatusBlocked
	c.lastErr = err
	c.mu.Unlock()
	if socket != nil {
		socket.close()
	}
	c.publishState()
	for _, fn := range c.events.status.snapshot() {
		fn(StatusBlocked, err)
	}
}

func (c *channelConnection) setStatus(status ChannelStatus) {
	c.mu.Lock()
	if c.status == status {
		c.mu.Unlock()
		return
	}
	c.status = status
	c.mu.Unlock()
	c.publishState()
	c.emitStatus(status, nil)
}

func (c *channelConnection) emitStatus(status ChannelStatus, err error) {
	for _, fn := range c.events.status.snapshot() {
		fn(status, err)
	}
}
