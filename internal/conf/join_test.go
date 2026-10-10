package conf

import (
	"testing"
	"time"
)

// What a subscriber sees when it joins a call already in progress.
//
// A Next Object SUBSCRIBE starts after everything that exists, which for
// video is the middle of a GOP, and playback discards inbound frames until it
// sees a keyframe. So on its own a fresh subscription is blank until the
// publisher's next keyframe — five seconds, at the interval this app runs.
//
// Two things close that, and they are independent on purpose. A fill fetch
// stream replays the group in progress from its keyframe, which needs the relay to
// have it cached; a NEW_GROUP_REQUEST asks the publisher for a fresh keyframe,
// which needs the publisher to still be there and encoding. Either one alone
// paints the tile. Both are exercised below — the backfill here, the request in
// TestSubscribingAsksThePublisherForAKeyFrame.

// TestAJoiningSubscriberIsBackfilledToTheKeyFrame is the backfill on its own:
// the publisher writes a group and then stops, so nothing further is coming and
// the only way a keyframe reaches the subscriber is the fill replaying one
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
// §10.2.19 NEW_GROUP_REQUEST, carried on a REQUEST_UPDATE and forwarded to the
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

	// Something must have been published: §10.2.19 forwards a request only for a
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

// TestAReceiverCanAskForAKeyFrameLater covers the request made after joining:
// a receiver that lost its reference chain somewhere this package cannot see —
// the frontend's decoder, the bridge on the way to it — asks the publisher for
// a new group by the track's handle.
//
// What it pins is the value. §10.2.19 forwards a request only above the
// relay's largest group, and only when no equal-or-greater one is outstanding,
// so asking with the value the subscription asked with when it joined would
// stop at the relay. The request has to be one past the newest group the
// receiver has actually seen.
func TestAReceiverCanAskForAKeyFrameLater(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	asked := make(chan struct{}, 4)
	alice, _ := joinRoomWithKeyFrameHook(t, addr, "newgroup-later", "alice", func() {
		asked <- struct{}{}
	})
	declareBothTracks(t, alice)
	if err := alice.WriteFrame(videoFrame(0, true, 900)); err != nil {
		t.Fatalf("write the keyframe: %v", err)
	}

	bob, bobRec := joinRoom(t, addr, "newgroup-later", "bob")
	var handle uint32
	waitFor(t, "bob to subscribe to alice's video", subscribeWait, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		for _, tr := range tracks {
			if tr.Config.Kind == "video" {
				handle = tr.Handle
				return true
			}
		}
		return false
	})

	// The subscribe asks once; answer it the way a publisher does, with a new
	// group, which is also what clears the relay's outstanding request.
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("subscribing did not ask the publisher for a new group")
	}
	if err := alice.WriteFrame(videoFrame(40_000, true, 900)); err != nil {
		t.Fatalf("write the answering keyframe: %v", err)
	}
	waitFor(t, "bob to receive the answering keyframe", subscribeWait, func() bool {
		for _, f := range videoFrames(bobRec) {
			if f.KeyFrame && f.Timestamp == 40_000 {
				return true
			}
		}
		return false
	})

	bob.RequestKeyFrame(handle, KeyFrameForDecoder)
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("a receiver's later request never reached the publisher — a " +
			"rebuilt decoder waits for the publisher's next scheduled keyframe")
	}
}

// TestNextGroupIsOnePastTheNewest pins the arithmetic the request values come
// from, including group zero, which is a real group and must not read as
// "nothing seen yet".
func TestNextGroupIsOnePastTheNewest(t *testing.T) {
	var tr remoteTrack
	if got := tr.nextGroup(); got != 0 {
		t.Errorf("nothing seen: nextGroup = %d, want 0 (no group information)", got)
	}
	tr.noteGroup(0)
	if got := tr.nextGroup(); got != 1 {
		t.Errorf("group 0 seen: nextGroup = %d, want 1", got)
	}
	tr.noteGroup(5)
	tr.noteGroup(3) // a straggler from an older group does not lower it
	if got := tr.nextGroup(); got != 6 {
		t.Errorf("groups 0, 5, 3 seen: nextGroup = %d, want 6", got)
	}

	backfilled := remoteTrack{backfilled: 9, hasBackfill: true}
	if got := backfilled.nextGroup(); got != 10 {
		t.Errorf("backfilling group 9: nextGroup = %d, want 10", got)
	}
}
