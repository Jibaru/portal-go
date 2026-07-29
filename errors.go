package portal

import (
	"errors"
	"fmt"
)

// Error is the single error type the SDK surfaces (§8).
//
// Every failure carries a stable, machine-readable Code — safe to branch on with
// errors.As + a switch, or the Is* helpers below. Reason, when present, is
// end-user-visible copy (a middleware rejection message); render it in the
// send-rejection UX.
type Error struct {
	// Code is the stable discriminator (wire refusal codes, publish error codes,
	// or the SDK-local codes below).
	Code string
	// Reason is end-user-visible copy for blocked sends; empty otherwise.
	Reason string
	// Message is the developer-facing description.
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("portal: %s (%s)", e.Message, e.Code)
}

// Error codes surfaced by the SDK, beyond the wire-level refusal and publish
// codes (which pass through verbatim).
const (
	// CodeInvalidAPIKey — a bad or unknown apiKey (§1). Terminal: the connection
	// goes to StatusBlocked with no reconnect loop.
	CodeInvalidAPIKey = "invalid_api_key"
	// CodeBlocked — a gate or middleware refused the send (§4). Reason carries
	// end-user-visible copy.
	CodeBlocked = "blocked"
	// CodeTokenExpired — the token was rejected as expired (refusal or HTTP 401).
	// A callback token is re-resolved once and retried; a still-failing retry —
	// or a static string token, which cannot be re-resolved — surfaces this and
	// moves status to StatusBlocked.
	CodeTokenExpired = "token_expired"
	// CodeNotMember — a membership channel with no row for this user (on
	// connect, or on a to:-send).
	CodeNotMember = "not_member"
	// CodeChannelAtCapacity — the channel refused admission at its hard cap.
	CodeChannelAtCapacity = "channel_at_capacity"
	// CodeAnonymousNotAllowed — the channel is configured anonymous: false and
	// the token is anonymous.
	CodeAnonymousNotAllowed = "anonymous_not_allowed"
	// CodeNotYetSupported — a reserved surface was used in v1 (a where filter on
	// a channel, attachments, or a non-text media kind). Typed but rejected loudly.
	CodeNotYetSupported = "not_yet_supported"
	// CodeDegraded — a send into an extension namespace whose extension is
	// degraded; the channel keeps working.
	CodeDegraded = "degraded"
	// CodeNetworkError — the publish HTTP request itself failed.
	CodeNetworkError = "network_error"
	// CodeMintFailed — the SDK could not obtain an anonymous token.
	CodeMintFailed = "mint_failed"
)

// Helpers mirroring the named error subclasses of @portalsdk/core.

func codeIs(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// IsInvalidAPIKey reports an InvalidApiKeyError-equivalent failure.
func IsInvalidAPIKey(err error) bool { return codeIs(err, CodeInvalidAPIKey) }

// IsBlocked reports a BlockedError-equivalent failure (middleware refused the send).
func IsBlocked(err error) bool { return codeIs(err, CodeBlocked) }

// IsTokenExpired reports a TokenExpiredError-equivalent failure.
func IsTokenExpired(err error) bool { return codeIs(err, CodeTokenExpired) }

// IsNotMember reports a NotMemberError-equivalent failure.
func IsNotMember(err error) bool { return codeIs(err, CodeNotMember) }

// IsChannelAtCapacity reports a ChannelAtCapacityError-equivalent failure.
func IsChannelAtCapacity(err error) bool { return codeIs(err, CodeChannelAtCapacity) }

// IsAnonymousNotAllowed reports an AnonymousNotAllowedError-equivalent failure.
func IsAnonymousNotAllowed(err error) bool { return codeIs(err, CodeAnonymousNotAllowed) }

// IsNotYetSupported reports a NotYetSupportedError-equivalent failure.
func IsNotYetSupported(err error) bool { return codeIs(err, CodeNotYetSupported) }

// IsDegraded reports a DegradedError-equivalent failure.
func IsDegraded(err error) bool { return codeIs(err, CodeDegraded) }

func newError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func blockedError(reason string) *Error {
	return &Error{Code: CodeBlocked, Reason: reason, Message: reason}
}

func withReason(base, reason string) string {
	if reason == "" {
		return base
	}
	return base + ": " + reason
}
