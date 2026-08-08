package telemetry

import (
	"sync"
	"time"
)

// SkewTracker measures whether media is arriving progressively later than the
// clock that produced it says it should.
//
// # Why the derivative and not the value
//
// For one object, arrival wall time minus its media timestamp is a number with
// no absolute meaning: the publisher's media clock starts at an arbitrary
// epoch, the two machines' clocks are not synchronised, and encode latency sits
// in the middle. What that sum has going for it is that every term in it is
// *constant* except the one worth knowing — how long the object waited in a
// queue on the way here. So the offset is meaningless and its slope is not.
// Differentiating cancels the epoch, the clock offset and the fixed encode
// cost, and leaves the rate at which a bottleneck queue is filling or draining.
//
// A steadily positive slope means a queue is building somewhere on the inbound
// path: more is being sent to us than is getting through, and the excess is
// turning into latency. That is the condition that precedes the relay giving up
// on this subscriber, and unlike the relay's verdict it can be seen coming.
//
// # What it is not
//
// It is not a bandwidth estimate. It says the path is over capacity, never by
// how much, and it cannot say anything at all about capacity that is going
// unused — an idle link and a link with exactly enough headroom both read zero.
//
// # Noise floor
//
// The two clocks drift apart, which is indistinguishable from a slowly filling
// queue. Consumer crystals are specified around 50 ppm, so the worst honest
// drift contributes 0.05 ms/s. A bottleneck queue that is actually filling
// moves tens of ms per second, so there are roughly three orders of magnitude
// between the noise and the signal — but a threshold set below about 1 ms/s is
// measuring the clocks, not the network.
type SkewTracker struct {
	mu sync.Mutex
	// samples holds the trailing skewWindow of arrivals, oldest first.
	samples []skewSample
	// base is the first skew seen, subtracted from every sample so the stored
	// numbers stay small and readable. It cancels in the slope regardless.
	base     float64
	haveBase bool
}

type skewSample struct {
	at time.Time
	// skew is arrival minus media timestamp, in milliseconds, relative to base.
	skew float64
}

const (
	// skewWindow is how much history the trend is fitted over. Long enough to
	// average out per-object jitter, short enough that a queue which started
	// filling is reported while it still matters.
	skewWindow = 4 * time.Second
	// skewMinSamples and skewMinSpan gate reporting a slope at all. A line
	// through a handful of points collected over a moment is noise with a
	// direction, and it would arrive exactly at subscription start, when a
	// decoder is spinning up and arrivals are least regular.
	skewMinSamples = 20
	skewMinSpan    = time.Second
)

// Add records one object's arrival. mediaMicros is the object's media
// timestamp in microseconds; zero means the publisher sent none, which leaves
// nothing to compare against.
//
// Feed this from one track only — a track whose objects are produced on a
// fixed cadence, which in this app means audio. Video frames are emitted at
// whatever rate the encoder finishes them and a keyframe takes materially
// longer than a delta frame, so encode time stops being the constant the whole
// method depends on.
func (s *SkewTracker) Add(now time.Time, mediaMicros uint64) {
	if mediaMicros == 0 {
		return
	}
	// Differenced as integers before becoming a float: both are large absolute
	// microsecond counts, and float64 subtraction of two such values throws
	// away the low bits that are the entire measurement.
	skew := float64(now.UnixMicro()-int64(mediaMicros)) / 1000

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.haveBase {
		s.base, s.haveBase = skew, true
	}
	s.samples = append(s.samples, skewSample{at: now, skew: skew - s.base})

	cutoff := now.Add(-skewWindow)
	drop := 0
	for drop < len(s.samples) && s.samples[drop].at.Before(cutoff) {
		drop++
	}
	if drop > 0 {
		s.samples = append(s.samples[:0], s.samples[drop:]...)
	}
}

// Slope returns the trend in milliseconds of accumulated delay per second of
// wall clock, and whether there was enough history to fit one.
//
// Positive means arrivals are falling behind the media clock: a queue on the
// inbound path is filling. Negative means it is draining. Around zero means
// whatever the path is carrying, it is carrying it at the rate it is produced.
func (s *SkewTracker) Slope() (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.samples) < skewMinSamples {
		return 0, false
	}
	first := s.samples[0].at
	if s.samples[len(s.samples)-1].at.Sub(first) < skewMinSpan {
		return 0, false
	}

	// Ordinary least squares through (seconds since first, skew). The samples
	// are irregularly spaced — they arrive as objects do — so this is fitted
	// against real elapsed time rather than sample index.
	n := float64(len(s.samples))
	var sumT, sumY float64
	for _, sm := range s.samples {
		sumT += sm.at.Sub(first).Seconds()
		sumY += sm.skew
	}
	meanT, meanY := sumT/n, sumY/n

	var num, den float64
	for _, sm := range s.samples {
		dt := sm.at.Sub(first).Seconds() - meanT
		num += dt * (sm.skew - meanY)
		den += dt * dt
	}
	if den == 0 {
		return 0, false // every sample landed on the same instant
	}
	return num / den, true
}
