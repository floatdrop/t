package conf

import (
	"log/slog"
	"testing"
	"time"

	"github.com/floatdrop/moq-go/pkg/moqt/session"

	"tlmst/internal/bridge"
	"tlmst/internal/telemetry"
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

// layeredPublisher joins a room and declares audio plus a video track that says
// it carries two temporal layers, which is what the frontend's L1T2 encoder
// declares and what tells a subscriber how many subgroups to expect.
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
		TemporalLayers: 2,
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
// held is a group whose tail never reaches the decoder, which on a two-second
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

// A group can arrive all at once rather than at the frame rate — a backfilled
// group replayed to someone who has just joined, or a publisher catching up
// after a stall. Nothing then paces the two subgroup streams against each
// other, and the relay is free to drain one far ahead of the other.
//
// What must survive that is the base layer, whole and in order: it is what makes
// the group decodable at all, and a hole in it is a hole in the picture. The
// enhancement layer is best-effort here and deliberately so. A group that has
// only seen the base cannot tell a layer that was shed from one that has not
// arrived yet, and waiting for it is what froze tiles for four seconds at a
// time; conceding it costs this group half its frame rate instead.
func TestABurstDeliversTheBaseLayerWhole(t *testing.T) {
	alice, bobRec := layeredPair(t, "burst1")

	const frames = 40 // twenty on each layer
	for i := range frames {
		f := layeredVideoFrame(
			uint64(i)*frameStep, i == 0, 300, temporalLayerFor(i))
		if err := alice.WriteFrame(f); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}
	// Ends the group, and is excluded from what follows.
	closer := uint64(frames) * frameStep
	if err := alice.WriteFrame(
		layeredVideoFrame(closer, true, 300, 0)); err != nil {
		t.Fatalf("write the keyframe that closes the group: %v", err)
	}

	base := func() []bridge.MediaFrame {
		var got []bridge.MediaFrame
		for _, f := range videoFrames(bobRec) {
			if f.Timestamp < closer && f.TemporalLayer == 0 {
				got = append(got, f)
			}
		}
		return got
	}
	waitFor(t, "every base-layer frame of the burst to reach bob", 15*time.Second,
		func() bool { return len(base()) >= frames/2 })

	got := base()
	if len(got) != frames/2 {
		t.Fatalf("bob received %d of %d base-layer frames written back to back",
			len(got), frames/2)
	}
	for i, f := range got {
		if want := uint64(2*i) * frameStep; f.Timestamp != want {
			t.Fatalf("base frame %d of the burst arrived out of order: "+
				"timestamp %d, want %d", i, f.Timestamp, want)
		}
	}

	// Reported, not required: how much of the disposable layer a burst happens
	// to carry says something about the path, and nothing about correctness.
	var enhancement int
	for _, f := range videoFrames(bobRec) {
		if f.Timestamp < closer && f.TemporalLayer == 1 {
			enhancement++
		}
	}
	t.Logf("the burst carried %d of %d enhancement frames alongside a whole "+
		"base layer", enhancement, frames/2)
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

// The step the whole layered layout was for. When the relay gives up on a
// subscription, the ladder's first answer should be to decline the top temporal
// layer rather than to change encoding: same track, same decoder, same picture
// at half the frame rate, and no keyframe to wait for. Only the frames stop —
// nothing is torn down.
func TestTheFirstStepDownIsALayerNotAnEncoding(t *testing.T) {
	// A relay that permits Range Filters, which moq-go's does not by default:
	// MAX_FILTER_RANGES is zero unless set, and zero prohibits them. The rung
	// is dormant against a relay that says nothing — see
	// TestTheLayerRungIsSkippedWhereFiltersAreRefused for that half.
	relayServer := startRelayWith(t, session.WithMaxFilterRanges(4))
	addr := relayServer.Addr()

	// Declares both encodings, and says the primary carries two layers — so
	// the ladder has both a rung to skip to and a layer to shed first.
	alice, _ := joinRoom(t, addr, "rung", "alice")
	for _, cfg := range []*bridge.TrackConfig{
		{Kind: "video", Codec: "avc1.42e01f", Width: 1280, Height: 720, TemporalLayers: 2},
		{Kind: KindVideoLow, Codec: "avc1.42e01f", Width: 640, Height: 360},
		{Kind: "audio", Codec: "opus", SampleRate: 48000, Channels: 1},
	} {
		if err := alice.DeclareTrack(cfg); err != nil {
			t.Fatalf("declare %s: %v", cfg.Kind, err)
		}
	}

	link := startShaper(t, addr, 32_000, 64)
	spy := newLogSpy(t)
	_, bobRec := joinRoomWithCounters(
		t, link.Addr(), "rung", "bob", telemetry.NewRegistry(), slog.New(spy))

	waitFor(t, "bob to take the full picture", 15*time.Second, func() bool {
		v, ok := bobRec.trackFor("video")
		return ok && v.Config.Width == 1280
	})
	full, _ := bobRec.trackFor("video")

	stop := make(chan struct{})
	publishPacedLayers(t, alice, stop, temporalLayerFor)
	defer close(stop)

	waitFor(t, "the relay to give up on the full picture", 30*time.Second, func() bool {
		return spy.sawAttr("msg",
			"the relay stopped forwarding a track: we are not keeping up")
	})
	waitFor(t, "the ladder to step down to the base layer", 15*time.Second, func() bool {
		return spy.sawAttr("to", "base-only")
	})

	// A *new* handle at the same size, which is the assertion that means
	// anything: trackFor reports the newest track announced, so a demotion whose
	// SUBSCRIBE was rejected leaves the old one in place and an assertion on the
	// width alone passes without a subscription existing. That is how a rung
	// that never worked against any moq-go relay went unnoticed until two
	// clients were run against a deployed one.
	waitFor(t, "the base-only subscription to be announced", 20*time.Second, func() bool {
		v, ok := bobRec.trackFor("video")
		return ok && v.Handle != full.Handle
	})

	v, _ := bobRec.trackFor("video")
	if v.Config.Width != 1280 {
		t.Errorf("the first step down changed encoding to %dx%d; the point of "+
			"the layer rung is that the picture stays the same size",
			v.Config.Width, v.Config.Height)
	}
	if spy.sawAttr("to", "small") {
		t.Error("the ladder went straight to the smaller encoding, skipping " +
			"the cheaper step it now has")
	}
	_, _, _, errs := bobRec.snapshot()
	for _, e := range errs {
		t.Errorf("the user was told a track could not be reached: %q", e)
	}
}

// A relay is entitled to refuse §5.1.3 Range Filters, and one that says nothing
// about MAX_FILTER_RANGES has refused them: §10.3.1.6 makes the default zero.
// Sending one anyway is not ignored — the SUBSCRIBE is rejected outright with
// INVALID_FILTER.
//
// So the rung that is expressed as a filter has to be skipped there, exactly as
// it is skipped against a publisher with no layer to shed. Without this the
// first congestion event took the rung, lost the subscription, exhausted the
// retry loop and gave up on that participant's video for the rest of the call —
// strictly worse than the demotion the rung was an improvement on. Found by
// running two clients against a deployed relay; every in-process test passed,
// because the test relay permits filters unless told otherwise.
func TestTheLayerRungIsSkippedWhereFiltersAreRefused(t *testing.T) {
	relayServer := startRelayWith(t, session.WithMaxFilterRanges(0))
	addr := relayServer.Addr()

	alice, _ := joinRoom(t, addr, "nofilter", "alice")
	for _, cfg := range []*bridge.TrackConfig{
		{Kind: "video", Codec: "avc1.42e01f", Width: 1280, Height: 720, TemporalLayers: 2},
		{Kind: KindVideoLow, Codec: "avc1.42e01f", Width: 640, Height: 360},
		{Kind: "audio", Codec: "opus", SampleRate: 48000, Channels: 1},
	} {
		if err := alice.DeclareTrack(cfg); err != nil {
			t.Fatalf("declare %s: %v", cfg.Kind, err)
		}
	}

	link := startShaper(t, addr, 32_000, 64)
	spy := newLogSpy(t)
	_, bobRec := joinRoomWithCounters(
		t, link.Addr(), "nofilter", "bob", telemetry.NewRegistry(), slog.New(spy))

	waitFor(t, "bob to take the full picture", 15*time.Second, func() bool {
		v, ok := bobRec.trackFor("video")
		return ok && v.Config.Width == 1280
	})

	stop := make(chan struct{})
	publishPacedLayers(t, alice, stop, temporalLayerFor)
	defer close(stop)

	waitFor(t, "the relay to give up on the full picture", 30*time.Second, func() bool {
		return spy.sawAttr("msg",
			"the relay stopped forwarding a track: we are not keeping up")
	})

	// Straight to the smaller encoding: the rung in between cannot be asked
	// for here, so it is not a rung.
	waitFor(t, "video to come back at the smaller encoding", 20*time.Second, func() bool {
		v, ok := bobRec.trackFor("video")
		return ok && v.Config.Width == 640
	})

	if spy.sawAttr("to", "base-only") {
		t.Error("the ladder stepped to base-only against a relay that refuses " +
			"the filter it is expressed as")
	}
	_, _, _, errs := bobRec.snapshot()
	for _, e := range errs {
		t.Errorf("the user was told a track could not be reached: %q", e)
	}
}
