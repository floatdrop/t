package telemetry

import (
	"sync"
	"time"

	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

// QUICTrace implements qlogwriter.Trace so the app can consume a QUIC
// connection's qlog event stream in-process instead of writing it to a
// file. quic-go's Config.Tracer hands us a Trace per connection; every
// event the connection produces — RTT and congestion updates, packet
// sends, losses — arrives at RecordEvent, where we fold it into counters
// the debug panel samples.
//
// As of quic-go v0.61 there is no direct accessor for RTT or loss on a
// Connection, so this qlog stream is the only way to get them.
type QUICTrace struct {
	mu sync.Mutex

	// Latest gauge values, replaced on each recovery:metrics_updated.
	minRTT       time.Duration
	smoothedRTT  time.Duration
	latestRTT    time.Duration
	rttVariance  time.Duration
	cwnd         int
	bytesInFly   int
	packetsInFly int
	congestion   string

	// Monotonic counters since the connection opened.
	packetsSent     uint64
	packetsReceived uint64
	packetsLost     uint64
	bytesSent       uint64
	bytesReceived   uint64
}

// QUICSnapshot is a point-in-time read of a QUICTrace.
type QUICSnapshot struct {
	MinRTT           time.Duration
	SmoothedRTT      time.Duration
	LatestRTT        time.Duration
	RTTVariance      time.Duration
	CongestionWindow int
	BytesInFlight    int
	PacketsInFlight  int
	CongestionState  string

	PacketsSent     uint64
	PacketsReceived uint64
	PacketsLost     uint64
	BytesSent       uint64
	BytesReceived   uint64
}

// NewQUICTrace returns a trace ready to hand to quic.Config.Tracer.
func NewQUICTrace() *QUICTrace { return &QUICTrace{} }

// AddProducer implements qlogwriter.Trace. Every producer folds into the
// same counters, so the returned recorder is the trace itself.
func (t *QUICTrace) AddProducer() qlogwriter.Recorder { return t }

// SupportsSchemas implements qlogwriter.Trace. We accept every schema:
// the type switch in RecordEvent is what actually selects events.
func (t *QUICTrace) SupportsSchemas(string) bool { return true }

// Close implements io.Closer for qlogwriter.Recorder. The trace outlives
// its producers — the snapshot stays readable after the connection ends —
// so there is nothing to release.
func (t *QUICTrace) Close() error { return nil }

// RecordEvent implements qlogwriter.Recorder. quic-go calls this from its
// connection goroutines, potentially concurrently.
func (t *QUICTrace) RecordEvent(ev qlogwriter.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch e := ev.(type) {
	case qlog.MetricsUpdated:
		// quic-go omits unchanged fields, so a zero means "no update"
		// rather than "now zero" — keep the previous value.
		if e.MinRTT != 0 {
			t.minRTT = e.MinRTT
		}
		if e.SmoothedRTT != 0 {
			t.smoothedRTT = e.SmoothedRTT
		}
		if e.LatestRTT != 0 {
			t.latestRTT = e.LatestRTT
		}
		if e.RTTVariance != 0 {
			t.rttVariance = e.RTTVariance
		}
		if e.CongestionWindow != 0 {
			t.cwnd = e.CongestionWindow
		}
		t.bytesInFly = e.BytesInFlight
		t.packetsInFly = e.PacketsInFlight

	case qlog.PacketSent:
		t.packetsSent++
		t.bytesSent += uint64(e.Raw.Length)

	case qlog.PacketReceived:
		t.packetsReceived++
		t.bytesReceived += uint64(e.Raw.Length)

	case qlog.PacketLost:
		t.packetsLost++

	case qlog.CongestionStateUpdated:
		t.congestion = e.State.String()
	}
}

// Snapshot reads the current values.
func (t *QUICTrace) Snapshot() QUICSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return QUICSnapshot{
		MinRTT:           t.minRTT,
		SmoothedRTT:      t.smoothedRTT,
		LatestRTT:        t.latestRTT,
		RTTVariance:      t.rttVariance,
		CongestionWindow: t.cwnd,
		BytesInFlight:    t.bytesInFly,
		PacketsInFlight:  t.packetsInFly,
		CongestionState:  t.congestion,
		PacketsSent:      t.packetsSent,
		PacketsReceived:  t.packetsReceived,
		PacketsLost:      t.packetsLost,
		BytesSent:        t.bytesSent,
		BytesReceived:    t.bytesReceived,
	}
}
