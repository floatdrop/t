package conf

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"tlmst/internal/bridge"
	"tlmst/internal/telemetry"
)

// logSpy records what a room logged, so a test can assert on a signal whose
// only output is a log line.
//
// The store is held separately and shared, because a Room does not log through
// the handler it was given: Join derives one with log.With(...), and every
// remote derives another from that. A handler that embedded slog.Handler and
// left WithAttrs alone would hand those derivations the *inner* handler and
// take itself out of the chain — recording nothing while the lines it was
// meant to capture sail past it into the test output.
type logSpy struct {
	slog.Handler
	store *spyStore
}

type spyStore struct {
	mu      sync.Mutex
	records []map[string]string
}

func newLogSpy(t *testing.T) *logSpy {
	return &logSpy{Handler: testLogger(t).Handler(), store: &spyStore{}}
}

func (s *logSpy) Handle(ctx context.Context, r slog.Record) error {
	attrs := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	s.store.mu.Lock()
	s.store.records = append(s.store.records, attrs)
	s.store.mu.Unlock()
	return s.Handler.Handle(ctx, r)
}

// Enabled is unconditionally true. The handler underneath may be discarding —
// testLogger discards unless the test was run verbose — and delegating would
// make these assertions pass or fail depending on how the suite was invoked.
func (s *logSpy) Enabled(context.Context, slog.Level) bool { return true }

func (s *logSpy) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logSpy{Handler: s.Handler.WithAttrs(attrs), store: s.store}
}

func (s *logSpy) WithGroup(name string) slog.Handler {
	return &logSpy{Handler: s.Handler.WithGroup(name), store: s.store}
}

// sawAttr reports whether any record carried key=value. Only attributes from
// the call site are visible: anything bound by With lives in the handler
// chain, not in the record.
func (s *logSpy) sawAttr(key, value string) bool {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, r := range s.store.records {
		if r[key] == value {
			return true
		}
	}
	return false
}

// joinRoomWithCounters is joinRoom with the telemetry registry and logger
// supplied, so a test can read what the sampler would report and what the room
// said while reporting it.
func joinRoomWithCounters(
	t *testing.T,
	addr, room, nickname string,
	counters *telemetry.Registry,
	log *slog.Logger,
) (*Room, *recorder) {
	t.Helper()
	rec := newRecorder()
	r, err := Join(t.Context(), log, rec, counters, Config{
		Relay:    addr,
		Room:     room,
		Nickname: nickname,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("join as %s: %v", nickname, err)
	}
	t.Cleanup(r.Close)
	return r, rec
}

// publishPaced writes audio and video at something like a real call's cadence
// until stop is closed: audio every 20 ms, video at 30 fps with a keyframe
// every two seconds. Together they come to roughly half a megabit, which is
// the figure the bottleneck in these tests is set against.
//
// Flat video: every frame on the base layer, which is one subgroup and the
// shape every track had before temporal layers existed. See layers_test.go for
// the publisher that emits two.
func publishPaced(t *testing.T, room *Room, stop <-chan struct{}) {
	t.Helper()
	publishPacedLayers(t, room, stop, func(int) uint8 { return 0 })
}

// publishPacedLayers is publishPaced with each video frame's temporal layer
// chosen by layerFor, which is what decides how many subgroups a group opens.
func publishPacedLayers(
	t *testing.T,
	room *Room,
	stop <-chan struct{},
	layerFor func(frame int) uint8,
) {
	t.Helper()
	const (
		audioStep = 20 * time.Millisecond
		videoStep = 33 * time.Millisecond
		audioSize = 80
		videoSize = 2000
		keySize   = 8000
	)

	go func() {
		tick := time.NewTicker(audioStep)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			_ = room.WriteFrame(audioFrame(uint64(i)*20_000, audioSize))
		}
	}()

	go func() {
		tick := time.NewTicker(videoStep)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			key := i%60 == 0 // every ~2 s
			size := videoSize
			if key {
				size = keySize
			}
			_ = room.WriteFrame(
				layeredVideoFrame(uint64(i)*33_000, key, size, layerFor(i)))
		}
	}()
}

// publisherWithBothTracks joins a room and declares audio and video.
func publisherWithBothTracks(t *testing.T, addr, room, nickname string) *Room {
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

// skewFor reads what the sampler would report for a participant's audio track.
func skewFor(s *telemetry.Sampler, id string) (float64, bool) {
	m := s.Sample(time.Now())
	for _, tr := range m.Tracks {
		if tr.Label != telemetry.InPrefix+id+"/"+AudioTrack {
			continue
		}
		if tr.SkewMillisPerSec == nil {
			return 0, false
		}
		return *tr.SkewMillisPerSec, true
	}
	return 0, false
}

// The drift meter and the relay's overload verdict are both claims about a
// path that cannot carry what is being sent, and neither can be observed on
// loopback: there is no queue to fill there and nothing ever falls behind. So
// these two put a real bottleneck in front of the subscriber — a userspace UDP
// forwarder with a finite queue, see shaper — and watch what comes out.
//
// They are a pair on purpose. A meter that only ever rises is not measuring
// congestion, it is measuring that it is switched on; the healthy-path case is
// what makes the congested one mean anything.

func TestSkewRisesOnACongestedPath(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()
	alice := publisherWithBothTracks(t, addr, "congested", "alice")

	// Around half what the publisher sends, so the queue fills steadily
	// rather than instantly.
	link := startShaper(t, addr, 32_000, 64)
	counters := telemetry.NewRegistry()
	_, bobRec := joinRoomWithCounters(
		t, link.Addr(), "congested", "bob", counters, testLogger(t))

	waitFor(t, "bob to subscribe to alice", 10*time.Second, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 2
	})

	sampler := telemetry.NewSampler(counters, nil)
	stop := make(chan struct{})
	publishPaced(t, alice, stop)
	defer close(stop)

	// The rise is steep and early — the queue is filling from the first
	// second — so this watches for it rather than waiting out a fixed run.
	var peak float64
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		if slope, ok := skewFor(sampler, alice.State().ID); ok {
			peak = math.Max(peak, slope)
			if peak > congestedDriftFloor {
				break
			}
		}
	}

	passed, dropped := link.Stats()
	t.Logf("peak drift %+.2f ms/s; link passed %d packets, dropped %d", peak, passed, dropped)

	if peak <= congestedDriftFloor {
		t.Errorf("peak drift %+.2f ms/s over a bottleneck, want more than %.0f: "+
			"a filling queue was not reported", peak, congestedDriftFloor)
	}
	if dropped == 0 {
		t.Error("the bottleneck dropped nothing; the path was not actually congested")
	}

	// This one stops as soon as the rise is unmistakable, which is well before
	// the relay's lag window runs out, so it does not reach the overload
	// verdict. That has its own test below.
}

// TestRelayReportsWhenWeCannotKeepUp is the other inbound signal: not our
// measurement of the path, but the relay's verdict on us.
//
// Its fanout measures how long each forwarded object waits in this
// subscriber's send queue, and once one has waited longer than the lag window
// it resets the subgroup stream with TOO_FAR_BEHIND and terminates the
// subscription. Read as a bare error that is indistinguishable from a group
// ending normally, which is what it used to be. So this squeezes the link hard
// enough to earn the verdict and checks that it arrives understood.
func TestRelayReportsWhenWeCannotKeepUp(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()
	alice := publisherWithBothTracks(t, addr, "overloaded", "alice")

	// The same bottleneck as the drift test, and deliberately not a harsher
	// one. The relay checks the lag window when its writer *dequeues* an
	// object, so a subscriber whose link is catastrophically slow leaves that
	// writer blocked on flow control and rarely dequeuing — and is noticed
	// later than one merely oversubscribed. Squeezing harder here delayed the
	// verdict past twenty seconds; this is the load that earns it in about
	// four.
	link := startShaper(t, addr, 32_000, 64)
	spy := newLogSpy(t)
	_, bobRec := joinRoomWithCounters(
		t, link.Addr(), "overloaded", "bob", telemetry.NewRegistry(), slog.New(spy))

	waitFor(t, "bob to subscribe to alice", 15*time.Second, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 2
	})

	stop := make(chan struct{})
	publishPaced(t, alice, stop)
	defer close(stop)

	// The relay's default lag window is two seconds, so the verdict cannot
	// arrive sooner than that however hard the link is squeezed.
	waitFor(t, "the relay to give up on a track", 20*time.Second, func() bool {
		return spy.sawAttr("msg",
			"the relay stopped forwarding a track: we are not keeping up")
	})

	if !spy.sawAttr("code", "TOO_FAR_BEHIND") {
		t.Error("the relay's overload verdict was reported without its code, " +
			"so it arrived indistinguishable from a group ending")
	}
	passed, dropped := link.Stats()
	t.Logf("verdict received; link passed %d packets, dropped %d", passed, dropped)
}

func TestSkewStaysFlatOnAHealthyPath(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()
	alice := publisherWithBothTracks(t, addr, "healthy", "alice")

	// The same forwarder, so the only difference from the congested case is
	// how much it will carry: an order of magnitude above the offered load,
	// which is a link with room rather than no link at all.
	link := startShaper(t, addr, 1_000_000, 1024)
	counters := telemetry.NewRegistry()
	_, bobRec := joinRoomWithCounters(
		t, link.Addr(), "healthy", "bob", counters, testLogger(t))

	waitFor(t, "bob to subscribe to alice", 10*time.Second, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 2
	})

	sampler := telemetry.NewSampler(counters, nil)
	stop := make(chan struct{})
	publishPaced(t, alice, stop)
	defer close(stop)

	// Long enough for the window to fill several times over, so a trend that
	// drifted would have shown by now.
	time.Sleep(6 * time.Second)

	slope, ok := skewFor(sampler, alice.State().ID)
	if !ok {
		t.Fatal("no drift reported after six seconds of steady audio")
	}
	passed, dropped := link.Stats()
	t.Logf("drift %+.2f ms/s; link passed %d packets, dropped %d", slope, passed, dropped)

	// The panel warns past 1 ms/s, which is where clock drift stops being a
	// plausible explanation. A link with room has to sit inside that, or the
	// threshold is measuring the meter rather than the network.
	if math.Abs(slope) >= 1 {
		t.Errorf("drift %+.2f ms/s on a link with room, want under 1: "+
			"the meter reads congestion where there is none", slope)
	}
}

// congestedDriftFloor is what counts as "the queue is filling" for the test.
// Well above the panel's 1 ms/s warning threshold and far below what a real
// bottleneck produces — the congested path measured in hundreds — so the
// assertion is not tuned to a particular machine's timing.
const congestedDriftFloor float64 = 5

// The relay does not merely reset a stream when it gives up on a subscriber:
// it terminates the subscription. Nothing else rebuilds one, so before this
// the tile that happened to went blank for the rest of the call — the exact
// failure the reset was warning about, made permanent by not being answered.
//
// The answer is not to re-subscribe as we were, which would offer back the
// load that was just refused and be cut off again a lag window later. It is to
// come back smaller.
func TestOverloadDemotesRatherThanFreezing(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()
	alice := simulcastPublisher(t, addr, "demote", "alice")

	link := startShaper(t, addr, 32_000, 64)
	spy := newLogSpy(t)
	bobRoom, bobRec := joinRoomWithCounters(
		t, link.Addr(), "demote", "bob", telemetry.NewRegistry(), slog.New(spy))
	_ = bobRoom

	waitFor(t, "bob to take the full picture", 15*time.Second, func() bool {
		v, ok := bobRec.trackFor("video")
		return ok && v.Config.Width == 1280
	})

	stop := make(chan struct{})
	publishPaced(t, alice, stop)
	defer close(stop)

	waitFor(t, "the relay to give up on the full picture", 25*time.Second, func() bool {
		return spy.sawAttr("msg",
			"the relay stopped forwarding a track: we are not keeping up")
	})

	// The subscription the relay killed is rebuilt, and rebuilt smaller.
	waitFor(t, "video to come back at the smaller encoding", 20*time.Second, func() bool {
		v, ok := bobRec.trackFor("video")
		return ok && v.Config.Width == 640
	})

	if !spy.sawAttr("to", "small") {
		t.Error("the demotion was not reported as a step down the ladder")
	}
	_, _, _, errs := bobRec.snapshot()
	if len(errs) > 0 {
		t.Errorf("being demoted was reported to the user as a failure: %v", errs)
	}
}
