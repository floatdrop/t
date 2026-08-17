package conf

import (
	"testing"
	"time"
)

// What a subscriber sees when it joins a call already in progress.
//
// A largest-object SUBSCRIBE starts after everything that exists, which for
// video is the middle of a GOP, and playback discards inbound frames until it
// sees a keyframe. So on its own a fresh subscription is blank until the
// publisher's next keyframe — five seconds, at the interval this app runs.
//
// Two things close that, and they are independent on purpose. A Joining FETCH
// replays the group in progress from its keyframe, which needs the relay to
// have it cached; a NEW_GROUP_REQUEST asks the publisher for a fresh keyframe,
// which needs the publisher to still be there and encoding. Either one alone
// paints the tile. Both are exercised below — the backfill here, the request in
// TestSubscribingAsksThePublisherForAKeyFrame.

// TestAJoiningSubscriberIsBackfilledToTheKeyFrame is the backfill on its own:
// the publisher writes a group and then stops, so nothing further is coming and
// the only way a keyframe reaches the subscriber is the FETCH replaying one
// that has already been sent.
//
// This is the test the old backfill could not have passed. It raced live video
// through a high-water mark and lost by construction; what is asserted here is
// that the replay arrives at all, in order, and exactly once.
func TestAJoiningSubscriberIsBackfilledToTheKeyFrame(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice := publisherWithBothTracks(t, addr, "backfill", "alice")

	// A keyframe and some deltas, all before anyone subscribes. This is the
	// whole of what alice ever sends: the group stays open and no second
	// keyframe is ever written, so a subscriber that has to wait for one waits
	// for the length of the test.
	if err := alice.WriteFrame(videoFrame(0, true, 900)); err != nil {
		t.Fatalf("write the keyframe: %v", err)
	}
	for i := 1; i <= 10; i++ {
		if err := alice.WriteFrame(videoFrame(uint64(i)*33_000, false, 300)); err != nil {
			t.Fatalf("write delta %d: %v", i, err)
		}
	}
	// A backfill can only replay what the relay has cached, and writing is not
	// the same as the relay having read it: the objects are on a QUIC stream the
	// relay drains on its own schedule. Subscribing immediately would snapshot a
	// largest object part-way through the run and backfill exactly as far, which
	// is correct behaviour and a meaningless assertion.
	time.Sleep(250 * time.Millisecond)

	_, bobRec := joinRoom(t, addr, "backfill", "bob")
	waitFor(t, "bob to subscribe to alice's video", subscribeWait, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) >= 2
	})

	// The whole group, not just its keyframe: the backfill covers everything
	// from the keyframe up to where the subscription begins, which is the
	// reference chain the deltas after it need.
	waitFor(t, "the backfilled group to reach bob", 10*time.Second, func() bool {
		return len(videoFrames(bobRec)) >= 11
	})

	got := videoFrames(bobRec)
	// The keyframe has to be first. A decoder handed a delta frame ahead of the
	// keyframe that opens the group has nothing to reference it against, and the
	// FETCH answers in ascending Object ID precisely so it does not have to be
	// re-sorted here.
	if !got[0].KeyFrame {
		t.Errorf("first frame delivered was a delta; the backfill must start at "+
			"the keyframe that opens the group, got %+v", got[0])
	}
	// In order, and each frame once. There are two delivery paths into this
	// handle now — the FETCH and the live subscription — and the whole design is
	// that they are adjacent ranges rather than competing ones.
	seen := make(map[uint64]bool, len(got))
	var last uint64
	for i, f := range got {
		if seen[f.Timestamp] {
			t.Errorf("frame at %d was delivered twice", f.Timestamp)
		}
		seen[f.Timestamp] = true
		if i > 0 && f.Timestamp <= last {
			t.Errorf("frame %d at %d arrived after %d; the backfill and the live "+
				"subscription must concatenate, not interleave", i, f.Timestamp, last)
		}
		last = f.Timestamp
	}
}

// TestSubscribingAsksThePublisherForAKeyFrame is the other half, and the half
// that does not depend on anything being cached: subscribing to a video track
// asks its publisher to cut a new group, which for video is a keyframe.
//
// §10.2.13 NEW_GROUP_REQUEST, carried on a REQUEST_UPDATE and forwarded to the
// publisher by the relay because the video track advertises DYNAMIC_GROUPS.
// Every link in that chain is load-bearing and none of them reports a failure
// anywhere the app would see, so this asserts the far end: the publisher's
// application callback ran.
//
// It is what decouples the keyframe interval from the join latency. Without it
// the interval is what a joining subscriber waits, and every argument about how
// long a GOP should be is really an argument about that.
func TestSubscribingAsksThePublisherForAKeyFrame(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	asked := make(chan struct{}, 1)
	alice, _ := joinRoomWithKeyFrameHook(t, addr, "newgroup", "alice", func() {
		select {
		case asked <- struct{}{}:
		default:
		}
	})
	declareBothTracks(t, alice)

	// Something must have been published: §10.2.13 forwards a request only for a
	// group above the largest, and the relay needs a largest to compare against.
	if err := alice.WriteFrame(videoFrame(0, true, 900)); err != nil {
		t.Fatalf("write the keyframe: %v", err)
	}

	_, bobRec := joinRoom(t, addr, "newgroup", "bob")
	waitFor(t, "bob to subscribe to alice's video", subscribeWait, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) >= 2
	})

	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the publisher was never asked for a new group — a subscriber " +
			"joining mid-GOP has no way to shorten its wait for a keyframe, so " +
			"the keyframe interval is the join latency again")
	}
}
