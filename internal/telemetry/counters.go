package telemetry

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"tlmst/internal/bridge"
)

// TrackCounter accumulates traffic for one MOQ track. Publishers and
// subscribers bump it as they write and read objects; the sampler turns
// the deltas into the rates the debug panel plots.
type TrackCounter struct {
	bytes   atomic.Uint64
	objects atomic.Uint64
	groups  atomic.Uint64
	// skew is fed only for tracks whose arrival timing is worth trending —
	// see AddArrival. It stays empty on every other track, and an unfed
	// tracker reports no slope rather than a zero one.
	skew SkewTracker
}

// AddObject records one object of n payload bytes.
func (c *TrackCounter) AddObject(n int) {
	c.bytes.Add(uint64(n))
	c.objects.Add(1)
}

// AddGroup records the start of one group.
func (c *TrackCounter) AddGroup() { c.groups.Add(1) }

// AddArrival records when an object arrived against the media timestamp it
// carries, for the inbound delay trend. See [SkewTracker] for which tracks
// this is meaningful on.
func (c *TrackCounter) AddArrival(now time.Time, mediaMicros uint64) {
	c.skew.Add(now, mediaMicros)
}

// Lag reports how far this track has slipped behind where it started, in
// milliseconds, and whether there is anything to report. See [SkewTracker.Lag].
func (c *TrackCounter) Lag() (float64, bool) { return c.skew.Lag() }

func (c *TrackCounter) read() trackSample {
	s := trackSample{
		bytes:   c.bytes.Load(),
		objects: c.objects.Load(),
		groups:  c.groups.Load(),
	}
	s.skew, s.hasSkew = c.skew.Slope()
	s.lag, _ = c.skew.Lag()
	return s
}

// trackSample is one read of a TrackCounter: the monotonic totals the sampler
// differences, plus the skew trend, which is already a rate and is not.
type trackSample struct {
	bytes   uint64
	objects uint64
	groups  uint64
	skew    float64
	hasSkew bool
	lag     float64
}

// Registry holds the counters for every live track, keyed by a display
// label ("out/video", "in/9f3c1a/audio"), plus the direction split the
// panel shows as aggregate publish/subscribe bitrate.
type Registry struct {
	mu     sync.RWMutex
	tracks map[string]*TrackCounter
	order  []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tracks: make(map[string]*TrackCounter)}
}

// Track returns the counter for label, creating it on first use.
func (r *Registry) Track(label string) *TrackCounter {
	r.mu.RLock()
	c, ok := r.tracks[label]
	r.mu.RUnlock()
	if ok {
		return c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.tracks[label]; ok {
		return c
	}
	c = &TrackCounter{}
	r.tracks[label] = c
	r.order = append(r.order, label)
	return c
}

// Forget drops a track's counter — called when a subscription ends, so a
// departed participant stops appearing in the panel.
func (r *Registry) Forget(label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tracks, label)
	for i, l := range r.order {
		if l == label {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// Reset clears every counter, for a fresh session.
func (r *Registry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tracks = make(map[string]*TrackCounter)
	r.order = nil
}

func (r *Registry) snapshot() map[string]trackSample {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]trackSample, len(r.tracks))
	for label, c := range r.tracks {
		out[label] = c.read()
	}
	return out
}

// Sampler converts the monotonic counters in a Registry and a QUICTrace
// into the interval rates that bridge.Metrics carries. One Sampler serves
// one session; call Sample on a fixed tick.
type Sampler struct {
	registry *Registry
	trace    *QUICTrace

	last     time.Time
	lastQUIC QUICSnapshot
	lastPer  map[string]trackSample
}

// NewSampler returns a sampler over the given sources. trace may be nil
// before a session is dialed, in which case transport fields stay zero.
func NewSampler(reg *Registry, trace *QUICTrace) *Sampler {
	return &Sampler{registry: reg, trace: trace, lastPer: map[string]trackSample{}}
}

// Sample returns the metrics for the interval since the previous call.
// The first call establishes the baseline and reports zero rates.
func (s *Sampler) Sample(now time.Time) bridge.Metrics {
	m := bridge.Metrics{TimeMillis: now.UnixMilli()}

	elapsed := now.Sub(s.last).Seconds()
	first := s.last.IsZero()
	s.last = now
	if elapsed <= 0 {
		elapsed = 1
	}

	if s.trace != nil {
		q := s.trace.Snapshot()
		m.SmoothedRTTMillis = millis(q.SmoothedRTT)
		m.MinRTTMillis = millis(q.MinRTT)
		m.LatestRTTMillis = millis(q.LatestRTT)
		// With no RTT sample in the interval there is no peak to report;
		// the smoothed value keeps the series continuous instead of
		// dropping it to zero, which would read as a collapse in latency.
		if q.PeakRTT > 0 {
			m.PeakRTTMillis = millis(q.PeakRTT)
		} else {
			m.PeakRTTMillis = millis(q.SmoothedRTT)
		}
		m.CongestionWindow = q.CongestionWindow
		m.BytesInFlight = q.BytesInFlight
		m.PacketsInFlight = q.PacketsInFlight
		m.CongestionState = q.CongestionState
		m.PacketsSent = q.PacketsSent
		m.PacketsReceived = q.PacketsReceived
		m.PacketsLost = q.PacketsLost

		if !first {
			sent := diff(q.PacketsSent, s.lastQUIC.PacketsSent)
			lost := diff(q.PacketsLost, s.lastQUIC.PacketsLost)
			m.PacketsSentPerSec = float64(sent) / elapsed
			m.PacketsLostPerSec = float64(lost) / elapsed
			// Loss is expressed against packets sent in the same
			// interval; with nothing sent there is no rate to report.
			if sent > 0 {
				m.LossPercent = 100 * float64(lost) / float64(sent)
			}
			m.SendKbps = kbps(diff(q.BytesSent, s.lastQUIC.BytesSent), elapsed)
			m.ReceiveKbps = kbps(diff(q.BytesReceived, s.lastQUIC.BytesReceived), elapsed)
		}
		s.lastQUIC = q
	}

	per := s.registry.snapshot()
	labels := make([]string, 0, len(per))
	for label := range per {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	for _, label := range labels {
		cur := per[label]
		prev := s.lastPer[label]
		tm := bridge.TrackMetrics{
			Label:   label,
			Objects: cur.objects,
			Groups:  cur.groups,
		}
		// Already a rate, so it is reported as read rather than differenced,
		// and it is a pointer because zero is a real answer here — "arriving
		// exactly on cadence" and "not enough history to say" are opposite
		// readings that would otherwise both render as 0.
		if cur.hasSkew {
			skew := cur.skew
			tm.SkewMillisPerSec = &skew
			lag := cur.lag
			tm.LagMillis = &lag
		}
		if !first {
			tm.Kbps = kbps(diff(cur.bytes, prev.bytes), elapsed)
			objDelta := float64(diff(cur.objects, prev.objects)) / elapsed
			grpDelta := float64(diff(cur.groups, prev.groups)) / elapsed
			// "out/" labels are the local publications; everything
			// else is inbound. Splitting here keeps the aggregate
			// bitrates in step with the per-track rows.
			if isOutbound(label) {
				m.PublishKbps += tm.Kbps
				m.ObjectsOutPerSec += objDelta
				m.GroupsOutPerSec += grpDelta
			} else {
				m.SubscribeKbps += tm.Kbps
				m.ObjectsInPerSec += objDelta
			}
		}
		m.Tracks = append(m.Tracks, tm)
	}
	s.lastPer = per

	return m
}

// OutPrefix labels a locally published track; InPrefix labels an
// inbound one. Track labels must start with one of the two.
const (
	OutPrefix = "out/"
	InPrefix  = "in/"
)

func isOutbound(label string) bool {
	return len(label) >= len(OutPrefix) && label[:len(OutPrefix)] == OutPrefix
}

// diff returns cur-prev, guarding the counter-reset case (a new session
// resets the registry) so a rate never goes negative.
func diff(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

func kbps(bytes uint64, seconds float64) float64 {
	return float64(bytes) * 8 / 1000 / seconds
}

func millis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
