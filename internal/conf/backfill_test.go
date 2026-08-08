package conf

import (
	"testing"
	"time"

	"tlmst/internal/bridge"
)

// A subscriber joining mid-GOP used to see nothing at all until the publisher's
// next keyframe — up to the whole keyframe interval of blank tile — because the
// largest-object filter starts after whatever exists and playback discards
// inbound frames until it sees a keyframe. There is no way to ask a remote
// publisher for one sooner.
//
// The Relative Joining FETCH covers exactly the group in progress, from its
// keyframe up to where the subscription begins.
func TestVideoBackfillDeliversTheGroupInProgress(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice, _ := joinRoom(t, addr, "backfill", "alice")
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "video", Codec: "avc1.42e01f", Width: 640, Height: 360,
	}); err != nil {
		t.Fatalf("declare video: %v", err)
	}

	// A group opened by a keyframe, then some way into it — the state a
	// subscriber joining a call in progress actually finds.
	if err := alice.WriteFrame(videoFrame(0, true, 900)); err != nil {
		t.Fatalf("write keyframe: %v", err)
	}
	for i := 1; i <= 6; i++ {
		if err := alice.WriteFrame(videoFrame(uint64(i)*33_000, false, 300)); err != nil {
			t.Fatalf("write delta %d: %v", i, err)
		}
	}

	_, bobRec := joinRoom(t, addr, "backfill", "bob")
	waitFor(t, "bob to subscribe to the video", 10*time.Second, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 1
	})

	waitFor(t, "bob to be handed the keyframe he arrived after", 10*time.Second, func() bool {
		frames, _, _, _ := bobRec.snapshot()
		for _, f := range frames {
			if f.Kind == bridge.KindVideo && f.KeyFrame {
				return true
			}
		}
		return false
	})

	// Nothing is published after bob joined, so everything he has came from the
	// backfill — which means the whole group has to be there, not just its
	// first object.
	waitFor(t, "the rest of the group to follow the keyframe", 10*time.Second, func() bool {
		frames, _, _, _ := bobRec.snapshot()
		n := 0
		for _, f := range frames {
			if f.Kind == bridge.KindVideo {
				n++
			}
		}
		return n >= 7
	})
}

// The fetch ends where the subscription starts — {group, largest+1} is the
// exclusive end of one and the inclusive start of the other. If that boundary
// were off by one the overlapping object would be decoded twice, so it is worth
// pinning rather than trusting.
func TestVideoBackfillDoesNotDeliverAnythingTwice(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice, _ := joinRoom(t, addr, "backfill2", "alice")
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "video", Codec: "avc1.42e01f", Width: 640, Height: 360,
	}); err != nil {
		t.Fatalf("declare video: %v", err)
	}

	if err := alice.WriteFrame(videoFrame(0, true, 900)); err != nil {
		t.Fatalf("write keyframe: %v", err)
	}
	for i := 1; i <= 4; i++ {
		if err := alice.WriteFrame(videoFrame(uint64(i)*33_000, false, 300)); err != nil {
			t.Fatalf("write delta %d: %v", i, err)
		}
	}

	_, bobRec := joinRoom(t, addr, "backfill2", "bob")
	waitFor(t, "bob to receive the backfilled group", 10*time.Second, func() bool {
		frames, _, _, _ := bobRec.snapshot()
		n := 0
		for _, f := range frames {
			if f.Kind == bridge.KindVideo {
				n++
			}
		}
		return n >= 5
	})

	// Then the publisher carries on, so the live subscription takes over from
	// where the backfill stopped.
	for i := 5; i <= 10; i++ {
		if err := alice.WriteFrame(videoFrame(uint64(i)*33_000, false, 300)); err != nil {
			t.Fatalf("write delta %d: %v", i, err)
		}
	}
	waitFor(t, "the live objects to follow", 10*time.Second, func() bool {
		frames, _, _, _ := bobRec.snapshot()
		n := 0
		for _, f := range frames {
			if f.Kind == bridge.KindVideo {
				n++
			}
		}
		return n >= 11
	})

	frames, _, _, _ := bobRec.snapshot()
	seen := map[uint64]int{}
	for _, f := range frames {
		if f.Kind == bridge.KindVideo {
			seen[f.Timestamp]++
		}
	}
	for ts, n := range seen {
		if n > 1 {
			t.Errorf("the frame at %d us arrived %d times: the backfill and the "+
				"subscription overlap, and a decoder would see it twice", ts, n)
		}
	}
}
