// Package telemetry collects what the debug panels display: backend log
// records, QUIC transport metrics from the connection's qlog event
// stream, and per-track byte/object counters.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"tlmst/internal/bridge"
)

// logRingSize is how many recent records the sink keeps so a frontend
// that connects (or reloads) mid-session can backfill its log panel.
const logRingSize = 2000

// logState is the part of a LogSink that every derived handler shares:
// the ring buffer, the level, and the live callback. WithAttrs and
// WithGroup produce new LogSink values that point at the same logState,
// which is what lets records logged through a derived logger land in the
// one buffer the debug panel reads.
type logState struct {
	mu      sync.RWMutex
	ring    []bridge.LogEntry
	next    int
	wrapped bool
	emit    func(bridge.LogEntry)

	level *slog.LevelVar
}

// LogSink is a slog.Handler that fans every record out to two places: a
// bounded ring buffer for replay, and a live callback for streaming to
// the frontend. It wraps no other handler — the caller composes it with a
// terminal handler via MultiHandler if it also wants stderr output.
//
// The level is settable at runtime so the debug panel can turn on
// moq-go's debug logging without a restart.
type LogSink struct {
	st *logState

	// attrs and groups accumulate through WithAttrs / WithGroup so
	// records logged through a derived logger keep their context.
	attrs  []slog.Attr
	groups []string
}

// NewLogSink returns a sink at the given initial level.
func NewLogSink(level slog.Level) *LogSink {
	lv := new(slog.LevelVar)
	lv.Set(level)
	return &LogSink{st: &logState{
		ring:  make([]bridge.LogEntry, logRingSize),
		level: lv,
	}}
}

// SetLevel changes the minimum level, taking effect immediately for every
// logger derived from this sink.
func (s *LogSink) SetLevel(level slog.Level) { s.st.level.Set(level) }

// Level reports the current minimum level.
func (s *LogSink) Level() slog.Level { return s.st.level.Level() }

// SetEmit installs the live callback, or clears it when fn is nil. The
// callback must not block: it runs on the goroutine that logged.
func (s *LogSink) SetEmit(fn func(bridge.LogEntry)) {
	s.st.mu.Lock()
	s.st.emit = fn
	s.st.mu.Unlock()
}

// Recent returns the buffered records in chronological order.
func (s *LogSink) Recent() []bridge.LogEntry {
	s.st.mu.RLock()
	defer s.st.mu.RUnlock()
	if !s.st.wrapped {
		return append([]bridge.LogEntry(nil), s.st.ring[:s.st.next]...)
	}
	out := make([]bridge.LogEntry, 0, len(s.st.ring))
	out = append(out, s.st.ring[s.st.next:]...)
	out = append(out, s.st.ring[:s.st.next]...)
	return out
}

func (s *LogSink) Enabled(_ context.Context, l slog.Level) bool {
	return l >= s.st.level.Level()
}

func (s *LogSink) Handle(_ context.Context, r slog.Record) error {
	entry := bridge.LogEntry{
		TimeMillis: r.Time.UnixMilli(),
		Level:      r.Level.String(),
		Message:    r.Message,
	}
	if r.Time.IsZero() {
		entry.TimeMillis = time.Now().UnixMilli()
	}

	if n := len(s.attrs) + r.NumAttrs(); n > 0 {
		entry.Attrs = make(map[string]string, n)
		for _, a := range s.attrs {
			putAttr(entry.Attrs, s.groups, a)
		}
		r.Attrs(func(a slog.Attr) bool {
			putAttr(entry.Attrs, s.groups, a)
			return true
		})
	}

	s.st.mu.Lock()
	s.st.ring[s.st.next] = entry
	s.st.next++
	if s.st.next == len(s.st.ring) {
		s.st.next = 0
		s.st.wrapped = true
	}
	emit := s.st.emit
	s.st.mu.Unlock()

	if emit != nil {
		emit(entry)
	}
	return nil
}

func (s *LogSink) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return s
	}
	return &LogSink{
		st:     s.st,
		attrs:  append(append([]slog.Attr(nil), s.attrs...), attrs...),
		groups: s.groups,
	}
}

func (s *LogSink) WithGroup(name string) slog.Handler {
	if name == "" {
		return s
	}
	return &LogSink{
		st:     s.st,
		attrs:  s.attrs,
		groups: append(append([]string(nil), s.groups...), name),
	}
}

// putAttr flattens one attribute into dst, prefixing its key with any
// enclosing group names so nested values stay distinguishable.
func putAttr(dst map[string]string, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		for _, sub := range a.Value.Group() {
			putAttr(dst, append(groups, a.Key), sub)
		}
		return
	}
	key := a.Key
	for i := len(groups) - 1; i >= 0; i-- {
		key = groups[i] + "." + key
	}
	dst[key] = fmt.Sprint(a.Value.Any())
}

// MultiHandler sends every record to each of the given handlers. Used to
// tee the backend's logs to both stderr and the debug panel.
type MultiHandler []slog.Handler

func (m MultiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		// Each handler gets its own clone: a handler is free to mutate
		// the record it is given, and Record.Attrs is single-pass.
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(MultiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (m MultiHandler) WithGroup(name string) slog.Handler {
	out := make(MultiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(name)
	}
	return out
}
