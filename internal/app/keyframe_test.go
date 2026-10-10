package app

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"t/internal/bridge"
)

// keyFrameRequests counts the keyframe requests the frame log recorded, which
// is every one requestKeyFrame actually sent to the encoder.
func keyFrameRequests(a *App, buf *bytes.Buffer) int {
	a.frames.flush()
	return strings.Count(buf.String(), ",kfreq,")
}

// TestKeyFrameRequestsInsideTheIntervalArePostponed covers the request that
// used to be discarded for arriving too soon after the last one.
//
// A relay forwards one NEW_GROUP_REQUEST and treats it as outstanding until the
// track's largest group advances (§10.2.19), so every later request stops at
// the relay. Discarding the one it forwarded silenced every subscriber behind
// it until the scheduled keyframe — a whole interval of frozen tile. Postponed,
// it is answered at the end of the interval, and a burst of requests costs one
// keyframe rather than one each.
func TestKeyFrameRequestsInsideTheIntervalArePostponed(t *testing.T) {
	a := newTestApp(t)
	withServer(t, a)
	var buf bytes.Buffer
	a.frames = newFrameLog(&buf)

	a.requestKeyFrame()
	if got := keyFrameRequests(a, &buf); got != 1 {
		t.Fatalf("first request sent %d keyframe requests, want 1 at once", got)
	}

	// Inside the interval: none goes out now, however many arrive.
	for range 5 {
		a.requestKeyFrame()
	}
	if got := keyFrameRequests(a, &buf); got != 1 {
		t.Fatalf("requests inside the interval sent %d in total, want still 1", got)
	}

	// One goes out at the end of it, for all of them.
	deadline := time.Now().Add(keyFrameAskInterval + 2*time.Second)
	for keyFrameRequests(a, &buf) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := keyFrameRequests(a, &buf); got != 2 {
		t.Fatalf("after the interval %d keyframe requests in total, want 2 — a "+
			"request inside the interval was dropped rather than postponed", got)
	}
	time.Sleep(keyFrameAskInterval + 100*time.Millisecond)
	if got := keyFrameRequests(a, &buf); got != 2 {
		t.Errorf("%d keyframe requests in total, want 2 — the postponed burst "+
			"should cost one keyframe, not one each", got)
	}
}

// TestAKeyFrameCancelsThePostponedRequest covers the request that has already
// been answered. A publisher with no open group asks on every frame it turns
// away until the keyframe comes, so a postponed request is almost always
// pending when it does — and firing it anyway costs a second keyframe on the
// uplink that just stalled.
func TestAKeyFrameCancelsThePostponedRequest(t *testing.T) {
	a := newTestApp(t)
	withServer(t, a)
	var buf bytes.Buffer
	a.frames = newFrameLog(&buf)

	a.requestKeyFrame()
	a.requestKeyFrame() // postponed
	if err := a.HandleMedia(t.Context(), &bridge.MediaFrame{
		Kind: bridge.KindVideo, KeyFrame: true, Payload: []byte{1},
	}); err != nil {
		t.Fatalf("HandleMedia: %v", err)
	}

	time.Sleep(keyFrameAskInterval + 200*time.Millisecond)
	if got := keyFrameRequests(a, &buf); got != 1 {
		t.Errorf("%d keyframe requests, want 1 — the postponed one fired after a "+
			"keyframe had already answered it", got)
	}
}

// TestLeavingCancelsThePostponedRequest covers a request outliving its room: it
// would make the next session's encoder cut a keyframe nobody there asked for.
func TestLeavingCancelsThePostponedRequest(t *testing.T) {
	a := newTestApp(t)
	withServer(t, a)
	var buf bytes.Buffer
	a.frames = newFrameLog(&buf)

	a.requestKeyFrame()
	a.requestKeyFrame() // postponed
	a.leave()

	time.Sleep(keyFrameAskInterval + 200*time.Millisecond)
	if got := keyFrameRequests(a, &buf); got != 1 {
		t.Errorf("%d keyframe requests, want 1 — a request postponed for a room "+
			"fired after leaving it", got)
	}
}
