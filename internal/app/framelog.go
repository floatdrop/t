package app

import (
	"bufio"
	"fmt"
	"io"
	"sync"
	"time"

	"t/internal/bridge"
)

// frameLog records every locally encoded video frame as one CSV row, for
// measuring what keyframes cost: their size against the deltas around them,
// and how the encoder's rate control pays for them. It is an instrument for
// deciding the keyframe interval, switched on by the -framelog launch flag, and
// costs nothing when it is off.
//
// Rows are written where the frame enters the backend, ahead of the publish
// pump, so they describe what the encoder produced rather than what the uplink
// managed to carry.
//
// Columns:
//
//	ms      wall time since the log was opened
//	event   frame | declare | kfreq
//	ts_us   encoder timestamp (frame)
//	key     1 for a keyframe (frame)
//	layer   temporal layer, 0 is the base (frame)
//	bytes   payload size (frame)
//	detail  width x height @ bitrate for a declare; empty otherwise
type frameLog struct {
	mu    sync.Mutex
	w     *bufio.Writer
	start time.Time
}

func newFrameLog(w io.Writer) *frameLog {
	l := &frameLog{w: bufio.NewWriter(w), start: time.Now()}
	fmt.Fprintln(l.w, "ms,event,ts_us,key,layer,bytes,detail")
	return l
}

func (l *frameLog) since() float64 {
	return float64(time.Since(l.start).Microseconds()) / 1000
}

func (l *frameLog) frame(f *bridge.MediaFrame) {
	if l == nil || f.Kind != bridge.KindVideo {
		return
	}
	key := 0
	if f.KeyFrame {
		key = 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "%.1f,frame,%d,%d,%d,%d,\n",
		l.since(), f.Timestamp, key, f.TemporalLayer, len(f.Payload))
}

func (l *frameLog) declare(cfg *bridge.TrackConfig) {
	if l == nil || cfg.Kind != "video" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "%.1f,declare,,,,,%dx%d@%d %s\n",
		l.since(), cfg.Width, cfg.Height, cfg.Bitrate, cfg.Codec)
	l.w.Flush()
}

func (l *frameLog) keyFrameRequest() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "%.1f,kfreq,,,,,\n", l.since())
}

// flush writes out whatever is buffered. Called on a timer and at shutdown, so
// a run that is killed loses at most a second of rows.
func (l *frameLog) flush() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w.Flush()
}
