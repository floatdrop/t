// Package app wires the bridge, the conference session, and the debug
// telemetry into one object with a lifecycle: it is the bridge.Handler
// the WebSocket server calls, and it owns whichever conf.Room is joined.
package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"tlmst/internal/bridge"
	"tlmst/internal/conf"
	"tlmst/internal/telemetry"
)

// metricsInterval is how often a joined session samples transport and
// track counters for the debug plots. 250 ms gives the plots four points
// a second — responsive enough to see a congestion event, cheap enough to
// leave running.
const metricsInterval = 250 * time.Millisecond

// App is the backend half of the application.
type App struct {
	log      *slog.Logger
	sink     *telemetry.LogSink
	counters *telemetry.Registry
	server   *bridge.Server

	// mu guards room and the sampler goroutine's lifecycle. Join and
	// Leave arrive from the bridge read loop, but Shutdown can arrive
	// from the Wails main thread.
	mu      sync.Mutex
	room    *conf.Room
	stopMet context.CancelFunc
	// stopSession ends the supervisor watching the current session, so a
	// deliberate leave does not race a reconnect.
	stopSession context.CancelFunc
	// joined is what a reconnect re-dials with.
	joined *conf.Config
	// declared caches the encoder configurations the frontend has sent, by
	// kind. A reconnect rebuilds the catalog from these, so the frontend
	// does not have to notice the session was replaced.
	declared map[string]*bridge.TrackConfig
	// pendingInvite holds an invite link that arrived before the frontend
	// was ready to receive it.
	pendingInvite *bridge.Invite
}

// Reconnect backoff. Short enough that a relay restart is barely noticed,
// capped so a relay that is gone for good is retried without hammering it.
const (
	reconnectInitialDelay = 500 * time.Millisecond
	reconnectMaxDelay     = 10 * time.Second
)

// New returns an App. The caller must set its bridge server with
// SetServer before serving, and call Shutdown on exit.
func New(log *slog.Logger, sink *telemetry.LogSink) *App {
	return &App{
		log:      log,
		sink:     sink,
		counters: telemetry.NewRegistry(),
		declared: map[string]*bridge.TrackConfig{},
	}
}

// SetServer attaches the bridge the App reports through. Called once,
// before Serve, because Server and Handler are mutually referential.
func (a *App) SetServer(s *bridge.Server) { a.server = s }

// ---- bridge.Handler ---------------------------------------------------

// HandleControl dispatches one JSON control message from the frontend.
func (a *App) HandleControl(ctx context.Context, msg *bridge.ClientMessage) error {
	switch msg.Type {
	case bridge.MsgJoin:
		if msg.Join == nil {
			return errors.New("app: join message has no payload")
		}
		return a.join(ctx, msg.Join)

	case bridge.MsgLeave:
		a.leave()
		return nil

	case bridge.MsgTrack:
		if msg.Track == nil {
			return errors.New("app: track message has no payload")
		}
		return a.declareTrack(msg.Track)

	case bridge.MsgStats:
		// Frontend counters are for the debug panel only; the backend
		// does not act on them, and the panel already has them locally.
		return nil

	case bridge.MsgLogLevel:
		return a.setLogLevel(msg.LogLevel)

	case bridge.MsgUntrack:
		if msg.Untrack == "" {
			return errors.New("app: untrack message names no kind")
		}
		return a.untrackKind(msg.Untrack)

	case bridge.MsgReport:
		if msg.Report == nil {
			return errors.New("app: report message has no payload")
		}
		a.logReport(msg.Report)
		return nil

	default:
		return fmt.Errorf("app: unknown control message %q", msg.Type)
	}
}

// HandleMedia publishes one encoded frame captured by the frontend.
func (a *App) HandleMedia(_ context.Context, f *bridge.MediaFrame) error {
	a.mu.Lock()
	room := a.room
	a.mu.Unlock()
	if room == nil {
		// Frames can trail a Leave by a few milliseconds while the
		// frontend's encoders wind down. Not an error worth surfacing.
		return nil
	}
	return room.WriteFrame(f)
}

// HandleConnect starts streaming backend logs to the freshly connected
// frontend, backfilling whatever the ring buffer already holds so a panel
// opened mid-session is not empty.
func (a *App) HandleConnect() {
	for _, entry := range a.sink.Recent() {
		e := entry
		a.server.SendControl(&bridge.ServerMessage{Type: bridge.MsgLog, Log: &e})
	}
	a.sink.SetEmit(func(entry bridge.LogEntry) {
		e := entry
		a.server.SendControl(&bridge.ServerMessage{Type: bridge.MsgLog, Log: &e})
	})
	// A reconnecting WebView has no session state; tell it so explicitly
	// rather than leaving its banner blank.
	a.mu.Lock()
	room := a.room
	a.mu.Unlock()
	state := &bridge.SessionState{Phase: bridge.PhaseIdle}
	if room != nil {
		state = room.State()
	}
	a.server.SendControl(&bridge.ServerMessage{Type: bridge.MsgState, State: state})

	// A link that launched the app has been waiting for this moment.
	a.flushInvite()
}

// HandleDisconnect tears the session down when the WebView goes away: its
// encoders and decoders died with it, so the publications have nothing
// left to carry.
func (a *App) HandleDisconnect() {
	a.sink.SetEmit(nil)
	a.leave()
}

// ---- session lifecycle ------------------------------------------------

func (a *App) join(ctx context.Context, req *bridge.JoinRequest) error {
	a.leave()

	id, err := conf.NewID()
	if err != nil {
		return err
	}

	relay := strings.TrimSpace(req.Relay)
	room := strings.TrimSpace(req.Room)
	a.server.SendControl(&bridge.ServerMessage{
		Type: bridge.MsgState,
		State: &bridge.SessionState{
			Phase:    bridge.PhaseConnecting,
			Relay:    relay,
			Room:     room,
			ID:       id,
			Nickname: req.Nickname,
		},
	})

	cfg := conf.Config{
		Relay:    relay,
		Room:     room,
		Nickname: req.Nickname,
		ID:       id,
		// Development relays run self-signed certificates, and the app
		// has no UI for trusting one. Revisit before any deployment
		// where the relay identity matters.
		Insecure: true,
	}

	a.counters.Reset()
	joined, err := conf.Join(ctx, a.log, a.server, a.counters, cfg)
	if err != nil {
		a.server.SendControl(&bridge.ServerMessage{
			Type: bridge.MsgState,
			State: &bridge.SessionState{
				Phase:  bridge.PhaseFailed,
				Relay:  relay,
				Room:   room,
				Detail: err.Error(),
			},
		})
		return err
	}

	// The session's lifetime is not the caller's: HandleControl returns as
	// soon as the join succeeds, and the supervisor has to outlive it.
	sessCtx, stopSession := context.WithCancel(context.WithoutCancel(ctx))
	a.mu.Lock()
	a.joined = &cfg
	a.declared = map[string]*bridge.TrackConfig{}
	a.stopSession = stopSession
	a.mu.Unlock()

	a.installRoom(sessCtx, joined)
	go a.superviseSession(sessCtx, joined)

	a.server.SendControl(&bridge.ServerMessage{Type: bridge.MsgState, State: joined.State()})
	return nil
}

// installRoom makes room the current one and restarts the metrics sampler
// against its QUIC trace, which is per-session and does not survive a
// reconnect.
func (a *App) installRoom(ctx context.Context, room *conf.Room) {
	metCtx, stopMet := context.WithCancel(ctx)

	a.mu.Lock()
	previous := a.stopMet
	a.room = room
	a.stopMet = stopMet
	a.mu.Unlock()

	if previous != nil {
		previous()
	}
	go a.sampleMetrics(metCtx, room)
}

// superviseSession waits for the relay session to end and, unless the user
// left, re-dials until it is back.
func (a *App) superviseSession(ctx context.Context, room *conf.Room) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-room.Lost():
		}

		detail := "the relay connection dropped"
		if err := room.LostErr(); err != nil {
			detail = err.Error()
		}
		a.log.Warn("relay session lost, reconnecting", "detail", detail)
		// Release the dead session's streams and subscriptions before
		// standing a new one up, so the two never overlap.
		room.Close()

		next := a.redial(ctx, detail)
		if next == nil {
			return // the user left, or the app is shutting down
		}
		room = next
	}
}

// redial re-joins the room with exponential backoff, returning nil if the
// user leaves or the app shuts down first.
func (a *App) redial(ctx context.Context, detail string) *conf.Room {
	a.mu.Lock()
	cfg := a.joined
	a.mu.Unlock()
	if cfg == nil {
		return nil
	}

	delay := reconnectInitialDelay
	for attempt := 1; ; attempt++ {
		a.reportState(bridge.PhaseReconnecting, cfg, detail)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}

		a.counters.Reset()
		room, err := conf.Join(ctx, a.log, a.server, a.counters, *cfg)
		if err != nil {
			detail = err.Error()
			a.log.Warn("reconnect failed", "attempt", attempt, "err", err)
			delay = min(delay*2, reconnectMaxDelay)
			continue
		}

		a.installRoom(ctx, room)
		a.restoreDeclarations(room)
		a.reportState(bridge.PhaseJoined, cfg, "")
		// A new session has no open group, and the publisher will not start
		// one on a delta frame, so ask for a keyframe rather than waiting
		// out the encoder's own interval.
		a.server.SendControl(&bridge.ServerMessage{Type: bridge.MsgRequestKeyFrame})
		a.log.Info("reconnected to the relay", "attempt", attempt, "participant", room.State().ID)
		return room
	}
}

// restoreDeclarations replays the encoder configurations the frontend has
// already sent, so the new session's catalog describes the same tracks.
func (a *App) restoreDeclarations(room *conf.Room) {
	a.mu.Lock()
	declared := make([]*bridge.TrackConfig, 0, len(a.declared))
	for _, cfg := range a.declared {
		declared = append(declared, cfg)
	}
	a.mu.Unlock()

	for _, cfg := range declared {
		if err := room.DeclareTrack(cfg); err != nil {
			a.log.Warn("could not restore a track declaration",
				"kind", cfg.Kind, "err", err)
		}
	}
}

// reportState pushes one session-state update to the frontend.
func (a *App) reportState(phase string, cfg *conf.Config, detail string) {
	state := &bridge.SessionState{Phase: phase, Detail: detail}
	if cfg != nil {
		state.Relay, state.Room, state.Nickname = cfg.Relay, cfg.Room, cfg.Nickname
	}
	a.mu.Lock()
	if a.room != nil {
		state.ID = a.room.State().ID
	}
	a.mu.Unlock()
	a.server.SendControl(&bridge.ServerMessage{Type: bridge.MsgState, State: state})
}

func (a *App) leave() {
	a.mu.Lock()
	room, stopMet, stopSession := a.room, a.stopMet, a.stopSession
	a.room, a.stopMet, a.stopSession = nil, nil, nil
	a.joined = nil
	a.declared = map[string]*bridge.TrackConfig{}
	a.mu.Unlock()

	// Stop supervising before closing, or the supervisor would see the
	// session end and start reconnecting to a room the user just left.
	if stopSession != nil {
		stopSession()
	}
	if stopMet != nil {
		stopMet()
	}
	if room == nil {
		return
	}
	room.Close()
	a.server.SendControl(&bridge.ServerMessage{
		Type:  bridge.MsgState,
		State: &bridge.SessionState{Phase: bridge.PhaseIdle},
	})
}

func (a *App) declareTrack(cfg *bridge.TrackConfig) error {
	// Reject a description we cannot decode here rather than letting the
	// publisher put an unusable config in its catalog.
	if cfg.Description != "" {
		if _, err := base64.StdEncoding.DecodeString(cfg.Description); err != nil {
			return fmt.Errorf("app: track %s: bad base64 description: %w", cfg.Kind, err)
		}
	}

	a.mu.Lock()
	room := a.room
	if room != nil {
		// Remembered so a reconnect can rebuild the catalog without the
		// frontend having to re-send anything.
		a.declared[cfg.Kind] = cfg
	}
	a.mu.Unlock()
	if room == nil {
		return errors.New("app: cannot declare a track before joining")
	}
	return room.DeclareTrack(cfg)
}

// logReport folds a frontend event into the backend log, so the debug
// panel shows capture and decode problems next to the transport's.
func (a *App) logReport(r *bridge.ClientReport) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(r.Level)); err != nil {
		level = slog.LevelInfo
	}
	attrs := make([]any, 0, len(r.Attrs)*2+2)
	attrs = append(attrs, "source", "webview")
	for k, v := range r.Attrs {
		attrs = append(attrs, k, v)
	}
	a.log.Log(context.Background(), level, r.Message, attrs...)
}

// untrackKind withdraws a local track and forgets its declaration, so a
// reconnect does not resurrect a track the user switched off.
func (a *App) untrackKind(kind string) error {
	a.mu.Lock()
	room := a.room
	delete(a.declared, kind)
	a.mu.Unlock()
	if room == nil {
		return nil // nothing is published, so nothing to withdraw
	}
	return room.UndeclareTrack(kind)
}

func (a *App) setLogLevel(name string) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(name)); err != nil {
		return fmt.Errorf("app: unknown log level %q: %w", name, err)
	}
	a.sink.SetLevel(level)
	a.log.Info("backend log level changed", "level", level.String())
	return nil
}

// sampleMetrics streams a metrics sample to the frontend on a fixed tick
// for as long as the session lives.
func (a *App) sampleMetrics(ctx context.Context, room *conf.Room) {
	sampler := telemetry.NewSampler(a.counters, room.Trace())
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()

	// Prime the sampler so the first reported sample carries real rates
	// rather than the zeroes of a missing baseline.
	sampler.Sample(time.Now())

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m := sampler.Sample(now)
			a.server.SendControl(&bridge.ServerMessage{Type: bridge.MsgMetrics, Metrics: &m})
		}
	}
}

// OpenInvite hands the frontend a relay and room to join, from an invite
// link the OS delivered. Queued when no frontend is attached yet, which is
// the launch-by-link case: the WebView takes a moment to connect, and
// dropping the invite would make the link look broken.
func (a *App) OpenInvite(relay, room string) {
	invite := &bridge.Invite{Relay: relay, Room: room}
	a.log.Info("invite link received", "relay", relay, "room", room)

	a.mu.Lock()
	a.pendingInvite = invite
	a.mu.Unlock()

	if a.server.Connected() {
		a.flushInvite()
	}
}

// flushInvite delivers a queued invite, if any.
func (a *App) flushInvite() {
	a.mu.Lock()
	invite := a.pendingInvite
	a.pendingInvite = nil
	a.mu.Unlock()
	if invite != nil {
		a.server.SendControl(&bridge.ServerMessage{Type: bridge.MsgInvite, Invite: invite})
	}
}

// Shutdown leaves any joined room. Safe to call more than once.
func (a *App) Shutdown() {
	a.sink.SetEmit(nil)
	a.leave()
}
