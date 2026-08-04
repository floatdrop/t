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
	// pendingInvite holds an invite link that arrived before the frontend
	// was ready to receive it.
	pendingInvite *bridge.Invite
}

// New returns an App. The caller must set its bridge server with
// SetServer before serving, and call Shutdown on exit.
func New(log *slog.Logger, sink *telemetry.LogSink) *App {
	return &App{
		log:      log,
		sink:     sink,
		counters: telemetry.NewRegistry(),
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

	a.counters.Reset()
	joined, err := conf.Join(ctx, a.log, a.server, a.counters, conf.Config{
		Relay:    relay,
		Room:     room,
		Nickname: req.Nickname,
		ID:       id,
		// Development relays run self-signed certificates, and the app
		// has no UI for trusting one. Revisit before any deployment
		// where the relay identity matters.
		Insecure: true,
	})
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

	metCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	a.mu.Lock()
	a.room = joined
	a.stopMet = stop
	a.mu.Unlock()

	go a.sampleMetrics(metCtx, joined)

	a.server.SendControl(&bridge.ServerMessage{Type: bridge.MsgState, State: joined.State()})
	return nil
}

func (a *App) leave() {
	a.mu.Lock()
	room, stop := a.room, a.stopMet
	a.room, a.stopMet = nil, nil
	a.mu.Unlock()

	if stop != nil {
		stop()
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
