package portal

import (
	"context"
	"net/http"
	"strings"
)

// Production hosts, baked in exactly as in @portalsdk/core.
const (
	defaultAPIURL      = "https://api.useportal.co"
	defaultRealtimeURL = "wss://realtime.useportal.co"
)

// TokenFunc resolves the current signed user token. It is re-invoked on connect,
// reconnect, and expiry (recommended over a static token).
type TokenFunc func(ctx context.Context) (string, error)

// Config configures a Client. Only APIKey is required.
type Config struct {
	// APIKey is the publishable key identifying the app (pk_…); safe to embed.
	APIKey string

	// Token is a static signed user token, used as-is (static or short-lived
	// sessions). Leave empty and set TokenFunc for refreshable tokens, or leave
	// both empty for anonymous mode: the SDK mints and manages its own anonymous
	// credential on first use, keeping one stable anonymous identity for the
	// lifetime of the Client. Supply a token later (e.g. on login) with
	// Client.SetToken / Client.SetTokenFunc.
	Token string

	// TokenFunc, when set, wins over Token and is re-invoked on connect,
	// reconnect and expiry.
	TokenFunc TokenFunc

	// APIURL and RealtimeURL override the baked-in production hosts — point them
	// at a local or mock server. Primarily for development and testing.
	APIURL      string
	RealtimeURL string

	// HTTPClient overrides the HTTP client used for publish/history/members/mint.
	// Defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// hosts is the resolved host set: the REST origin, the websocket origin, and the
// websocket origin's HTTP form (publish/history/members are served from the
// realtime host, not the API host — only the token mint hits APIURL).
type hosts struct {
	apiURL          string
	realtimeURL     string
	realtimeHTTPURL string
}

func trimTrailingSlash(url string) string {
	return strings.TrimRight(url, "/")
}

func wsToHTTPOrigin(url string) string {
	if rest, ok := strings.CutPrefix(url, "wss://"); ok {
		return "https://" + rest
	}
	if rest, ok := strings.CutPrefix(url, "ws://"); ok {
		return "http://" + rest
	}
	return url
}

func httpToWSOrigin(url string) string {
	if rest, ok := strings.CutPrefix(url, "https://"); ok {
		return "wss://" + rest
	}
	if rest, ok := strings.CutPrefix(url, "http://"); ok {
		return "ws://" + rest
	}
	return url
}

func resolveHosts(config Config) hosts {
	apiURL := config.APIURL
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	realtimeURL := config.RealtimeURL
	if realtimeURL == "" {
		realtimeURL = defaultRealtimeURL
	}
	realtimeURL = trimTrailingSlash(realtimeURL)
	return hosts{
		apiURL:          trimTrailingSlash(apiURL),
		realtimeURL:     realtimeURL,
		realtimeHTTPURL: wsToHTTPOrigin(realtimeURL),
	}
}
