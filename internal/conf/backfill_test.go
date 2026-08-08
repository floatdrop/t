package conf

import (
	"sync/atomic"
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

// The backfill and the live subscription arrive on separate streams, read on
// separate goroutines, and both push into the same sink. Their ranges are
// adjacent, so delivering the backfill first is correct ordering — but only if
// something makes that happen.
//
// Without it the decoder is handed the keyframe, then live frames whose
// reference frames have not arrived, then older frames with timestamps going
// backwards. Playback neither reorders nor checks monotonicity: it gates on the
// first keyframe and decodes in arrival order, and presentIndex is documented
// to require an ascending queue. What a participant sees is a smeared picture
// that jumps forward and back for as long as the backfill takes.
//
// Three things have to hold at once for the interleave to appear, and the two
// tests above have none of them:
//
//   - The publisher must still be publishing while the subscriber joins. If it
//     falls silent the backfill has the link to itself and finishes before the
//     next live object exists.
//   - The join must be deep into a group, so there is a substantial backfill to
//     drain rather than a couple of objects. Sixty objects is two seconds at
//     30 fps, which is one whole GOP — the worst case, and an ordinary one.
//   - The link must be *above* the live rate and below the live rate plus the
//     backfill. Below the live rate everything queues and the relay happens to
//     serve one stream then the other, which looks like correct ordering and is
//     not; on loopback the backfill lands in a single burst before the next
//     live object and the two never overlap. The bug lives in between, which is
//     where a real subscriber lives.
//
// Sized accordingly: 1200-byte frames at 30 fps is ~36 kB/s of live video
// against a 50 kB/s link, leaving about 14 kB/s for a ~76 kB backfill.
func TestVideoBackfillDoesNotOverlapTheLiveStream(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice, _ := joinRoom(t, addr, "order", "alice")
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "video", Codec: "avc1.42e01f", Width: 640, Height: 360,
	}); err != nil {
		t.Fatalf("declare video: %v", err)
	}

	// One long group, so the backfill is exactly "everything published so far"
	// and the test never races a group boundary it did not choose.
	var written atomic.Int64
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(33 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			if i > 0 {
				select {
				case <-stop:
					return
				case <-tick.C:
				}
			}
			size, key := 1200, i == 0
			if key {
				size = 4000
			}
			if alice.WriteFrame(videoFrame(uint64(i)*33_000, key, size)) == nil {
				written.Store(int64(i) + 1)
			}
		}
	}()
	defer close(stop)

	// Counted rather than timed: how far into the group bob arrives is what
	// decides how big the backfill is, and a wall-clock wait would make that
	// depend on how loaded the machine running the test is.
	waitFor(t, "a full GOP to accumulate", 20*time.Second, func() bool {
		return written.Load() >= backfillGOP
	})

	link := startShaper(t, addr, backfillLinkBytesPerSec, 64)
	_, bobRec := joinRoom(t, link.Addr(), "order", "bob")

	// Enough past the join to be sure the live stream has taken over: the two
	// ranges are adjacent and nothing is skipped, so this many objects cannot
	// have come from the backfill alone.
	const wantFrames = backfillGOP + 45
	waitFor(t, "the backfill and the live stream both to have been delivered",
		40*time.Second, func() bool {
			return len(videoTimestamps(bobRec)) >= wantFrames
		})

	ts := videoTimestamps(bobRec)
	for i := 1; i < len(ts); i++ {
		if ts[i] >= ts[i-1] {
			continue
		}
		// The neighbourhood, not just the pair: what this looks like when it is
		// wrong is live objects sprinkled through the backfill, and the run of
		// timestamps around the first one says so at a glance.
		t.Fatalf("frame %d us arrived after %d us — the backfill and the live "+
			"stream interleaved, and the decoder would be handed frames out of "+
			"order. Around it (us): %v",
			ts[i], ts[i-1], ts[max(0, i-4):min(len(ts), i+5)])
	}
}

const (
	// backfillGOP is how many objects are in the group before the subscriber
	// joins — two seconds at 30 fps, which is one keyframe interval.
	backfillGOP = 60
	// backfillLinkBytesPerSec is the subscriber's downlink: above the live
	// stream so it is delivered in real time, below live plus backfill so the
	// backfill takes long enough to still be arriving. See the test.
	backfillLinkBytesPerSec = 50_000
)

// videoTimestamps is the delivery order of one recorder's video frames, in
// microseconds — the order a decoder would be fed in.
func videoTimestamps(rec *recorder) []uint64 {
	frames, _, _, _ := rec.snapshot()
	var out []uint64
	for _, f := range frames {
		if f.Kind == bridge.KindVideo {
			out = append(out, f.Timestamp)
		}
	}
	return out
}
