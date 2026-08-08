package conf

import (
	"testing"
	"time"

	"tlmst/internal/bridge"
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
	bob.SetVideoInterest(nil)

	waitFor(t, "the video subscription to be dropped", 10*time.Second, func() bool {
		return bobRec.retired(video.Handle)
	})
	if bobRec.retired(audio.Handle) {
		t.Error("audio was dropped with the video: someone speaking off screen " +
			"still has to be heard, and is 32 kbps against video's 1.5 Mbps")
	}

	// Scrolled back.
	bob.SetVideoInterest([]string{alice.State().ID})

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
	bob.SetVideoInterest(nil)
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

// The roster says what a participant publishes, which is not what we happen to
// be subscribed to. Conflating them would show everyone scrolled off screen as
// having switched their camera off, and correct itself only when they scrolled
// back — a lie that moves with the scrollbar.
func TestRosterReportsWhatIsPublishedNotWhatIsSubscribed(t *testing.T) {
	alice, bob, bobRec := alicePublishing(t, "interest3")

	video, _ := bobRec.trackFor("video")
	bob.SetVideoInterest(nil)
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
