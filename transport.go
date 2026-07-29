package portal

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Jibaru/portal-go/wire"
)

// Reconnect profile — the partysocket options used by @portalsdk/core, verbatim.
const (
	minReconnectDelay = 1 * time.Second
	maxReconnectDelay = 30 * time.Second
	reconnectGrow     = 1.5
	connectTimeout    = 10 * time.Second
	// minUptime: a connection must stay open this long before the backoff resets;
	// a flappy socket keeps its grown delay.
	minUptime = 5 * time.Second
)

type socketEventKind int

const (
	socketOpen socketEventKind = iota
	socketMessage
	socketClosed
	socketRefused
)

// socketEvent is what the transport reports upward. Exactly one of the payload
// fields is meaningful per kind: data for socketMessage, code/reason for
// socketRefused.
type socketEvent struct {
	kind   socketEventKind
	data   []byte
	code   string
	reason string
}

// transport is a reconnecting WebSocket: the Go equivalent of partysocket as
// configured by @portalsdk/core.
//
// The URL is re-built (token re-resolved) before every attempt. A handshake
// rejected with an HTTP error surfaces as socketRefused, carrying the code from
// the x-portal-error header or the {code, reason} body — the socket never opened,
// so this is disjoint from socketClosed (a drop after open, or a failed attempt
// with no readable refusal).
//
// Events are delivered from a single goroutine, in order.
type transport struct {
	buildURL func(ctx context.Context) (string, error)
	onEvent  func(socketEvent)

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	conn   *websocket.Conn
	closed bool
	// redial wakes the run loop out of its backoff sleep for an immediate retry.
	redial chan struct{}
}

func newTransport(buildURL func(ctx context.Context) (string, error), onEvent func(socketEvent)) *transport {
	ctx, cancel := context.WithCancel(context.Background())
	t := &transport{
		buildURL: buildURL,
		onEvent:  onEvent,
		ctx:      ctx,
		cancel:   cancel,
		redial:   make(chan struct{}, 1),
	}
	go t.run()
	return t
}

// send writes a text frame, best-effort: while the socket is down the frame is
// dropped (keepalive is periodic, activity is throttled, and read-state frames
// re-sync from the next ready snapshot).
func (t *transport) send(data []byte) {
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()
	if conn == nil {
		return
	}
	_ = conn.WriteMessage(websocket.TextMessage, data)
}

// reconnect force-cycles the connection immediately (fresh URL, fresh token),
// resetting the backoff — used for reassign frames and token-refresh retries.
func (t *transport) reconnect() {
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()
	select {
	case t.redial <- struct{}{}:
	default:
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// close tears the transport down permanently. No further events are delivered.
func (t *transport) close() {
	t.mu.Lock()
	t.closed = true
	conn := t.conn
	t.conn = nil
	t.mu.Unlock()
	t.cancel()
	if conn != nil {
		_ = conn.Close()
	}
}

func (t *transport) emit(event socketEvent) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if !closed {
		t.onEvent(event)
	}
}

func (t *transport) run() {
	delay := minReconnectDelay
	for {
		if t.ctx.Err() != nil {
			return
		}
		// Drain any stale redial signal so it can't skip the *next* backoff.
		select {
		case <-t.redial:
		default:
		}

		uptime := t.attempt()
		if t.ctx.Err() != nil {
			return
		}
		if uptime >= minUptime {
			delay = minReconnectDelay
		}

		// Backoff sleep, interruptible by an explicit reconnect() or teardown.
		select {
		case <-time.After(delay):
			delay = time.Duration(math.Min(float64(delay)*reconnectGrow, float64(maxReconnectDelay)))
		case <-t.redial:
			delay = minReconnectDelay
		case <-t.ctx.Done():
			return
		}
	}
}

// attempt performs one connect-read cycle. Returns how long the socket stayed
// open (0 if it never opened).
func (t *transport) attempt() (uptime time.Duration) {
	url, err := t.buildURL(t.ctx)
	if err != nil {
		// An unresolvable token is a transient failure at this layer: report a
		// close and let the backoff retry. Terminal causes (mint refusals) are
		// handled above the transport, which will close() us.
		t.emit(socketEvent{kind: socketClosed})
		return 0
	}

	dialer := &websocket.Dialer{HandshakeTimeout: connectTimeout}
	dialCtx, cancelDial := context.WithTimeout(t.ctx, connectTimeout)
	conn, resp, err := dialer.DialContext(dialCtx, url, nil)
	cancelDial()
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		if code, reason, ok := extractRefusal(resp); ok {
			t.emit(socketEvent{kind: socketRefused, code: code, reason: reason})
			return 0
		}
		t.emit(socketEvent{kind: socketClosed})
		return 0
	}

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = conn.Close()
		return 0
	}
	t.conn = conn
	t.mu.Unlock()

	openedAt := time.Now()
	t.emit(socketEvent{kind: socketOpen})

	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if kind == websocket.TextMessage {
			t.emit(socketEvent{kind: socketMessage, data: data})
		}
	}

	t.mu.Lock()
	if t.conn == conn {
		t.conn = nil
	}
	t.mu.Unlock()
	_ = conn.Close()

	t.emit(socketEvent{kind: socketClosed})
	return time.Since(openedAt)
}

// extractRefusal reads a refusal off a rejected upgrade response: the
// x-portal-error header first (it survives body-eating proxies), then the
// {code, reason} body (§1.1).
func extractRefusal(resp *http.Response) (code, reason string, ok bool) {
	if resp == nil {
		return "", "", false
	}
	var body wire.RefusalBody
	if resp.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if err == nil {
			_ = json.Unmarshal(raw, &body)
		}
	}
	if header := resp.Header.Get(wire.ErrorHeader); header != "" {
		return header, body.Reason, true
	}
	if body.Code != "" {
		return string(body.Code), body.Reason, true
	}
	return "", "", false
}
