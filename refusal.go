package portal

import "github.com/Jibaru/portal-go/wire"

// refusalDecision classifies an upgrade refusal into the SDK's reaction:
// token-expired (retry once with a fresh credential) or terminal (blocked).
type refusalDecision struct {
	tokenExpired bool
	err          *Error
}

func invalidAPIKeyMessage(reason string) string {
	if reason != "" {
		return reason
	}
	return "The apiKey was rejected. Pass your publishable key, not a secret key — a secret key must never ship in client code."
}

// classifyRefusal maps a refusal code (§1.1) to its public error and reaction.
// An unrecognised code is terminal, surfaced as a base *Error carrying the wire
// code so callers can still branch on it.
func classifyRefusal(code, reason string) refusalDecision {
	if !wire.IsRefusalCode(code) {
		return refusalDecision{err: &Error{Code: code, Message: withReason("The connection was refused", reason)}}
	}
	switch wire.RefusalCode(code) {
	case wire.RefusalTokenExpired:
		return refusalDecision{
			tokenExpired: true,
			err:          newError(CodeTokenExpired, withReason("The token has expired", reason)),
		}
	case wire.RefusalInvalidAPIKey:
		return refusalDecision{err: newError(CodeInvalidAPIKey, invalidAPIKeyMessage(reason))}
	case wire.RefusalNotMember:
		return refusalDecision{err: newError(CodeNotMember, withReason("You are not a member of this channel", reason))}
	case wire.RefusalAnonymousNotAllowed:
		return refusalDecision{err: newError(CodeAnonymousNotAllowed, withReason("This channel does not allow anonymous access", reason))}
	case wire.RefusalChannelAtCapacity:
		return refusalDecision{err: newError(CodeChannelAtCapacity, withReason("The channel is at capacity", reason))}
	// No dedicated public code in §8: surfaced as a base *Error carrying the wire code.
	case wire.RefusalInvalidToken:
		return refusalDecision{err: &Error{Code: code, Message: withReason("The token was rejected", reason)}}
	case wire.RefusalBanned:
		return refusalDecision{err: &Error{Code: code, Message: withReason("You are banned from this channel", reason)}}
	case wire.RefusalUnknownChannel:
		return refusalDecision{err: &Error{Code: code, Message: withReason("No such channel", reason)}}
	default: // wire.RefusalUnsupportedVersion
		return refusalDecision{err: &Error{Code: code, Message: withReason("The server does not support this protocol version", reason)}}
	}
}

func refusalError(code, reason string) *Error {
	return classifyRefusal(code, reason).err
}
