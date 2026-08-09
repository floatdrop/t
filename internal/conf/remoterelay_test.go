package conf

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"tlmst/internal/bridge"
)

// Temporal layers against a relay that is actually on a network.
//
// Everything else in this suite runs a relay in-process over loopback, where
// nothing is lost, nothing is reordered and the RTT is a scheduling decision.
// That is the right default — it is hermetic and it is fast — but it cannot
// answer whether the layered path survives a real path, which is the one
// question a subgroup-per-layer layout raises: two streams, two round trips,
// two chances for the relay to drain one well ahead of the other.
//
// Skipped unless TLMST_RELAY names one, so the suite stays hermetic.
func remoteRelay(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("TLMST_RELAY")
	if addr == "" {
		t.Skip("set TLMST_RELAY=host:port to run this against a real relay")
	}
	return addr
}

// remoteRoom keeps two runs of this from landing in the same room on a relay
// other people may be using.
func remoteRoom(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// ordering reports how many times the delivered sequence went backwards, which
// is the invariant that must hold whatever the network does to the frames.
func ordering(frames []bridge.MediaFrame) int {
	backwards := 0
	for i := 1; i < len(frames); i++ {
		if frames[i].Timestamp <= frames[i-1].Timestamp {
			backwards++
		}
	}
	return backwards
}

// TestRemoteRelayCarriesLayeredVideoInOrder publishes a two-layer stream at a
// real cadence across a real relay and reads it back.
//
// Loss is a fact of a real path, so completeness is reported rather than
// demanded. Order is not: a frame delivered after one that follows it is
// undecodable whatever the reason, so reassembly must never emit one.
func TestRemoteRelayCarriesLayeredVideoInOrder(t *testing.T) {
	addr := remoteRelay(t)
	room := remoteRoom("svc")

	alice := layeredPublisher(t, addr, room, "alice")
	_, bobRec := joinRoom(t, addr, room, "bob")
	waitFor(t, "bob to subscribe to alice's tracks over the network", 30*time.Second,
		func() bool {
			_, tracks, _, _ := bobRec.snapshot()
			return len(tracks) == 2
		})

	const seconds = 6
	stop := make(chan struct{})
	publishPacedLayers(t, alice, stop, temporalLayerFor)
	time.Sleep(seconds * time.Second)
	close(stop)
	// Long enough for the last group in flight to arrive.
	time.Sleep(2 * time.Second)

	got := videoFrames(bobRec)
	if len(got) == 0 {
		t.Fatal("no video arrived at all over the relay")
	}

	layers := bobRec.layerBytes("video")
	base, enhancement := layers[0], layers[1]
	backwards := ordering(got)

	// 30 fps for the publishing window, minus whatever the first subscription
	// missed. An approximation of what was sent, which is all that is needed to
	// read the delivered share as a percentage.
	sent := seconds * 30
	t.Logf("relay %s: %d video frames in %ds (~%d%% of a 30 fps window)",
		addr, len(got), seconds, 100*len(got)/sent)
	t.Logf("  base layer %d bytes, enhancement layer %d bytes (%d%% enhancement)",
		base, enhancement, 100*enhancement/max(1, base+enhancement))
	t.Logf("  out-of-order deliveries: %d", backwards)

	if backwards != 0 {
		t.Errorf("%d frames were delivered after a later one; reassembly must "+
			"never hand a decoder a frame it is already past", backwards)
	}
	if enhancement == 0 {
		t.Error("nothing arrived on the enhancement layer: the second subgroup " +
			"did not survive the trip, so there is no layer to shed")
	}
	if base == 0 {
		t.Error("nothing arrived on the base layer")
	}
}

// TestRemoteRelayCarriesALayeredBurst is the backfill shape over a real path: a
// whole group at once, with nothing pacing the two subgroup streams against
// each other, which is where the relay is free to drain one far ahead of the
// other.
func TestRemoteRelayCarriesALayeredBurst(t *testing.T) {
	addr := remoteRelay(t)
	room := remoteRoom("svcburst")

	alice := layeredPublisher(t, addr, room, "alice")
	_, bobRec := joinRoom(t, addr, room, "bob")
	waitFor(t, "bob to subscribe to alice's tracks over the network", 30*time.Second,
		func() bool {
			_, tracks, _, _ := bobRec.snapshot()
			return len(tracks) == 2
		})

	const frames = 40
	for i := range frames {
		f := layeredVideoFrame(
			uint64(i)*frameStep, i == 0, 1200, temporalLayerFor(i))
		if err := alice.WriteFrame(f); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}
	closer := uint64(frames) * frameStep
	if err := alice.WriteFrame(
		layeredVideoFrame(closer, true, 1200, 0)); err != nil {
		t.Fatalf("write the keyframe that closes the group: %v", err)
	}
	time.Sleep(5 * time.Second)

	var got []bridge.MediaFrame
	for _, f := range videoFrames(bobRec) {
		if f.Timestamp < closer {
			got = append(got, f)
		}
	}
	backwards := ordering(got)
	t.Logf("relay %s: %d of %d frames of a burst group, out-of-order %d",
		addr, len(got), frames, backwards)

	if backwards != 0 {
		t.Errorf("%d frames of the burst were delivered out of order", backwards)
	}
	if len(got) < frames {
		t.Logf("NOTE: %d frames did not arrive; over a real path that can be "+
			"loss rather than reassembly", frames-len(got))
	}
}

// Measuring what a reordering buffer would have to hold, and for how long.
//
// The design question it answers: video ordering across groups needs a delay
// budget, and that budget has to fit inside the audio playout lag (60 ms of
// preroll, 120 ms after a trim) or it stops being free. So the number that
// matters is not how often frames arrive out of order but how long one would
// have had to be held back to let an earlier one overtake it.
type inversion struct {
	// hold is how long the buffer would have had to keep the frame that
	// arrived early, waiting for the one that should have preceded it.
	hold time.Duration
	// span is how far apart the two frames are in media time.
	span time.Duration
	// crossGroup marks an inversion that spans a keyframe, which is the case
	// within-group reassembly cannot see.
	crossGroup bool
	// pos is how many frames of this handle had already been delivered, which
	// separates an inversion caused by the backfill racing the live edge at
	// subscription time from one the network caused later.
	pos int
}

// inversionsIn walks a delivery timeline and reports every frame that arrived
// after one that should have followed it.
//
// Per handle, which is not a detail. A demotion retires the subscription and
// builds a new one at the live edge, with a backfill behind it — so frames from
// the old handle and the new one interleave, seconds apart on the publisher's
// clock, and counting those together would report the ladder working as a
// reordering failure. Each handle is its own decoder; ordering only means
// anything within one.
func inversionsIn(frames []bridge.MediaFrame, at []time.Time, kind uint8, handle uint32) []inversion {
	var out []inversion
	var maxTs uint64
	var maxAt time.Time
	var keyframesSince int
	var delivered int
	seen := false

	for i, f := range frames {
		if f.Kind != kind || f.Handle != handle {
			continue
		}
		if !seen {
			maxTs, maxAt, seen = f.Timestamp, at[i], true
			continue
		}
		if f.Timestamp < maxTs {
			out = append(out, inversion{
				hold:       at[i].Sub(maxAt),
				span:       time.Duration(maxTs-f.Timestamp) * time.Microsecond,
				crossGroup: keyframesSince > 0,
				pos:        delivered,
			})
			continue
		}
		delivered++
		if f.KeyFrame {
			keyframesSince++
		}
		maxTs, maxAt = f.Timestamp, at[i]
	}
	return out
}

// report prints the distribution a delay budget would have to cover.
func report(t *testing.T, label string, frames []bridge.MediaFrame, at []time.Time, kind uint8) {
	t.Helper()
	var handles []uint32
	counts := map[uint32]int{}
	for _, f := range frames {
		if f.Kind != kind {
			continue
		}
		if counts[f.Handle] == 0 {
			handles = append(handles, f.Handle)
		}
		counts[f.Handle]++
	}

	total := 0
	var inv []inversion
	for _, h := range handles {
		total += counts[h]
		inv = append(inv, inversionsIn(frames, at, kind, h)...)
	}
	if len(inv) == 0 {
		t.Logf("%s: %d frames over %d handle(s), 0 inversions — no buffer would "+
			"have changed anything", label, total, len(handles))
		return
	}
	holds := make([]time.Duration, len(inv))
	cross := 0
	var worstSpan time.Duration
	for i, v := range inv {
		holds[i] = v.hold
		if v.crossGroup {
			cross++
		}
		worstSpan = max(worstSpan, v.span)
	}
	// Splitting the big ones by where they fell in the handle's life: a backfill
	// replays the group in progress behind the live edge, so anything it causes
	// lands in the opening seconds of a subscription and nowhere else.
	early, late := 0, 0
	for _, v := range inv {
		if v.hold < 500*time.Millisecond {
			continue
		}
		if v.pos < 60 {
			early++
		} else {
			late++
		}
	}
	slices.Sort(holds)
	t.Logf("%s: %d frames over %d handle(s), %d inversions (%d cross-group, %.2f%%)",
		label, total, len(handles), len(inv), cross,
		100*float64(len(inv))/float64(max(total, 1)))
	t.Logf("    hold needed: p50 %v, p95 %v, worst %v (widest media span %v)",
		holds[len(holds)/2], holds[(len(holds)*95)/100], holds[len(holds)-1], worstSpan)
	t.Logf("    holds over 500ms: %d in a handle's first 60 frames, %d after",
		early, late)
}

// TestRemoteRelayMeasuresReorderingBudget is the measurement that sizes D, on
// a healthy path and on one squeezed to the point where the ladder acts.
//
// Reported rather than asserted: it is evidence for a design decision, and the
// only thing it can fail on is a path that delivered nothing.
func TestRemoteRelayMeasuresReorderingBudget(t *testing.T) {
	addr := remoteRelay(t)

	for _, tc := range []struct {
		name string
		via  func(t *testing.T) string
	}{
		{"healthy", func(*testing.T) string { return addr }},
		{"bottlenecked", func(t *testing.T) string {
			return startShaper(t, addr, 32_000, 64).Addr()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			room := remoteRoom("budget-" + tc.name)
			alice := layeredPublisher(t, addr, room, "alice")
			_, bobRec := joinRoom(t, tc.via(t), room, "bob")
			waitFor(t, "bob to subscribe", 30*time.Second, func() bool {
				_, tracks, _, _ := bobRec.snapshot()
				return len(tracks) == 2
			})

			stop := make(chan struct{})
			publishPacedLayers(t, alice, stop, temporalLayerFor)
			time.Sleep(45 * time.Second)
			close(stop)
			time.Sleep(2 * time.Second)

			frames, at := bobRec.timeline()
			if len(frames) == 0 {
				t.Fatal("nothing arrived")
			}
			report(t, tc.name+"/video", frames, at, bridge.KindVideo)
			report(t, tc.name+"/audio", frames, at, bridge.KindAudio)
		})
	}
}
