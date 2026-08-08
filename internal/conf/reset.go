package conf

import (
	"errors"
	"strconv"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/floatdrop/moq-go/pkg/moqt"
)

// A data stream that a peer reset carries a reason, and the reasons are not
// interchangeable. Read as a bare error they all look like "the stream ended",
// which is what an ordinary FIN looks like too — so the one case worth acting
// on arrives indistinguishable from the common case that needs no action.
//
// The case worth acting on is the relay giving up on us. moq-go's relay holds
// a bounded send queue per subscriber and measures how long each forwarded
// object waits in it; once an object has waited longer than the relay's fanout
// lag window (2 s by default) it concludes we cannot keep up, resets our
// subgroup stream with TOO_FAR_BEHIND and terminates the subscription. That is
// not a transport failure and not the participant leaving. It is the one
// unambiguous statement anybody makes about our inbound capacity, and it
// arrives for free.

// streamReset reports the MOQT reset code a peer sent on a data stream.
//
// Only a remote reset counts: a stream this client cancelled itself surfaces
// the same types, and reporting our own teardown as a verdict from the relay
// would be worse than saying nothing.
//
// Both transports, because the app speaks both and they raise different types
// for the same event. Over WebTransport the reset arrives as an HTTP/3 stream
// error, which webtransport-go maps back to the application code the peer
// actually sent and re-raises as its own type — the same number, wearing a
// different coat. Decoding only the QUIC one meant every reset over a
// WebTransport relay read as an ordinary end of stream, so a subscriber there
// was never told it had been dropped for falling behind and never came back
// smaller: the freeze this exists to prevent, on the transport where it was
// hardest to notice.
func streamReset(err error) (moqt.StreamResetCode, bool) {
	var qErr *quic.StreamError
	if errors.As(err, &qErr) {
		if !qErr.Remote {
			return 0, false
		}
		return moqt.StreamResetCode(qErr.ErrorCode), true
	}

	var wtErr *webtransport.StreamError
	if errors.As(err, &wtErr) {
		if !wtErr.Remote {
			return 0, false
		}
		return moqt.StreamResetCode(wtErr.ErrorCode), true
	}
	return 0, false
}

// overloadReset reports whether a reset code means the relay stopped
// forwarding because this subscriber could not keep up with the live edge.
//
// TOO_FAR_BEHIND is the lag window: we were too slow for too long.
// EXCESSIVE_LOAD is the relay's coarser backstop on cumulative drops, and is
// off by default, but it means the same thing from where we stand.
func overloadReset(code moqt.StreamResetCode) bool {
	return code == moqt.StreamResetTooFarBehind || code == moqt.StreamResetExcessiveLoad
}

// resetName renders a §3.3.4 reset code for a log line. Codes this client has
// no reading of are rendered numerically rather than guessed at.
func resetName(code moqt.StreamResetCode) string {
	switch code {
	case moqt.StreamResetInternalError:
		return "INTERNAL_ERROR"
	case moqt.StreamResetCancelled:
		return "CANCELLED"
	case moqt.StreamResetDeliveryTimeout:
		return "DELIVERY_TIMEOUT"
	case moqt.StreamResetSessionClosed:
		return "SESSION_CLOSED"
	case moqt.StreamResetGoingAway:
		return "GOING_AWAY"
	case moqt.StreamResetTooFarBehind:
		return "TOO_FAR_BEHIND"
	case moqt.StreamResetUnknownObjectStatus:
		return "UNKNOWN_OBJECT_STATUS"
	case moqt.StreamResetExpiredAuthToken:
		return "EXPIRED_AUTH_TOKEN"
	case moqt.StreamResetExcessiveLoad:
		return "EXCESSIVE_LOAD"
	case moqt.StreamResetMalformedTrack:
		return "MALFORMED_TRACK"
	default:
		return "code " + strconv.FormatUint(uint64(code), 10)
	}
}
