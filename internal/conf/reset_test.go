package conf

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/floatdrop/moq-go/pkg/moqt"
)

// The app dials raw QUIC or WebTransport depending on the relay address, and
// the two raise different types for the same event: a peer resetting a data
// stream. Reading only one of them is not a partial implementation, it is a
// silent one — the reset arrives looking exactly like the end of a group, and
// a subscriber dropped for falling behind is never told, never demotes, and
// sits frozen for the rest of the call.
func TestStreamResetReadsBothTransports(t *testing.T) {
	const code = moqt.StreamResetTooFarBehind

	cases := []struct {
		name string
		err  error
		want moqt.StreamResetCode
		ok   bool
	}{
		{
			name: "quic reset by the peer",
			err:  &quic.StreamError{ErrorCode: quic.StreamErrorCode(code), Remote: true},
			want: code,
			ok:   true,
		},
		{
			name: "webtransport reset by the peer",
			err:  &webtransport.StreamError{ErrorCode: webtransport.StreamErrorCode(code), Remote: true},
			want: code,
			ok:   true,
		},
		{
			// Our own teardown surfaces the same type. Reporting it as the
			// relay's verdict would demote a participant for a subscription we
			// closed on purpose.
			name: "quic reset by us",
			err:  &quic.StreamError{ErrorCode: quic.StreamErrorCode(code), Remote: false},
			ok:   false,
		},
		{
			name: "webtransport reset by us",
			err:  &webtransport.StreamError{ErrorCode: webtransport.StreamErrorCode(code), Remote: false},
			ok:   false,
		},
		{
			// A group ending normally, which is what every group does.
			name: "end of stream",
			err:  io.EOF,
			ok:   false,
		},
		{
			name: "something else entirely",
			err:  errors.New("read failed"),
			ok:   false,
		},
		{
			// The read path wraps as it goes, so this has to survive being
			// buried rather than only matching a bare error.
			name: "wrapped several layers deep",
			err: fmt.Errorf("reading media: %w",
				fmt.Errorf("subgroup: %w",
					&webtransport.StreamError{
						ErrorCode: webtransport.StreamErrorCode(code),
						Remote:    true,
					})),
			want: code,
			ok:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := streamReset(tc.err)
			if ok != tc.ok {
				t.Fatalf("streamReset ok = %v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("streamReset code = %v, want %v", got, tc.want)
			}
		})
	}
}

// The codes this client acts on have to survive the round trip through each
// transport's own error type, or a demotion would be decided on the wrong one.
func TestOverloadCodesSurviveBothTransports(t *testing.T) {
	for _, code := range []moqt.StreamResetCode{
		moqt.StreamResetTooFarBehind,
		moqt.StreamResetExcessiveLoad,
	} {
		for name, err := range map[string]error{
			"quic":         &quic.StreamError{ErrorCode: quic.StreamErrorCode(code), Remote: true},
			"webtransport": &webtransport.StreamError{ErrorCode: webtransport.StreamErrorCode(code), Remote: true},
		} {
			got, ok := streamReset(err)
			if !ok {
				t.Errorf("%s: %s was not read as a reset", name, resetName(code))
				continue
			}
			if !overloadReset(got) {
				t.Errorf("%s: %s was not recognised as an overload", name, resetName(code))
			}
		}
	}
}

// A code this client has no reading of is rendered rather than guessed at, so
// a log line about an unfamiliar reset still says which one it was.
func TestResetNameFallsBackToTheNumber(t *testing.T) {
	if got := resetName(moqt.StreamResetTooFarBehind); got != "TOO_FAR_BEHIND" {
		t.Errorf("resetName = %q, want TOO_FAR_BEHIND", got)
	}
	if got := resetName(moqt.StreamResetCode(0x7fff)); got != "code 32767" {
		t.Errorf("resetName = %q, want the numeric fallback", got)
	}
}
