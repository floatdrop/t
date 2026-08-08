package telemetry

import (
	"math"
	"testing"
	"time"
)

// epoch is an arbitrary wall clock, and mediaEpoch an arbitrary media clock
// deliberately unrelated to it: the whole point of the tracker is that the
// offset between the two does not matter, only how it changes.
var (
	epoch      = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	mediaEpoch = uint64(500_000_000) // 500 s into some publisher's timeline
)

// feed plays samples in at the audio cadence, with arrivals stretched by
// drift: 0 means every object arrives exactly as produced, 0.01 means wall
// time runs 1% longer than media time, which is a queue filling at 10 ms/s.
func feed(s *SkewTracker, samples int, drift float64) {
	const cadence = 20 * time.Millisecond
	for i := range samples {
		media := mediaEpoch + uint64(i)*uint64(cadence/time.Microsecond)
		elapsed := time.Duration(float64(i) * float64(cadence) * (1 + drift))
		s.Add(epoch.Add(elapsed), media)
	}
}

func TestSkewFlatWhenKeepingUp(t *testing.T) {
	var s SkewTracker
	feed(&s, 200, 0)

	slope, ok := s.Slope()
	if !ok {
		t.Fatal("four seconds of arrivals produced no slope")
	}
	// Zero is a real reading here, and the one that must not be confused with
	// "no data" — a track that is keeping up has to report keeping up.
	if math.Abs(slope) > 0.01 {
		t.Errorf("slope = %v ms/s, want ~0 for arrivals on cadence", slope)
	}
}

func TestSkewTracksAFillingQueue(t *testing.T) {
	var s SkewTracker
	feed(&s, 200, 0.01) // wall time 1% longer than media time

	slope, ok := s.Slope()
	if !ok {
		t.Fatal("no slope from a steadily worsening path")
	}
	if math.Abs(slope-10) > 0.5 {
		t.Errorf("slope = %v ms/s, want ~10 for a queue filling at 1%%", slope)
	}
}

func TestSkewNegativeWhenDraining(t *testing.T) {
	var s SkewTracker
	feed(&s, 200, -0.005)

	slope, ok := s.Slope()
	if !ok {
		t.Fatal("no slope from a draining path")
	}
	if slope > -1 {
		t.Errorf("slope = %v ms/s, want clearly negative while a queue drains", slope)
	}
}

// The publisher's epoch is arbitrary and unsynchronised with ours, so a
// tracker that leaked the absolute offset into its answer would report a
// wildly different number for a path behaving identically.
func TestSkewIgnoresTheClockOffset(t *testing.T) {
	var near, far SkewTracker
	feed(&near, 200, 0.01)

	saved := mediaEpoch
	mediaEpoch = 9_000_000_000 // same path, publisher clock hours away
	defer func() { mediaEpoch = saved }()
	feed(&far, 200, 0.01)

	a, okA := near.Slope()
	b, okB := far.Slope()
	if !okA || !okB {
		t.Fatal("expected both trackers to report a slope")
	}
	if math.Abs(a-b) > 0.01 {
		t.Errorf("slope moved with the publisher's epoch: %v vs %v", a, b)
	}
}

func TestSkewWithoutEnoughHistory(t *testing.T) {
	var s SkewTracker
	feed(&s, skewMinSamples-1, 0)

	if _, ok := s.Slope(); ok {
		t.Error("reported a trend from fewer samples than the minimum")
	}
}

// Enough samples, but all of them from a moment — a line through those is
// noise with a direction, which is what the span gate is for.
func TestSkewWithoutEnoughSpan(t *testing.T) {
	var s SkewTracker
	for i := range 100 {
		s.Add(epoch.Add(time.Duration(i)*time.Millisecond), mediaEpoch+uint64(i)*1000)
	}

	if _, ok := s.Slope(); ok {
		t.Error("reported a trend from a window shorter than the minimum span")
	}
}

func TestSkewIgnoresObjectsWithoutATimestamp(t *testing.T) {
	var s SkewTracker
	for i := range 100 {
		s.Add(epoch.Add(time.Duration(i)*20*time.Millisecond), 0)
	}

	if _, ok := s.Slope(); ok {
		t.Error("an object carrying no timestamp was measured against one")
	}
}

// The window is what makes this a current reading rather than a session
// average: once a path recovers, the trend has to follow it back.
func TestSkewForgetsOldSamples(t *testing.T) {
	var s SkewTracker

	// A bad stretch, then well over a window of a good one.
	const cadence = 20 * time.Millisecond
	at, media := epoch, mediaEpoch
	for range 100 {
		at = at.Add(cadence + 200*time.Microsecond)
		media += uint64(cadence / time.Microsecond)
		s.Add(at, media)
	}
	for range 400 {
		at = at.Add(cadence)
		media += uint64(cadence / time.Microsecond)
		s.Add(at, media)
	}

	slope, ok := s.Slope()
	if !ok {
		t.Fatal("no slope after the path recovered")
	}
	if math.Abs(slope) > 0.01 {
		t.Errorf("slope = %v ms/s, want ~0: the bad stretch should have aged out", slope)
	}
}

// Slope says a call is falling behind; Lag says it has fallen behind. The
// second is what a subscription is rebuilt to escape, and the two answer
// different questions from the same samples.
func TestSkewReportsAccumulatedLag(t *testing.T) {
	var s SkewTracker
	if _, ok := s.Lag(); ok {
		t.Error("reported a lag before any object arrived")
	}

	// Four seconds at 1% slower than the media clock is 40 ms of slip.
	feed(&s, 200, 0.01)

	lag, ok := s.Lag()
	if !ok {
		t.Fatal("no lag after four seconds of arrivals")
	}
	if math.Abs(lag-40) > 5 {
		t.Errorf("lag = %v ms, want ~40 for 4 s at 1%% behind", lag)
	}
}

func TestSkewLagStaysFlatWhenKeepingUp(t *testing.T) {
	var s SkewTracker
	feed(&s, 200, 0)

	lag, ok := s.Lag()
	if !ok {
		t.Fatal("no lag reported")
	}
	if math.Abs(lag) > 1 {
		t.Errorf("lag = %v ms on a path that is keeping up, want ~0", lag)
	}
}

// A suspension exists so the slope is not fitted across a burst this client
// caused. It must not take the running total with it: the events that suspend
// this — a video subscription being rebuilt — happen far more often than the
// slip it is meant to catch, so a lag that reset on every one of them could
// never reach the threshold that acts on it.
func TestSkewCarriesLagAcrossASuspension(t *testing.T) {
	var s SkewTracker
	feed(&s, 200, 0.01) // ~40 ms of slip

	before, ok := s.Lag()
	if !ok {
		t.Fatal("no lag before the suspension")
	}

	s.Suspend(epoch.Add(5*time.Second), time.Second)

	after, ok := s.Lag()
	if !ok {
		t.Fatal("the suspension threw the accumulated lag away")
	}
	if math.Abs(after-before) > 0.01 {
		t.Errorf("lag went from %v to %v ms across a suspension, want it kept", before, after)
	}

	// And it keeps accumulating from there rather than restarting at zero.
	saved := mediaEpoch
	mediaEpoch = uint64(600_000_000)
	defer func() { mediaEpoch = saved }()
	for i := range 200 {
		media := mediaEpoch + uint64(i)*20_000
		elapsed := time.Duration(float64(i) * float64(20*time.Millisecond) * 1.01)
		s.Add(epoch.Add(10*time.Second+elapsed), media)
	}

	total, _ := s.Lag()
	if total < before+30 {
		t.Errorf("lag = %v ms after a second slipping stretch, want it to have "+
			"grown past the %v ms banked before", total, before)
	}
}

// The slope, by contrast, must not see across the gap — that is what the
// suspension is for.
func TestSkewSlopeIgnoresTheSuspendedWindow(t *testing.T) {
	var s SkewTracker
	feed(&s, 200, 0.01)
	s.Suspend(epoch.Add(5*time.Second), time.Second)

	if _, ok := s.Slope(); ok {
		t.Error("reported a slope straight after a suspension, with no samples to fit")
	}
}
