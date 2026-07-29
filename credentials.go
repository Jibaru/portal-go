package portal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// expirySkew renews the anonymous credential this long before its recorded
// expiry, so it never arrives at the server already dead.
const expirySkew = 30 * time.Second

// jwtClaims are the two registered claims the SDK reads off its own anonymous
// token — best-effort, unverified (verification is the server's job).
type jwtClaims struct {
	sub string
	exp int64 // epoch seconds; 0 = absent
}

func decodeJWTClaims(token string) jwtClaims {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return jwtClaims{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Tolerate padded payloads.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return jwtClaims{}
		}
	}
	var claims struct {
		Sub string  `json:"sub"`
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return jwtClaims{}
	}
	return jwtClaims{sub: claims.Sub, exp: int64(claims.Exp)}
}

type anonCredential struct {
	token     string
	anonID    string
	expiresAt time.Time // zero = no recorded expiry
}

// credentials owns the bearer token for every connection and HTTP call.
//
// Three modes, mirroring @portalsdk/core:
//   - user callback (TokenFunc): re-resolved on every use; refresh is the app's job.
//   - user static (Token): used as-is; cannot be re-resolved on expiry.
//   - managed (neither): the SDK mints an anonymous credential on first use and
//     keeps one stable anonymous identity (anonId) across refreshes.
type credentials struct {
	mu         sync.Mutex
	apiKey     string
	tokenFn    TokenFunc
	static     string
	hasStatic  bool
	anon       *anonCredential
	mintFlight *mintFlight
	mintClient *httpClient
}

// mintFlight is the single-flight cell for an in-progress mint.
type mintFlight struct {
	done  chan struct{}
	token string
	err   error
}

func newCredentials(h hosts, apiKey string, static string, hasStatic bool, fn TokenFunc, httpc *http.Client) *credentials {
	return &credentials{
		apiKey:    apiKey,
		tokenFn:   fn,
		static:    static,
		hasStatic: hasStatic,
		mintClient: &httpClient{
			baseURL: h.apiURL,
			apiKey:  apiKey,
			// The mint route authenticates by apiKey only and never resolves a
			// bearer token.
			token: func(ctx context.Context) (string, error) {
				return "", newError("internal", "the mint route does not use a bearer token")
			},
			client: httpc,
		},
	}
}

// managed reports whether the SDK owns the credential (anonymous mode); expiry
// is handled internally.
func (c *credentials) managed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokenFn == nil && !c.hasStatic
}

// userStatic reports whether the user supplied a static string token (which
// cannot be re-resolved).
func (c *credentials) userStatic() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokenFn == nil && c.hasStatic
}

// resolve returns the current bearer token, minting or refreshing the anonymous
// credential as needed.
func (c *credentials) resolve(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.tokenFn != nil {
		fn := c.tokenFn
		c.mu.Unlock()
		return fn(ctx)
	}
	if c.hasStatic {
		token := c.static
		c.mu.Unlock()
		return token, nil
	}
	if anon := c.anon; anon != nil {
		if anon.expiresAt.IsZero() || time.Until(anon.expiresAt) > expirySkew {
			token := anon.token
			c.mu.Unlock()
			return token, nil
		}
	}
	return c.mintLocked(ctx)
}

// invalidate expires the cached anonymous token so the next resolve re-mints —
// keeping the anonId so the identity is stable across the refresh. No-op in
// user-token mode. Called when the server reports the credential expired.
func (c *credentials) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokenFn == nil && !c.hasStatic && c.anon != nil {
		c.anon.expiresAt = time.Unix(0, 1)
	}
}

// setToken replaces the token source (static string form). Reports whether the
// identity changed — a change means active connections must re-authenticate.
func (c *credentials) setToken(token string, has bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := c.tokenFn != nil || c.hasStatic != has || c.static != token
	c.tokenFn = nil
	c.static = token
	c.hasStatic = has
	if has {
		c.anon = nil
	}
	return changed
}

// setTokenFunc replaces the token source (callback form). Always treated as an
// identity change (callbacks cannot be compared for equality in Go).
func (c *credentials) setTokenFunc(fn TokenFunc) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokenFn = fn
	c.static = ""
	c.hasStatic = false
	if fn != nil {
		c.anon = nil
	}
	return true
}

// mintLocked mints (or joins an in-flight mint of) the anonymous token. Called
// with c.mu held; releases it before any network I/O.
func (c *credentials) mintLocked(ctx context.Context) (string, error) {
	if flight := c.mintFlight; flight != nil {
		c.mu.Unlock()
		select {
		case <-flight.done:
			return flight.token, flight.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	flight := &mintFlight{done: make(chan struct{})}
	c.mintFlight = flight
	anonID := ""
	if c.anon != nil {
		anonID = c.anon.anonID
	}
	client := c.mintClient
	c.mu.Unlock()

	token, err := func() (string, error) {
		outcome, err := client.mintAnonymousToken(ctx, anonID)
		if err != nil {
			return "", &Error{Code: CodeMintFailed, Message: "failed to obtain an anonymous token: " + err.Error()}
		}
		if !outcome.ok {
			return "", refusalError(outcome.code, outcome.reason)
		}
		return outcome.token, nil
	}()

	c.mu.Lock()
	if err == nil {
		claims := decodeJWTClaims(token)
		next := &anonCredential{token: token, anonID: anonID}
		if claims.sub != "" {
			next.anonID = claims.sub
		}
		if claims.exp != 0 {
			next.expiresAt = time.Unix(claims.exp, 0)
		}
		c.anon = next
	}
	if c.mintFlight == flight {
		c.mintFlight = nil
	}
	c.mu.Unlock()

	flight.token, flight.err = token, err
	close(flight.done)
	return token, err
}
