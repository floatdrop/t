package conf

import (
	"testing"
	"time"

	"tlmst/internal/bridge"
	"tlmst/internal/telemetry"
)

// retired reports whether the backend told the frontend to drop this handle.
func (r *recorder) retired(handle uint32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, g := range r.gone {
		if g.Handle == handle {
			return true
		}
	}
	return false
}

// trackFor returns the newest announced track of a kind, and whether there was
// one at all.
func (r *recorder) trackFor(kind string) (bridge.RemoteTrack, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.tracks) - 1; i >= 0; i-- {
		if r.tracks[i].Config.Kind == kind {
			return r.tracks[i], true
		}
	}
	return bridge.RemoteTrack{}, false
}

// alicePublishing joins a publisher declaring both kinds, and returns it with
// a subscriber watching.
func alicePublishing(t *testing.T, room string) (*Room, *Room, *recorder) {
	t.Helper()
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice := publisherWithBothTracks(t, addr, room, "alice")
	bob, bobRec := joinRoom(t, addr, room, "bob")
	waitFor(t, "bob to subscribe to both of alice's tracks", 10*time.Second, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 2
	})
	return alice, bob, bobRec
}

// A tile scrolled out of view is a megabit a second of pictures decoded into a
// scroll region nobody is looking at. Discarding them on arrival would still
// have paid for them on the wire, so the only saving available is not asking —
// which is a decision only the frontend can make, and one it had no way to
// express.
func TestVideoInterestDropsWhatIsNotOnScreen(t *testing.T) {
	alice, bob, bobRec := alicePublishing(t, "interest1")

	video, ok := bobRec.trackFor("video")
	if !ok {
		t.Fatal("bob never subscribed to alice's video")
	}
	audio, ok := bobRec.trackFor("audio")
	if !ok {
		t.Fatal("bob never subscribed to alice's audio")
	}

	// Everyone scrolled off screen.
	bob.SetVideoInterest(nil, nil)

	waitFor(t, "the video subscription to be dropped", 10*time.Second, func() bool {
		return bobRec.retired(video.Handle)
	})
	if bobRec.retired(audio.Handle) {
		t.Error("audio was dropped with the video: someone speaking off screen " +
			"still has to be heard, and is 32 kbps against video's 1.5 Mbps")
	}

	// Scrolled back.
	bob.SetVideoInterest([]string{alice.State().ID}, nil)

	waitFor(t, "the video subscription to come back", 10*time.Second, func() bool {
		again, ok := bobRec.trackFor("video")
		return ok && again.Handle != video.Handle
	})
}

// Declining a track is not the same as failing to get one. missing() drives a
// retry loop that gives up loudly, so reading a deliberate non-subscription as
// a failure would spend the whole budget re-establishing a track nobody asked
// for and then tell the user the participant could not be reached.
func TestDecliningVideoIsNotAFailedSubscription(t *testing.T) {
	_, bob, bobRec := alicePublishing(t, "interest2")

	video, _ := bobRec.trackFor("video")
	bob.SetVideoInterest(nil, nil)
	waitFor(t, "the video subscription to be dropped", 10*time.Second, func() bool {
		return bobRec.retired(video.Handle)
	})

	// Comfortably longer than the retry loop would take to exhaust itself and
	// report.
	time.Sleep(2 * time.Second)

	_, _, _, errs := bobRec.snapshot()
	if len(errs) > 0 {
		t.Errorf("declining video was reported to the user as a failure: %v", errs)
	}
}

// inboundKbps measures what this subscriber is actually being sent over a
// window, the way the debug panel's aggregate does.
func inboundKbps(s *telemetry.Sampler, window time.Duration) float64 {
	s.Sample(time.Now()) // baseline; rates are per interval
	time.Sleep(window)
	return s.Sample(time.Now()).SubscribeKbps
}

// TestVisibilityGatingCutsInboundBitrate is the measurement rather than the
// mechanism: how much does not asking actually save?
//
// Deliberately on an unshaped link. A bottleneck would answer a different
// question — under congestion the received rate is capped by the link whatever
// is subscribed, so both readings would come out near the bottleneck and the
// saving would look like nothing. What is worth knowing is how much less is
// asked for, which is visible only when the path can carry everything.
func TestVisibilityGatingCutsInboundBitrate(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice := publisherWithBothTracks(t, addr, "measure", "alice")
	carol := publisherWithBothTracks(t, addr, "measure", "carol")

	counters := telemetry.NewRegistry()
	bob, bobRec := joinRoomWithCounters(t, addr, "measure", "bob", counters, testLogger(t))
	waitFor(t, "bob to subscribe to both publishers", 15*time.Second, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 4
	})

	stop := make(chan struct{})
	publishPaced(t, alice, stop)
	publishPaced(t, carol, stop)
	defer close(stop)

	sampler := telemetry.NewSampler(counters, nil)
	const window = 3 * time.Second

	everyone := inboundKbps(sampler, window)

	// One of the two tiles scrolled off screen.
	bob.SetVideoInterest([]string{alice.State().ID}, nil)
	time.Sleep(time.Second) // let the subscription actually go away

	oneVisible := inboundKbps(sampler, window)

	saved := 100 * (1 - oneVisible/everyone)
	t.Logf("both tiles visible: %.0f kbps; one scrolled off: %.0f kbps (%.0f%% less)",
		everyone, oneVisible, saved)

	// Two publishers, one video declined. Video is the overwhelming majority
	// of each, so the saving should be close to half — well clear of a third,
	// which is the floor this asserts so it is not measuring timing jitter.
	if saved < 33 {
		t.Errorf("declining one of two videos saved only %.0f%% of inbound "+
			"bitrate (%.0f -> %.0f kbps); the subscription is still being paid for",
			saved, everyone, oneVisible)
	}
}

// The roster says what a participant publishes, which is not what we happen to
// be subscribed to. Conflating them would show everyone scrolled off screen as
// having switched their camera off, and correct itself only when they scrolled
// back — a lie that moves with the scrollbar.
func TestRosterReportsWhatIsPublishedNotWhatIsSubscribed(t *testing.T) {
	alice, bob, bobRec := alicePublishing(t, "interest3")

	video, _ := bobRec.trackFor("video")
	bob.SetVideoInterest(nil, nil)
	waitFor(t, "the video subscription to be dropped", 10*time.Second, func() bool {
		return bobRec.retired(video.Handle)
	})

	waitFor(t, "the roster to still show alice publishing video", 5*time.Second, func() bool {
		_, _, peers, _ := bobRec.snapshot()
		for _, p := range peers {
			if p.ID == alice.State().ID {
				return p.HasVideo
			}
		}
		return false
	})
}

// simulcastPublisher declares both video encodings at sizes a test can tell
// apart, plus audio.
func simulcastPublisher(t *testing.T, addr, room, nickname string) *Room {
	t.Helper()
	r, _ := joinRoom(t, addr, room, nickname)
	for _, cfg := range []*bridge.TrackConfig{
		{Kind: "video", Codec: "avc1.42e01f", Width: 1280, Height: 720},
		{Kind: KindVideoLow, Codec: "avc1.42e01f", Width: 640, Height: 360},
		{Kind: "audio", Codec: "opus", SampleRate: 48000, Channels: 1},
	} {
		if err := r.DeclareTrack(cfg); err != nil {
			t.Fatalf("declare %s: %v", cfg.Kind, err)
		}
	}
	return r
}

// A tile is only as big as the grid draws it, and the publisher cannot know
// how big that is — it sizes its encoders from its own window, which is not
// the one the picture lands in. So the subscriber chooses, and choosing is
// only possible if both encodings are offered and told apart.
func TestSubscriberTakesTheLayerItsTileCanUse(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice := simulcastPublisher(t, addr, "layers", "alice")
	bob, bobRec := joinRoom(t, addr, "layers", "bob")

	waitFor(t, "bob to take the full picture by default", 10*time.Second, func() bool {
		v, ok := bobRec.trackFor("video")
		return ok && v.Config.Width == 1280
	})
	full, _ := bobRec.trackFor("video")

	// The tile turns out to be a thumbnail.
	bob.SetVideoInterest([]string{alice.State().ID}, []string{alice.State().ID})

	waitFor(t, "bob to move to the smaller encoding", 10*time.Second, func() bool {
		v, ok := bobRec.trackFor("video")
		return ok && v.Config.Width == 640
	})
	small, _ := bobRec.trackFor("video")
	if small.Handle == full.Handle {
		t.Error("the layer changed without a fresh handle, so the frontend " +
			"would keep decoding the old one into the same sink")
	}

	// Expanded back to full size.
	bob.SetVideoInterest([]string{alice.State().ID}, nil)
	waitFor(t, "bob to move back to the full picture", 10*time.Second, func() bool {
		v, ok := bobRec.trackFor("video")
		return ok && v.Config.Width == 1280
	})
}

// A publisher on an older build offers one video track and no small layer.
// Wanting the small one has to fall back to what exists, because the
// alternative is a permanently blank tile for a peer who is publishing
// perfectly well.
func TestWantingASmallLayerNobodyOffersFallsBack(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice := publisherWithBothTracks(t, addr, "fallback", "alice")
	bob, bobRec := joinRoom(t, addr, "fallback", "bob")
	waitFor(t, "bob to subscribe", 10*time.Second, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 2
	})

	bob.SetVideoInterest([]string{alice.State().ID}, []string{alice.State().ID})
	time.Sleep(time.Second)

	if _, ok := bobRec.trackFor("video"); !ok {
		t.Fatal("no video subscription at all")
	}
	_, _, _, errs := bobRec.snapshot()
	if len(errs) > 0 {
		t.Errorf("asking for a layer the publisher does not offer was reported "+
			"as a failure: %v", errs)
	}
}
