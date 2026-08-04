package conf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/floatdrop/moq-go/pkg/moqt"
	"github.com/floatdrop/moq-go/pkg/moqt/message"
	"github.com/floatdrop/moq-go/pkg/moqt/session"
	"github.com/floatdrop/moq-go/pkg/moqt/wire"

	"tlmst/internal/bridge"
	"tlmst/internal/telemetry"
)

// Sink is where a Room delivers inbound media and control updates —
// satisfied by *bridge.Server.
type Sink interface {
	SendMedia(*bridge.MediaFrame)
	SendControl(*bridge.ServerMessage)
}

// Room is one joined conference: the MOQ session, the local participant's
// publications, and a subscription per remote participant.
type Room struct {
	cfg      Config
	log      *slog.Logger
	sink     Sink
	counters *telemetry.Registry

	sess   *session.Session
	trace  *telemetry.QUICTrace
	router *router

	pub *publisher

	ctx    context.Context
	cancel context.CancelFunc
	// done closes once the session's read loops have exited, so Close can
	// wait for a quiet shutdown.
	done chan struct{}

	// lost closes when the MOQ session ends without Close being called —
	// the relay went away, the transport broke, the peer sent GOAWAY. It is
	// driven off session.Done rather than inferred from whichever read loop
	// errors first, so there is one authoritative signal to act on.
	lost    chan struct{}
	lostErr error

	// migrate closes when the relay sends GOAWAY (§10.4) — it is shutting
	// down or rebalancing and wants us on a different session. Separate
	// from lost because the session is still usable: we are being given a
	// grace period rather than cut off, and acting on it early is the whole
	// point of the message.
	migrate     chan struct{}
	migration   Migration
	migrateOnce sync.Once
	// leaving marks a deliberate Close, so the watcher can tell "the user
	// left" from "the session died".
	leaving atomic.Bool

	handles atomic.Uint32

	mu      sync.Mutex
	remotes map[string]*remote
	closed  bool
}

// Join dials the relay, announces the local participant, and starts
// watching the room for others. It returns once the session is
// established; discovery and media flow continue in the background until
// Close.
func Join(ctx context.Context, log *slog.Logger, sink Sink, counters *telemetry.Registry, cfg Config) (*Room, error) {
	if cfg.Room == "" {
		return nil, errors.New("conf: room identifier is required")
	}
	if cfg.Relay == "" {
		return nil, errors.New("conf: relay address is required")
	}
	if cfg.ID == "" {
		id, err := NewID()
		if err != nil {
			return nil, err
		}
		cfg.ID = id
	}

	res, err := dial(ctx, log, cfg.Relay, cfg.Insecure)
	if err != nil {
		return nil, err
	}

	// The room's lifetime is its own, not the caller's join context: a
	// join that returns must leave the session running.
	roomCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	r := &Room{
		cfg:      cfg,
		log:      log.With("room", cfg.Room, "self", cfg.ID),
		sink:     sink,
		counters: counters,
		sess:     res.sess,
		trace:    res.trace,
		ctx:      roomCtx,
		cancel:   cancel,
		done:     make(chan struct{}),
		lost:     make(chan struct{}),
		migrate:  make(chan struct{}),
		remotes:  make(map[string]*remote),
	}
	r.handles.Store(bridge.HandleRemoteBase)
	r.router = newRouter(r.log)

	r.pub, err = newPublisher(roomCtx, r.log, res.sess, counters, cfg)
	if err != nil {
		cancel()
		_ = res.sess.Close(moqt.SessionInternalError, "publish failed")
		return nil, err
	}

	watch, err := r.watchRoom(roomCtx)
	if err != nil {
		cancel()
		_ = res.sess.Close(moqt.SessionInternalError, "namespace watch failed")
		return nil, err
	}

	// Registered after the session is up. OnGoaway replays a GOAWAY that
	// arrived before this point, so nothing is lost by being late.
	res.sess.OnGoaway(r.onGoaway)

	go r.runRouter()
	go r.readAnnouncements(watch)
	go r.watchSession()

	r.log.Info("joined room", "relay", cfg.Relay, "nickname", cfg.Nickname)
	return r, nil
}

// Trace exposes the session's QUIC trace so the metrics sampler can read
// transport health.
func (r *Room) Trace() *telemetry.QUICTrace { return r.trace }

// Lost is closed when the session ended on its own — the relay became
// unreachable, the transport failed, the peer went away. It never closes for
// a Close initiated here, so a supervisor can use it as "reconnect now"
// without having to second-guess a deliberate leave.
func (r *Room) Lost() <-chan struct{} { return r.lost }

// LostErr reports why the session ended, once Lost has closed. Nil means it
// ended cleanly, which for a relay-side shutdown is the normal case.
func (r *Room) LostErr() error {
	<-r.lost
	return r.lostErr
}

// Migration is a relay's request that we continue on a different session,
// carried by GOAWAY (§10.4).
type Migration struct {
	// Relay is where the relay wants us to go — the GOAWAY New Session URI.
	// Empty when it did not name one, which means "reconnect to me".
	Relay string
	// Grace is how long the relay says it will wait before closing this
	// session itself. Zero when it did not say.
	Grace time.Duration
}

// Migrating is closed when the relay has asked us to move to a new session.
// Unlike Lost, the current session still works — the request is an invitation
// to move before being disconnected, so a supervisor should act on it
// promptly rather than wait out the grace period.
func (r *Room) Migrating() <-chan struct{} { return r.migrate }

// Migration reports what the relay asked for, once Migrating has closed.
func (r *Room) Migration() Migration {
	<-r.migrate
	return r.migration
}

// onGoaway records the relay's migration request. Only the first is acted on:
// a second GOAWAY says nothing new, and the supervisor is already moving.
func (r *Room) onGoaway(g *message.Goaway) {
	r.migrateOnce.Do(func() {
		r.migration = Migration{
			Relay: string(g.NewSessionURI),
			// §10.4 carries the grace period in milliseconds.
			Grace: time.Duration(g.Timeout) * time.Millisecond,
		}
		r.log.Info("relay asked us to migrate",
			"new_relay", r.migration.Relay, "grace", r.migration.Grace)
		close(r.migrate)
	})
}

// watchSession turns the session ending into the Lost signal.
func (r *Room) watchSession() {
	<-r.sess.Done()
	if r.leaving.Load() {
		return // Close did this on purpose
	}
	r.lostErr = r.sess.Err()
	r.log.Warn("relay session ended", "err", r.lostErr)
	close(r.lost)
}

// State renders the room for the frontend's session banner.
func (r *Room) State() *bridge.SessionState {
	return &bridge.SessionState{
		Phase:    bridge.PhaseJoined,
		Relay:    r.cfg.Relay,
		Room:     r.cfg.Room,
		ID:       r.cfg.ID,
		Nickname: r.cfg.Nickname,
	}
}

// DeclareTrack records a local encoder configuration and republishes the
// catalog so remote participants can decode what follows.
func (r *Room) DeclareTrack(cfg *bridge.TrackConfig) error {
	return r.pub.declareConfig(cfg)
}

// UndeclareTrack withdraws a local track from the catalog, so subscribers
// drop it instead of holding a decoder that will never be fed again.
func (r *Room) UndeclareTrack(kind string) error {
	return r.pub.undeclareConfig(kind)
}

// WriteFrame publishes one encoded frame captured by the frontend.
func (r *Room) WriteFrame(f *bridge.MediaFrame) error {
	return r.pub.writeFrame(f)
}

// watchRoom opens the SUBSCRIBE_NAMESPACE that reports participants
// arriving in and leaving the room.
func (r *Room) watchRoom(ctx context.Context) (*session.NamespaceSubscription, error) {
	prefix := roomPrefix(r.cfg.Room)
	sub, err := r.sess.SubscribeNamespace(ctx, &message.SubscribeNamespace{
		TrackNamespacePrefix: prefix,
	})
	if err != nil {
		return nil, fmt.Errorf("conf: SUBSCRIBE_NAMESPACE %v: %w", prefix, err)
	}
	r.log.Info("watching room", "prefix", nsString(prefix))
	return sub, nil
}

// readAnnouncements turns NAMESPACE / NAMESPACE_DONE notifications into
// remote participants joining and leaving.
//
// The relay sends the namespace *suffix* relative to our subscribed
// prefix, so with prefix ("tlmst", room) a one-field suffix is exactly
// the participant identifier.
func (r *Room) readAnnouncements(sub *session.NamespaceSubscription) {
	defer sub.Close()

	for {
		msg, err := message.Parse(sub)
		if err != nil {
			if r.ctx.Err() == nil && !errors.Is(err, io.EOF) {
				r.log.Warn("namespace watch ended", "err", err)
			}
			return
		}

		switch m := msg.(type) {
		case *message.Namespace:
			id, ok := participantID(m.TrackNamespaceSuffix)
			if !ok {
				r.log.Debug("ignoring namespace announcement",
					"suffix", nsString(m.TrackNamespaceSuffix))
				continue
			}
			r.addRemote(id)

		case *message.NamespaceDone:
			id, ok := participantID(m.TrackNamespaceSuffix)
			if !ok {
				continue
			}
			r.removeRemote(id)

		default:
			r.log.Debug("unexpected namespace-stream message", "type", m.Type().String())
		}
	}
}

// participantID extracts the participant identifier from a NAMESPACE
// suffix, rejecting anything that isn't the single field we expect.
func participantID(suffix wire.TrackNamespace) (string, bool) {
	if len(suffix) != 1 || len(suffix[0]) == 0 {
		return "", false
	}
	return string(suffix[0]), true
}

func (r *Room) addRemote(id string) {
	// The relay announces our own namespace back to us, since our prefix
	// matches it. Skip it: we render the local preview from the camera
	// directly, not through the relay.
	if id == r.cfg.ID {
		return
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if _, exists := r.remotes[id]; exists {
		r.mu.Unlock()
		return
	}
	// Reserve the slot before the subscribe so a duplicate announcement
	// racing this one cannot start a second subscription.
	r.remotes[id] = nil
	r.mu.Unlock()

	r.log.Info("participant joined", "participant", id)
	rem, err := newRemote(r.ctx, r, id, namespaceFor(r.cfg.Room, id))

	r.mu.Lock()
	if err != nil || r.closed {
		delete(r.remotes, id)
		r.mu.Unlock()
		if err != nil {
			r.log.Warn("subscribing to participant failed", "participant", id, "err", err)
			r.sink.SendControl(&bridge.ServerMessage{
				Type:  bridge.MsgError,
				Error: fmt.Sprintf("could not subscribe to %s: %v", id, err),
			})
		} else {
			rem.close()
		}
		return
	}
	r.remotes[id] = rem
	r.mu.Unlock()

	r.publishParticipants()
}

func (r *Room) removeRemote(id string) {
	r.mu.Lock()
	rem, ok := r.remotes[id]
	delete(r.remotes, id)
	r.mu.Unlock()
	if !ok {
		return
	}

	r.log.Info("participant left", "participant", id)
	if rem != nil {
		rem.close()
	}
	r.publishParticipants()
}

// publishParticipants sends the current roster to the frontend.
func (r *Room) publishParticipants() {
	r.mu.Lock()
	out := make([]bridge.Participant, 0, len(r.remotes))
	for _, rem := range r.remotes {
		if rem != nil {
			out = append(out, rem.participant())
		}
	}
	r.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	r.sink.SendControl(&bridge.ServerMessage{
		Type:         bridge.MsgParticipants,
		Participants: out,
	})
}

// nextHandle allocates the bridge handle for one inbound track.
func (r *Room) nextHandle() uint32 { return r.handles.Add(1) }

// runRouter routes every inbound data stream to the handler registered for
// its track alias. It runs for the room's lifetime.
func (r *Room) runRouter() {
	defer close(r.done)
	err := r.router.Run(r.ctx, r.sess)
	if err != nil && r.ctx.Err() == nil && !errors.Is(err, context.Canceled) {
		// Logged only. A dead session surfaces through Lost, which is the
		// one signal the supervisor acts on — reporting it from here too
		// would race that and tell the frontend the same thing twice.
		r.log.Debug("data stream loop ended", "err", err)
	}
}

// Close leaves the room: ends the publications, drops every subscription,
// and closes the session.
func (r *Room) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.leaving.Store(true)
	remotes := make([]*remote, 0, len(r.remotes))
	for _, rem := range r.remotes {
		if rem != nil {
			remotes = append(remotes, rem)
		}
	}
	r.remotes = make(map[string]*remote)
	r.mu.Unlock()

	for _, rem := range remotes {
		rem.close()
	}
	if r.pub != nil {
		r.pub.close()
	}

	_ = r.sess.Close(moqt.SessionNoError, "leaving")
	r.cancel()
	<-r.done
	r.log.Info("left room")
}
