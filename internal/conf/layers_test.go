package conf

import (
	"testing"
	"time"

	"tlmst/internal/bridge"
)

// A publisher that actually emits temporal layers.
//
// Everything else in this suite publishes flat video — every frame on the base
// layer, one subgroup per group, the shape every track had before layers
// existed. That is enough to prove the layered path does no harm and useless
// for proving it does anything: against a flat publisher, declining the
// enhancement subgroup declines a stream nobody was writing to, so it sheds
// nothing and cannot be told from asking for everything. A rung that cannot be
// distinguished from the rung above it is not a rung, and a test that cannot
// distinguish them will pass whether the code works or not.
//
// The layer assignment here is a convention rather than an encoder's decision —
// nothing in a test encodes anything — but it is the convention a real L1T2
// encoder follows, and the two properties the transport depends on are kept: a
// keyframe is always on the base layer, and no base frame ever references a
// frame above it. What the base layer alone yields is therefore a real, if
// synthetic, video stream at half the frame rate.

// frameStep is the publishing cadence these tests use, 30 fps in microseconds.
const frameStep = 33_000

// temporalLayerFor assigns frame i in an L1T2 pattern: alternate frames on the
// enhancement layer, so the base is half the frame rate and half the frames.
//
// Even-numbered frames are the base, which is what keeps keyframes on it: every
// pacer here puts keyframes at a multiple of an even keyframe interval, and a
// keyframe above the base layer would be a group whose first object is on a
// stream a base-only subscriber had declined.
func temporalLayerFor(i int) uint8 {
	if i%2 == 1 {
		return 1
	}
	return 0
}

// layerBytes totals received payload bytes per temporal layer for one track
// kind, matched through the announced handles the way countsFor does.
//
// Bytes rather than frames because the question it answers is what shedding a
// layer would save, and the wire is paid for in bytes.
func (r *recorder) layerBytes(kind string) map[uint8]int {
	r.mu.Lock()
	defer r.mu.Unlock()

	handles := make(map[uint32]bool)
	for _, t := range r.tracks {
		if t.Config.Kind == kind {
			handles[t.Handle] = true
		}
	}
	out := make(map[uint8]int)
	for _, f := range r.frames {
		if handles[f.Handle] {
			out[f.TemporalLayer] += len(f.Payload)
		}
	}
	return out
}

// videoFrames pulls the received video out of a recorder, in arrival order.
func videoFrames(rec *recorder) []bridge.MediaFrame {
	frames, _, _, _ := rec.snapshot()
	var video []bridge.MediaFrame
	for _, f := range frames {
		if f.Kind == bridge.KindVideo {
			video = append(video, f)
		}
	}
	return video
}

// layeredPublisher joins a room and declares audio plus video.
//
// The declaration says nothing about layers, and does not need to: how many a
// track carries is not something a subscriber has to be told any more. It used
// to be, when reassembly waited for the layers it had been told to expect;
// what waits now is one enhancement object for the base frame it references,
// which the objects themselves answer. What makes this publisher layered is
// what it writes — see publishLayeredGroup.
func layeredPublisher(t *testing.T, addr, room, nickname string) *Room {
	t.Helper()
	r, _ := joinRoom(t, addr, room, nickname)
	if err := r.DeclareTrack(&bridge.TrackConfig{
		Kind: "audio", Codec: "opus", SampleRate: 48000, Channels: 1,
	}); err != nil {
		t.Fatalf("declare audio: %v", err)
	}
	if err := r.DeclareTrack(&bridge.TrackConfig{
		Kind: "video", Codec: "avc1.42e01f", Width: 640, Height: 360,
	}); err != nil {
		t.Fatalf("declare video: %v", err)
	}
	return r
}

// layeredPair brings up a two-layer publisher and a subscriber watching it,
// with both of the publisher's media tracks subscribed.
func layeredPair(t *testing.T, room string) (*Room, *recorder) {
	t.Helper()
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice := layeredPublisher(t, addr, room, "alice")
	_, bobRec := joinRoom(t, addr, room, "bob")
	waitFor(t, "bob to subscribe to alice's tracks", 10*time.Second, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 2
	})
	return alice, bobRec
}

// publishLayeredGroup writes one group of layered video at the frame cadence: a
// keyframe on the base layer, then count-1 frames alternating between the
// layers. The first timestamp is at, and the group is left open — a group ends
// when the next keyframe starts the one after it.
func publishLayeredGroup(t *testing.T, room *Room, at uint64, count int) {
	t.Helper()
	for i := range count {
		f := layeredVideoFrame(
			at+uint64(i)*frameStep, i == 0, 300, temporalLayerFor(i))
		if err := room.WriteFrame(f); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
		time.Sleep(33 * time.Millisecond)
	}
}

// TestTemporalLayersArriveInDecodeOrder is reorder.go over a real relay.
//
// Its unit tests drive the reassembler directly, which establishes the ordering
// rule and nothing about whether anything upstream honours it: that each layer's
// object IDs stay consecutive so the relay keeps one stream per subgroup rather
// than one per frame, that both streams survive the trip, and that the reader
// threads them back together on the timestamp. Each of those is a separate place
// decode order can be lost, and not one of them is exercised by a publisher that
// only ever opens a single stream.
//
// One group, deliberately: group boundaries are their own streams and their own
// question, and this is about what happens inside a group, where the two
// concurrent readers are.
func TestTemporalLayersArriveInDecodeOrder(t *testing.T) {
	alice, bobRec := layeredPair(t, "layered1")

	// The tail of a group is held rather than delivered — the base layer's last
	// frame is waiting to hear that the enhancement layer has nothing earlier
	// left, which only the next object or the end of that stream can say. In a
	// call the next frame answers in 33 ms. Here the next keyframe does it, by
	// closing the group, which is how a publisher ends one.
	const frames = 41 // one keyframe and forty deltas, twenty on each layer
	publishLayeredGroup(t, alice, 0, frames)
	closesTheGroup := uint64(frames) * frameStep
	if err := alice.WriteFrame(
		layeredVideoFrame(closesTheGroup, true, 300, 0)); err != nil {
		t.Fatalf("write the keyframe that closes the group: %v", err)
	}

	inGroup := func() []bridge.MediaFrame {
		var got []bridge.MediaFrame
		for _, f := range videoFrames(bobRec) {
			if f.Timestamp < closesTheGroup {
				got = append(got, f)
			}
		}
		return got
	}
	waitFor(t, "every frame of the group to reach bob", 10*time.Second, func() bool {
		return len(inGroup()) >= frames
	})

	got := inGroup()
	if len(got) != frames {
		t.Fatalf("bob received %d frames of the group, want %d", len(got), frames)
	}
	for i, f := range got {
		if want := uint64(i) * frameStep; f.Timestamp != want {
			t.Fatalf("frame %d of the group arrived out of decode order: "+
				"timestamp %d, want %d — a decoder would have been handed a "+
				"delta frame ahead of the frame it references",
				i, f.Timestamp, want)
		}
		// Taken from the Subgroup ID the frame arrived on, so a frame reported
		// on the layer it was published on is one that rode that layer's
		// subgroup the whole way.
		if want := temporalLayerFor(i); f.TemporalLayer != want {
			t.Errorf("frame %d arrived on layer %d, want %d",
				i, f.TemporalLayer, want)
		}
	}
}

// Nothing may be stranded at the end of a group. A group's last frames are held
// against the streams still open, and the streams of a group all end together
// when the next keyframe rotates it — so a group that ends with something still
// held is a group whose tail never reaches the decoder, which on a one-second
// GOP is most of a second of a tile frozen part-way through.
func TestNoFramesAreStrandedAtTheEndOfALayeredGroup(t *testing.T) {
	alice, bobRec := layeredPair(t, "layered2")

	// Each group ends on the enhancement layer, which is the case where the
	// base stream has nothing further to say and the group's last object is on
	// the stream a base-only subscriber would have declined.
	const (
		groups   = 3
		perGroup = 10 // even, so the last frame of a group is on layer 1
		total    = groups * perGroup
	)
	for g := range groups {
		publishLayeredGroup(t, alice, uint64(g*perGroup)*frameStep, perGroup)
	}
	// Closes the last group. A group of its own, and one that stays open: it
	// declares two layers and carries a single base frame, so it waits for a
	// stream that never comes and is not counted below.
	closer := uint64(total) * frameStep
	if err := alice.WriteFrame(
		layeredVideoFrame(closer, true, 300, 0)); err != nil {
		t.Fatalf("write the keyframe that closes the last group: %v", err)
	}

	closed := func() []bridge.MediaFrame {
		var got []bridge.MediaFrame
		for _, f := range videoFrames(bobRec) {
			if f.Timestamp < closer {
				got = append(got, f)
			}
		}
		return got
	}
	waitFor(t, "every frame of every closed group to reach bob", 10*time.Second, func() bool {
		return len(closed()) >= total
	})
	if got := len(closed()); got != total {
		t.Errorf("bob received %d video frames across %d groups, want %d",
			got, groups, total)
	}
}

// TestTheEnhancementLayerIsWorthShedding is the measurement the base-only rung
// rests on: how much of the video is on the layer that can be dropped?
//
// The saving is what makes the rung worth having. A step down the ladder that
// frees nothing spends a subscription change to leave the link exactly as
// congested as it was, and the ladder would do better to skip straight to the
// smaller encoding. Measured over sustained publishing rather than a burst,
// because it is a claim about a rate.
//
// Deliberately on an unshaped link, for the reason
// TestVisibilityGatingCutsInboundBitrate is: under a bottleneck the received
// rate is capped by the link however it is divided, and the division is the
// subject.
func TestTheEnhancementLayerIsWorthShedding(t *testing.T) {
	alice, bobRec := layeredPair(t, "layered3")

	stop := make(chan struct{})
	publishPacedLayers(t, alice, stop, temporalLayerFor)
	time.Sleep(3 * time.Second)
	close(stop)

	bytes := bobRec.layerBytes("video")
	base, enhancement := bytes[0], bytes[1]
	if base == 0 {
		t.Fatal("no base layer arrived at all")
	}
	if enhancement == 0 {
		t.Fatal("nothing arrived on the enhancement layer: either the " +
			"publisher put every frame on one subgroup or the second stream " +
			"never crossed the relay, and either way there is nothing to shed")
	}

	share := 100 * float64(enhancement) / float64(base+enhancement)
	t.Logf("base layer %d bytes, enhancement layer %d bytes (%.0f%% of the video)",
		base, enhancement, share)

	// Half the frames are on the enhancement layer but not half the bytes:
	// every keyframe is on the base, and a keyframe here is four times the size
	// of a delta. A quarter clears that arithmetic, and clears the jitter in
	// where a three-second window happens to fall.
	if share < 25 {
		t.Errorf("the enhancement layer is only %.0f%% of the video; shedding "+
			"it would not be worth the subscription change it costs", share)
	}
}

// A group can arrive all at once rather than at the frame rate — a publisher
// catching up after a stall, or a relay that had it buffered. Nothing then
// paces the two subgroup streams against each other, and the relay is free to
// drain one far ahead of the other.
//
// What is guaranteed is that the group starts on its keyframe and that whatever
// is delivered is in order and delivered once. What is *not* guaranteed is that
// both layers survive, and the asymmetry is worth being precise about: if the
// enhancement stream runs far enough ahead, the backlog valve concedes its
// indices, and base-layer frames arriving afterwards are below what has already
// gone out. Closing that needs a bounded wait for a layer that has not spoken
// yet — a timer, which this design has so far done without — so for now a burst
// is allowed to cost frames, and is not allowed to cost decodability.
func TestABurstStartsOnAKeyFrameAndStaysInOrder(t *testing.T) {
	alice, bobRec := layeredPair(t, "burst1")

	const frames = 40 // twenty on each layer
	for i := range frames {
		f := layeredVideoFrame(
			uint64(i)*frameStep, i == 0, 300, temporalLayerFor(i))
		if err := alice.WriteFrame(f); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}
	closer := uint64(frames) * frameStep
	if err := alice.WriteFrame(
		layeredVideoFrame(closer, true, 300, 0)); err != nil {
		t.Fatalf("write the keyframe that closes the group: %v", err)
	}

	inGroup := func() []bridge.MediaFrame {
		var got []bridge.MediaFrame
		for _, f := range videoFrames(bobRec) {
			if f.Timestamp < closer {
				got = append(got, f)
			}
		}
		return got
	}
	waitFor(t, "the burst to reach bob", 20*time.Second, func() bool {
		return len(inGroup()) > 0
	})
	time.Sleep(2 * time.Second) // let the rest of it land

	got := inGroup()
	if !got[0].KeyFrame || got[0].Timestamp != 0 {
		t.Errorf("the group started at timestamp %d (keyframe=%v); without its "+
			"keyframe first the whole group is undecodable",
			got[0].Timestamp, got[0].KeyFrame)
	}
	seen := make(map[uint64]bool, len(got))
	for i, f := range got {
		if i > 0 && f.Timestamp <= got[i-1].Timestamp {
			t.Errorf("frame %d went backwards: timestamp %d after %d",
				i, f.Timestamp, got[i-1].Timestamp)
		}
		if seen[f.Timestamp] {
			t.Errorf("frame at %d was delivered twice", f.Timestamp)
		}
		seen[f.Timestamp] = true
	}

	var base int
	for _, f := range got {
		if f.TemporalLayer == 0 {
			base++
		}
	}
	t.Logf("the burst delivered %d of %d frames, %d of them base layer",
		len(got), frames, base)
}

// layeredShare reports what fraction of the video bytes a recorder received
// arrived on the enhancement layer.
func layeredShare(rec *recorder) float64 {
	bytes := rec.layerBytes("video")
	total := bytes[0] + bytes[1]
	if total == 0 {
		return 0
	}
	return float64(bytes[1]) / float64(total)
}

// The enhancement layer is marked disposable, and a link that cannot carry it
// should lose it rather than the picture. §8 lets the publisher say so per
// subgroup, and a relay honouring that resets the one stream with
// DELIVERY_TIMEOUT instead of terminating the subscription — which is the
// difference between a tile at half the frame rate and a tile that is gone.
//
// Measured as a share rather than a count: the bottleneck decides how much
// arrives in total, and the question is how it was divided.
func TestABottleneckCostsTheEnhancementLayerFirst(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()
	alice := layeredPublisher(t, addr, "shed", "alice")

	// Both subscribers watch the same publisher over the same relay, so the
	// only difference between them is the link.
	_, roomyRec := joinRoom(t, addr, "shed", "roomy")
	link := startShaper(t, addr, 32_000, 64)
	_, squeezedRec := joinRoom(t, link.Addr(), "shed", "squeezed")

	waitFor(t, "both subscribers to take alice's video", 20*time.Second, func() bool {
		_, roomy, _, _ := roomyRec.snapshot()
		_, squeezed, _, _ := squeezedRec.snapshot()
		return len(roomy) >= 2 && len(squeezed) >= 2
	})

	stop := make(chan struct{})
	publishPacedLayers(t, alice, stop, temporalLayerFor)
	time.Sleep(12 * time.Second)
	close(stop)
	time.Sleep(time.Second)

	roomyShare, squeezedShare := layeredShare(roomyRec), layeredShare(squeezedRec)
	squeezedBase := squeezedRec.layerBytes("video")[0]
	passed, dropped := link.Stats()

	t.Logf("enhancement layer: %.0f%% of the video on a link with room, "+
		"%.0f%% through the bottleneck (link passed %d, dropped %d)",
		100*roomyShare, 100*squeezedShare, passed, dropped)

	if squeezedBase == 0 {
		t.Fatal("the squeezed subscriber received no base layer at all: the " +
			"layer that is supposed to survive did not")
	}
	if roomyShare == 0 {
		t.Fatal("no enhancement layer even on the link with room; nothing was " +
			"shed because nothing was carried")
	}
	if squeezedShare >= roomyShare {
		t.Errorf("the bottleneck took %.0f%% enhancement against %.0f%% on a "+
			"link with room: the disposable layer was not given up first",
			100*squeezedShare, 100*roomyShare)
	}
}
