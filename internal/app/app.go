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
	"tlmst/internal/update"
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
	// version is the build this process is, published to the room in the
	// catalog and compared against GitHub's newest release.
	version string
	// offer is the release worth telling the frontend about, once the check
	// has found one. Held because the check finishes on its own schedule and
	// the WebView may not be listening yet — and because a WebView that
	// reconnects should not have to wait for the next run to hear about it.
	offer *bridge.Update
	// openURL hands a link to the OS. Supplied by main, which owns the Wails
	// application; nil in tests, where nothing should be opening anything.
	openURL func(string) error
	// lastKeyFrameAsk rate limits requestKeyFrame, whose trigger arrives at
	// the frame rate.
	lastKeyFrameAsk time.Time
	// reattach leaves the room if no frontend comes back. See HandleDisconnect.
	reattach *time.Timer
}

// reattachGrace is how long a room outlives the WebView that was driving it.
//
// Long enough to cover a reload and the bridge's own one-second reconnect,
// short enough that a peer who has actually quit does not linger in everyone's
// roster publishing nothing.
const reattachGrace = 5 * time.Second

// keyFrameAskInterval is the shortest gap between keyframe requests. Longer
// than an encode takes to turn one round, short enough that a group which
// failed to open costs a moment rather than the keyframe interval.
const keyFrameAskInterval = 500 * time.Millisecond

// Reconnect backoff. Short enough that a relay restart is barely noticed,
// capped so a relay that is gone for good is retried without hammering it.
const (
	reconnectInitialDelay = 500 * time.Millisecond
	reconnectMaxDelay     = 10 * time.Second
)

// New returns an App. The caller must set its bridge server with
// SetServer before serving, and call Shutdown on exit.
func New(log *slog.Logger, sink *telemetry.LogSink, version string) *App {
	return &App{
		log:      log,
		sink:     sink,
		version:  version,
		counters: telemetry.NewRegistry(),
		declared: map[string]*bridge.TrackConfig{},
	}
}

// SetOpenURL supplies the means of opening a link outside the WebView.
func (a *App) SetOpenURL(open func(string) error) { a.openURL = open }

// Version is the build this process is running.
func (a *App) Version() string { return a.version }

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

	case bridge.MsgOpenURL:
		return a.openLink(msg.OpenURL)

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
	err := room.WriteFrame(f)
	if errors.Is(err, conf.ErrAwaitingKeyFrame) {
		a.requestKeyFrame()
	}
	return err
}

// requestKeyFrame asks the frontend's encoder for an immediate keyframe.
//
// The publisher refuses to open a group on a delta frame, so any time it has
// no open group — the first frames of a session, or after a write failed and
// reset the subgroup — every frame is turned away until the encoder's own
// schedule comes round, which is a whole keyframe interval of frozen picture
// for every subscriber. The reconnect path has always asked for one; the write
// path, which reaches the same state for a different reason, never did.
//
// Rate limited because the trigger repeats at the frame rate: without it a
// stalled publisher would ask thirty times a second for something that takes
// one encode to deliver.
func (a *App) requestKeyFrame() {
	a.mu.Lock()
	if time.Since(a.lastKeyFrameAsk) < keyFrameAskInterval {
		a.mu.Unlock()
		return
	}
	a.lastKeyFrameAsk = time.Now()
	a.mu.Unlock()

	a.log.Debug("asking for a keyframe to reopen the video group")
	a.server.SendControl(&bridge.ServerMessage{Type: bridge.MsgRequestKeyFrame})
}

// HandleConnect starts streaming backend logs to the freshly connected
// frontend, backfilling whatever the ring buffer already holds so a panel
// opened mid-session is not empty.
func (a *App) HandleConnect() {
	// A frontend is here, so whatever room is held is theirs to resume.
	a.mu.Lock()
	if a.reattach != nil {
		a.reattach.Stop()
		a.reattach = nil
	}
	a.mu.Unlock()

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

	// A link that launched the app has been waiting for this moment, and so
	// has any release the startup check turned up before the WebView was
	// listening.
	a.flushInvite()
	a.sendOffer()
}

// HandleDisconnect withdraws what the departed WebView was publishing, and
// gives it a moment to come back before the room is left.
//
// The encoders and decoders died with the page, so the tracks have to go
// immediately: leaving them declared would hold every peer on a decoder
// waiting for media that cannot arrive. But the session itself is still good,
// and a WebView that has gone away has usually not gone far — a reload, a
// renderer restart, the bridge socket dropping and reconnecting a second
// later. Tearing the room down for that means a full rejoin: a new
// participant identifier, every peer resubscribing, and the user back at the
// welcome screen wondering what they did.
//
// So the room is held briefly instead. If a frontend reattaches it finds the
// session it left and starts publishing into it again; if none does, this is a
// genuine departure and the room is left as before.
func (a *App) HandleDisconnect() {
	a.sink.SetEmit(nil)

	a.mu.Lock()
	room := a.room
	a.mu.Unlock()
	if room == nil {
		return
	}

	// Withdrawn, not merely stopped. Nothing is feeding these now.
	if err := room.UndeclareTrack("video"); err != nil {
		a.log.Debug("withdrawing video after a disconnect", "err", err)
	}
	if err := room.UndeclareTrack("audio"); err != nil {
		a.log.Debug("withdrawing audio after a disconnect", "err", err)
	}

	a.holdRoom()
}

// holdRoom starts the countdown to leaving, and forgets what the departed page
// was publishing. Separate from HandleDisconnect so the decision can be tested
// without a live session to withdraw tracks from.
func (a *App) holdRoom() {
	a.mu.Lock()
	// Forgotten so a relay reconnect inside this window does not restore
	// declarations for encoders that no longer exist.
	a.declared = map[string]*bridge.TrackConfig{}
	if a.reattach != nil {
		a.reattach.Stop()
	}
	a.reattach = time.AfterFunc(reattachGrace, func() {
		a.log.Info("no frontend reattached; leaving the room")
		a.leave()
	})
	a.mu.Unlock()
	a.log.Info("frontend disconnected; holding the room", "grace", reattachGrace)
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
		Version:  a.version,
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

// stopMetrics ends the sampler for a session that is over, so nothing keeps
// reporting a closed connection's gauges.
func (a *App) stopMetrics() {
	a.mu.Lock()
	stop := a.stopMet
	a.stopMet = nil
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
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

// superviseSession keeps the room joined: it moves when the relay asks and
// re-dials when the session dies, until the user leaves.
func (a *App) superviseSession(ctx context.Context, room *conf.Room) {
	for {
		var detail, preferred string

		select {
		case <-ctx.Done():
			return

		case <-room.Migrating():
			// GOAWAY: the session still works, and the point of being told
			// is to move before being disconnected. Acting now spends none
			// of the grace period.
			m := room.Migration()
			detail = "the relay asked us to move"
			preferred = m.Relay
			a.log.Info("migrating on the relay's request",
				"new_relay", preferred, "grace", m.Grace)

		case <-room.Lost():
			detail = "the relay connection dropped"
			if err := room.LostErr(); err != nil {
				detail = err.Error()
			}
			a.log.Warn("relay session lost, reconnecting", "detail", detail)
		}

		// Closed before the replacement is dialled, rather than overlapping
		// them. Two live sessions would announce the same namespace and
		// publish the same tracks twice, and peers would see one participant
		// as two — worse than the momentary gap that closing first costs.
		room.Close()

		// Stopped before redialling, not after. The sampler reads the QUIC
		// trace of the session that just died, and it goes on doing so for the
		// whole backoff — reporting a frozen RTT and congestion window as
		// though they were live, on a connection that is closed. Counters are
		// reset per attempt; these gauges were not, and a stale number
		// presented as current is worse than a missing one.
		a.stopMetrics()

		next := a.redial(ctx, detail, preferred)
		if next == nil {
			return // the user left, or the app is shutting down
		}
		room = next
	}
}

// redial re-joins the room with exponential backoff, returning nil if the
// user leaves or the app shuts down first.
//
// preferred is the relay a GOAWAY named, tried before the configured one. A
// relay that is draining onto a replacement is the case that needs it: going
// back to the original address would mean re-dialling something on its way
// down. It is only preferred, not required — if it does not come up, the
// configured relay is still there to fall back to.
func (a *App) redial(ctx context.Context, detail, preferred string) *conf.Room {
	// Copied, not aliased: a successful migration updates the stored relay,
	// and reading the same struct outside the lock while doing so would race.
	a.mu.Lock()
	if a.joined == nil {
		a.mu.Unlock()
		return nil
	}
	cfg := *a.joined
	a.mu.Unlock()

	delay := reconnectInitialDelay
	for attempt := 1; ; attempt++ {
		a.reportState(bridge.PhaseReconnecting, &cfg, detail)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}

		attemptCfg := cfg
		attemptCfg.Relay = relayForAttempt(cfg.Relay, preferred, attempt)

		a.counters.Reset()
		room, err := conf.Join(ctx, a.log, a.server, a.counters, attemptCfg)
		if err != nil {
			detail = err.Error()
			a.log.Warn("reconnect failed",
				"attempt", attempt, "relay", attemptCfg.Relay, "err", err)
			delay = min(delay*2, reconnectMaxDelay)
			continue
		}

		// A migration that lands somewhere else makes that the address to
		// reconnect to from now on: the old one is going away.
		if attemptCfg.Relay != cfg.Relay {
			a.log.Info("relay changed by migration",
				"from", cfg.Relay, "to", attemptCfg.Relay)
			a.mu.Lock()
			if a.joined != nil {
				a.joined.Relay = attemptCfg.Relay
			}
			a.mu.Unlock()
			cfg.Relay = attemptCfg.Relay
		}

		// Declared before the reconnect is called one, because a session whose
		// catalog does not describe our tracks is not a recovered call: the
		// publications exist, so frames go out and the debug panel reads
		// healthy, but no peer ever subscribes to a track the catalog never
		// mentioned. Nobody can see or hear us, and everything on our own
		// screen says otherwise.
		if err := a.restoreDeclarations(room); err != nil {
			a.log.Warn("could not restore declarations; retrying the reconnect",
				"attempt", attempt, "err", err)
			room.Close()
			detail = err.Error()
			delay = min(delay*2, reconnectMaxDelay)
			continue
		}

		a.installRoom(ctx, room)
		a.reportState(bridge.PhaseJoined, &cfg, "")
		// A new session has no open group, and the publisher will not start
		// one on a delta frame, so ask for a keyframe rather than waiting
		// out the encoder's own interval.
		a.server.SendControl(&bridge.ServerMessage{Type: bridge.MsgRequestKeyFrame})
		a.log.Info("reconnected to the relay", "attempt", attempt, "participant", room.State().ID)
		return room
	}
}

// relayForAttempt picks the address one reconnect attempt should dial.
//
// A GOAWAY may name a replacement relay, and §10.4 allows it to be absent —
// which means "come back to me", not "go nowhere". An empty preferred address
// therefore has to resolve to the relay already configured, or a graceful
// drain by a relay that names no successor would leave us with nothing to
// dial.
//
// The named relay only gets the first attempt. If it does not come up, later
// attempts fall back to the configured address, so a bad or stale URI cannot
// strand the client.
func relayForAttempt(configured, preferred string, attempt int) string {
	if preferred != "" && attempt == 1 {
		return preferred
	}
	return configured
}

// restoreDeclarations replays the encoder configurations the frontend has
// already sent, so the new session's catalog describes the same tracks.
//
// Returns the first failure rather than logging past it. This used to warn and
// carry on, and the reconnect was then reported as joined regardless — which
// is the worst of the available outcomes, because everything downstream of it
// works. The publications are open so writeFrame succeeds, bytes leave, the
// panel shows healthy publish throughput, and the catalog says this
// participant has no tracks. Every peer correctly subscribes to nothing.
func (a *App) restoreDeclarations(room *conf.Room) error {
	a.mu.Lock()
	declared := make([]*bridge.TrackConfig, 0, len(a.declared))
	for _, cfg := range a.declared {
		declared = append(declared, cfg)
	}
	a.mu.Unlock()

	for _, cfg := range declared {
		if err := room.DeclareTrack(cfg); err != nil {
			return fmt.Errorf("restore %s declaration: %w", cfg.Kind, err)
		}
	}
	return nil
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
	if a.reattach != nil {
		a.reattach.Stop()
		a.reattach = nil
	}
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
			// Read here rather than counted by the sampler: the drops belong to
			// the bridge connection, which outlives any one session and knows
			// nothing about rooms.
			m.BridgeDropped = a.server.DroppedFrames()
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

// CheckForUpdate asks GitHub whether there is a newer release and, if so,
// remembers it and tells the frontend.
//
// Called once at startup from its own goroutine. Nothing waits on it and
// nothing retries it: the result is a button that may or may not appear, so a
// relay that is slow, an address that is rate-limited or a machine with no
// network at all should all produce the same thing — no button, and a line in
// the log for whoever goes looking.
func (a *App) CheckForUpdate(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, update.Timeout)
	defer cancel()

	release, err := update.Check(ctx, nil, a.version)
	if err != nil {
		// Debug, not warn. Being unable to reach GitHub is not a fault in
		// this app, and a conference that logs a warning every launch for
		// something nobody asked for teaches people to ignore warnings.
		a.log.Debug("update check failed", "err", err)
		return
	}
	if release.Version == "" {
		a.log.Info("running the newest release", "version", a.version)
		return
	}

	a.log.Info("a newer release is available",
		"running", a.version, "latest", release.Version, "url", release.URL)

	offer := &bridge.Update{Version: release.Version, URL: release.URL}
	a.mu.Lock()
	a.offer = offer
	a.mu.Unlock()
	a.sendOffer()
}

// sendOffer tells the frontend about a release, if one has been found and a
// frontend is there to hear it.
func (a *App) sendOffer() {
	a.mu.Lock()
	offer := a.offer
	a.mu.Unlock()
	if offer == nil || !a.server.Connected() {
		return
	}
	a.server.SendControl(&bridge.ServerMessage{Type: bridge.MsgUpdate, Update: offer})
}

// openLink hands a URL to the OS browser.
//
// Only links this app produced: the frontend can ask for the releases page and
// nothing else. The message crosses a loopback socket that any local process
// could in principle reach, and "open an arbitrary URL" is a capability worth
// exactly nobody having.
func (a *App) openLink(url string) error {
	if url == "" {
		return errors.New("app: openUrl message names no link")
	}
	if url != update.ReleasesURL && (a.offer == nil || url != a.offer.URL) {
		return fmt.Errorf("app: refusing to open an unrecognised link %q", url)
	}
	if a.openURL == nil {
		return errors.New("app: no way to open a link on this platform")
	}
	a.log.Info("opening a link outside the app", "url", url)
	return a.openURL(url)
}

// Shutdown leaves any joined room. Safe to call more than once.
func (a *App) Shutdown() {
	a.sink.SetEmit(nil)
	a.leave()
}
