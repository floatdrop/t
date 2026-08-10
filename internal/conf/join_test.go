package conf

import (
	"testing"
	"time"
)

// What a subscriber sees when it joins a call already in progress.
//
// There used to be a FETCH here, replaying the group in progress so the tile
// painted without waiting for the next keyframe. It went, because it could no
// longer win: every backfilled frame is older than every live one by
// construction, so the first live object to arrive made the whole replay stale,
// and the replay had a round trip and a whole group of bytes to get through
// first. What it reliably delivered was a decoded, sorted, discarded GOP and a
// four-second blind spot on the drift meter, while the tile stayed blank until
// the keyframe anyway.
//
// So the keyframe is the contract and the group length is the wait — which is
// why the publisher's keyframe interval came down to a second when the FETCH
// went.
func TestAJoiningSubscriberStartsAtAKeyFrame(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice := publisherWithBothTracks(t, addr, "join", "alice")

	// Mid-group when the subscriber arrives: a keyframe, then deltas, and only
	// then does anyone subscribe.
	if err := alice.WriteFrame(videoFrame(0, true, 900)); err != nil {
		t.Fatalf("write the keyframe: %v", err)
	}
	for i := 1; i <= 10; i++ {
		if err := alice.WriteFrame(videoFrame(uint64(i)*33_000, false, 300)); err != nil {
			t.Fatalf("write delta %d: %v", i, err)
		}
	}

	_, bobRec := joinRoom(t, addr, "join", "bob")
	waitFor(t, "bob to subscribe to alice's video", subscribeWait, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) >= 2
	})

	// The next group. Everything before it belongs to a group bob joined the
	// middle of and cannot decode.
	for i := 11; i <= 30; i++ {
		key := i == 20
		size := 300
		if key {
			size = 900
		}
		if err := alice.WriteFrame(videoFrame(uint64(i)*33_000, key, size)); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	waitFor(t, "video to reach bob", 10*time.Second, func() bool {
		return len(videoFrames(bobRec)) > 0
	})

	// A keyframe has to arrive, and within a group of joining: the backend
	// forwards the mid-group deltas too — the decoder's own gate discards those
	// (playback.ts drops until sawKeyFrame) — but if no keyframe followed, the
	// tile would never paint at all now that nothing replays the group.
	waitFor(t, "a keyframe to reach bob", 10*time.Second, func() bool {
		for _, f := range videoFrames(bobRec) {
			if f.KeyFrame {
				return true
			}
		}
		return false
	})

	got := videoFrames(bobRec)
	// Nothing is delivered twice: there is one path from the wire to the
	// decoder now, which is most of the point of removing the other one.
	seen := make(map[uint64]bool, len(got))
	for _, f := range got {
		if seen[f.Timestamp] {
			t.Errorf("frame at %d was delivered twice", f.Timestamp)
		}
		seen[f.Timestamp] = true
	}
}
