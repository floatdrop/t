package conf

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/floatdrop/moq-go/pkg/moqt/loc"
	"github.com/floatdrop/moq-go/pkg/moqt/message"
	"github.com/floatdrop/moq-go/pkg/moqt/msf"
	"github.com/floatdrop/moq-go/pkg/moqt/session"
	"github.com/floatdrop/moq-go/pkg/moqt/wire"

	"tlmst/internal/bridge"
	"tlmst/internal/telemetry"
)

// SUBSCRIBER_PRIORITY (§10.2.7) per track kind: what this client wants the
// relay to send first when it cannot send everything at once.
//
// The relay schedules a subscriber's streams by this before anything else
// (EffectiveStreamPriority, then SetSendPriority on the outgoing stream), and
// the implicit default when the parameter is omitted is 128 — which is what
// every subscription here used to carry, so audio, video and catalogs were
// ranked equally and drained round-robin. Under congestion that spends the
// scarce capacity evenly on the stream nobody can do without and the stream
// that costs fifty times as much.
//
// The order is what a participant would miss most. A catalog is tiny and is
// how a subscriber learns what exists at all. Audio is what a call is; it is
// also 32 kbps against video's 1.5 Mbps, so protecting it costs almost
// nothing. Video comes last and is the only thing there is meaningfully less
// of when the link is tight.
//
// Well below the 128 default, so anything left unset sorts behind all of it.
//
// Inert today, and worth saying so. The relay computes the §7.2 effective
// priority from this and hands it to SetSendPriority, which type-asserts the
// transport for a per-stream scheduling knob — and moq-go's own documentation
// records that none of its adapters implement one, because quic-go exposes no
// public per-stream priority API and webtransport-go follows suit. So this is
// correct protocol behaviour that changes no scheduling on either transport
// this app can use. It is kept because it costs a parameter, it is what a
// relay that can act on it needs, and it lights up the day a transport grows
// the knob — not because anything measured moved.
const (
	catalogPriority = 10
	audioPriority   = 20
	videoPriority   = 60
)

// videoLevel is how much of a participant's video this client is still asking
// for. The relay demotes us down this ladder by giving up on a subscription;
// time promotes us back up.
type videoLevel int

const (
	videoFull     videoLevel = iota // the primary encoding, every layer of it
	videoBaseOnly                   // the primary encoding without its top temporal layer
	videoSmall                      // the publisher's smaller encoding
	videoNone                       // audio only
)

func (l videoLevel) String() string {
	switch l {
	case videoBaseOnly:
		return "base-only"
	case videoSmall:
		return "small"
	case videoNone:
		return "none"
	default:
		return "full"
	}
}

// maxLag is how far behind the live edge a subscription may slip before it is
// rebuilt to escape it.
//
// Falling behind is not the same problem as being sent too much, and the
// ladder does not solve it. A path that is only mildly over capacity never
// earns the relay's verdict and never moves the drift trend far enough to
// demote — it just delivers everything a little late, and a little later
// again, until the call is a minute behind. Nothing local notices: audio,
// video and their sync are all consistently late together, buffers look
// healthy, and there is no shared clock to measure lateness against.
//
// So the accumulated slip is the signal, and rebuilding the subscription is
// the escape: a fresh SUBSCRIBE starts at the live edge by definition. A
// second and a half is above the quarter second the player trims to and below
// the point where anyone would call it a delay rather than a stutter.
const maxLag = 1500 * time.Millisecond

// resyncCooldown keeps a link that cannot hold the live edge from rebuilding
// its subscriptions continuously. Each rebuild costs a SUBSCRIBE and a
// backfilled group; doing it every second would spend more of the link on
// recovering than on media.
const resyncCooldown = 15 * time.Second

// backfillBlind is how long the drift meter stops measuring after a backfill.
//
// A backfilled group is a couple of seconds of video arriving at once, which
// delays the audio the meter is fed from — real contention, but caused by this
// client and over in a moment, so it says nothing about what the path can
// carry. Long enough to cover the burst and the queue it leaves behind.
const backfillBlind = 4 * time.Second

// backfillWait is how long a live subgroup waits for the group in progress to
// be delivered before going ahead without it. Above the keyframe interval, so
// a backfill that is merely slow is waited for rather than abandoned.
const backfillWait = 3 * time.Second

// audioRebuildCooldown is the shortest gap between rebuilding audio after the
// relay has refused it. Audio is never demoted — there is nothing smaller and
// it is not what filled the link — so the only response is to ask again, and
// the only protection against asking forever is to ask less often.
const audioRebuildCooldown = 5 * time.Second

// videoRecovery is how long a demotion holds before this client tries one step
// back up.
//
// Long enough that a congested minute is not spent flapping: a step up costs a
// fresh SUBSCRIBE, a new handle, a new decoder and a wait for the next keyframe
// — up to the publisher's keyframe interval of blank tile, with no way to ask
// for one sooner. Short enough that a burst of congestion does not pin someone
// to a thumbnail for the rest of the call.
const videoRecovery = 30 * time.Second

// videoRecoveryMax is as long as the wait between attempts to climb back gets.
//
// Without a ceiling the wait would be fixed, and a link that cannot hold the
// full picture never settles: it steps up on schedule, is cut off a lag window
// later, steps up again — a black tile for most of every cycle, for the rest of
// the call, each turn costing a SUBSCRIBE, a decoder and a backfilled group.
// That is worst against a publisher offering no small layer, where the ladder
// has nothing between the full picture and nothing at all.
//
// So a step up that does not survive lengthens the next wait, and the reduced
// state becomes somewhere to stay rather than somewhere to bounce off. Five
// minutes is long enough to stop being the thing anyone notices, and short
// enough that a link which genuinely recovered is not written off for the
// evening.
const videoRecoveryMax = 5 * time.Minute

// remote is one other participant in the room: their catalog
// subscription, whichever media subscriptions their catalog declares, and
// the bridge handles their frames travel under.
type remote struct {
	id  string
	ns  wire.TrackNamespace
	log *slog.Logger

	room *Room

	ctx    context.Context
	cancel context.CancelFunc

	// catalogSub and catalogFetch stay referenced so close can release
	// them; the fetch is nil when the backfill was refused.
	catalogSub   *session.Subscription
	catalogFetch *session.FetchRequest

	// applying serialises catalog application. Each catalog arrives on its
	// own stream and the router reads streams concurrently, so two catalogs
	// can be applied at once — and reconcile is check-then-act: both would
	// see a track as unsubscribed and both would subscribe it, leaving a
	// duplicate subscription and a duplicate decoder. Holding this for the
	// whole apply makes the decisions sequential. It is held across the
	// SUBSCRIBE round trip, which only delays further catalogs for this one
	// participant; other remotes and all media reading are unaffected.
	applying sync.Mutex

	// mu guards everything below, which the catalog reader mutates as
	// the remote's track list changes.
	mu sync.Mutex
	// catalogGroup is the highest catalog group applied so far. Catalogs
	// published in quick succession each land in their own group, so they
	// each get their own stream, and the router reads streams concurrently —
	// they can arrive in any order. Applying an older one over a newer one
	// resubscribes to tracks that never changed, so anything not newer than
	// this is dropped. Publisher group IDs are monotonic, which is what
	// makes them usable as the sequence number here.
	catalogGroup uint64
	// wantVideo/wantAudio are the configs the last applied catalog asked for,
	// kept so a subscribe that failed can be tried again against what the
	// publisher actually wants rather than waiting for them to republish.
	wantVideo    *bridge.TrackConfig
	wantVideoLow *bridge.TrackConfig
	wantAudio    *bridge.TrackConfig
	// retrying marks a resubscribe loop already in flight for this remote.
	retrying   bool
	hasCatalog bool
	// catalogVideo/catalogAudio are what the last catalog declared, which is
	// not the same as what is subscribed now that the frontend can decline
	// video it cannot see. The roster reports these: a participant whose tile
	// is merely scrolled out of view has not turned their camera off, and
	// saying so would be a lie the moment anyone scrolled back.
	catalogVideo bool
	catalogAudio bool
	nickname     string
	version      string
	video        *remoteTrack
	audio        *remoteTrack
	// level is how much of this participant's video is still being asked for,
	// after any demotion the relay has forced on us. It only ever lowers what
	// the frontend asked for, never raises it.
	level videoLevel
	// demotedFor is the handle the current demotion was decided on. A
	// subscription's groups arrive on separate streams and the relay resets
	// each of them, so the same verdict lands more than once; without this
	// every stream of a dead subscription would demote another step.
	demotedFor uint32
	// audioRebuiltAt is when audio was last rebuilt after an overload verdict.
	// Under sustained overload the relay refuses again a lag window later, and
	// without a floor the cycle repeats for the whole call — each turn costing
	// this participant a gap in the sound and a fresh decoder.
	audioRebuiltAt time.Time
	// resyncedAt is when this participant's subscriptions were last rebuilt to
	// escape a slip, so a link that cannot hold the live edge does not rebuild
	// them continuously.
	resyncedAt time.Time
	// recoveryWait is how long the next attempt to climb back waits. It grows
	// each time a step up fails to survive, and resets once one does.
	recoveryWait time.Duration
	// recoveredAt is when the last step up happened, so demote can tell one
	// that survived from one that was cut off almost immediately.
	recoveredAt time.Time
	// recovery lifts the demotion once the link has been quiet. Replaced on
	// each demotion, so the wait always runs from the most recent one.
	recovery *time.Timer
	// closed stops late catalog updates from resurrecting subscriptions
	// after the participant has left.
	closed bool
}

// remoteTrack is one subscribed media track of a remote participant.
type remoteTrack struct {
	handle uint32
	kind   uint8
	// name is the track subscribed, which for video says which encoding this
	// is. Two layers can carry identical configs — same codec, same
	// framerate — so the config alone cannot tell them apart.
	name string
	// config is what the publisher declared. layers is how many subgroups of it
	// this subscription actually asked for, which is the smaller number
	// whenever a §5.1.3 filter declined the top of the stack — and the one
	// reassembly must expect, since a subgroup this client declined is one no
	// group of it should ever wait for.
	config bridge.TrackConfig
	layers uint8
	sub    *session.Subscription
	// fetch is the joining FETCH that backfilled the group in progress when
	// this track was subscribed. Nil on audio, and on video whose backfill was
	// refused.
	fetch *session.FetchRequest
	// backfilled closes once the group in progress has been delivered, or once
	// it is established that none is coming. Live objects wait for it: the two
	// ranges are adjacent by construction, so delivering the backfill first and
	// the subscription second is correct ordering with no reordering buffer —
	// but only if something makes them happen in that order.
	backfilled   chan struct{}
	backfillOnce sync.Once
	label        string
	// delivered is the newest timestamp handed to the frontend on this handle,
	// so the backfill can tell whether it still has anything to contribute.
	// Atomic because the live path writes it from a stream goroutine while the
	// FETCH reads it from its own.
	delivered atomic.Uint64

	// groupsMu guards groups, which holds one reassembler per group currently
	// in flight — keyed by Group ID, entered by every subgroup stream of that
	// group and removed when the last of them ends.
	//
	// More than one at a time only during the overlap where a group's tail is
	// still arriving as the next one opens, so this holds one or two entries in
	// practice. Video with temporal layers is what makes it necessary at all: a
	// group then arrives on several concurrent streams and decode order has to
	// be put back together — see reorder.go.
	groupsMu sync.Mutex
	groups   map[uint64]*groupReassembler
}

// reassemblerFor returns the reassembler for one group, creating it on the
// first stream to arrive for that group, and registers subgroup as a stream
// that may still deliver objects.
//
// Every group is told how many subgroups to expect: what the publisher declared,
// less anything this subscription declined, since the declaration is the only
// thing that knows before the first frame does and the filter is the only thing
// that knows what was asked for. A track carrying one subgroup expects one,
// which is the behaviour it had before layers and the right one for a publisher
// too old to say.
func (t *remoteTrack) reassemblerFor(group, subgroup uint64) *groupReassembler {
	t.groupsMu.Lock()
	defer t.groupsMu.Unlock()
	if t.groups == nil {
		t.groups = make(map[uint64]*groupReassembler)
	}
	g, ok := t.groups[group]
	if !ok {
		g = newGroupReassembler()
		t.groups[group] = g
		t.retireSupersededLocked(group)
	}
	g.OpenSubgroup(subgroup)
	return g
}

// retireSupersededLocked flushes groups far enough behind newest that nothing
// more can be coming for them. The caller holds groupsMu.
//
// This is the bound on waiting for an expected stream that never arrives —
// which a group whose top layer the relay dropped without ever opening will do,
// as will one whose encoder simply had no enhancement frame to send. Neither
// produces any signal to wait for, so the only honest end to the wait is the
// evidence that the publisher has moved on.
//
// One whole group of slack, not none. A group's streams do not end together and
// the relay drains them in whatever order it likes, so the next group opening is
// no proof that this one is finished — flushing on it would discard exactly the
// late layer this is here to protect. Two groups on is proof enough: the
// publisher closes every subgroup of a group before opening the next, so a group
// two behind the newest has been closed at the source for an entire GOP.
func (t *remoteTrack) retireSupersededLocked(newest uint64) {
	for id, g := range t.groups {
		if id+2 > newest {
			continue
		}
		g.Flush()
		delete(t.groups, id)
	}
}

// finishSubgroup reports one subgroup stream as ended, releasing whatever that
// unblocks. The group is forgotten once no stream of it is left, so a call that
// runs for an hour does not accumulate a reassembler per GOP.
func (t *remoteTrack) finishSubgroup(group, subgroup uint64) {
	t.groupsMu.Lock()
	g, ok := t.groups[group]
	t.groupsMu.Unlock()
	if !ok {
		return
	}
	g.CloseSubgroup(subgroup)

	t.groupsMu.Lock()
	defer t.groupsMu.Unlock()
	if g.idle() {
		g.Flush()
		delete(t.groups, group)
	}
}

// dropGroups releases every group still in flight. Called when the track goes
// away, so frames already received and decodable are painted rather than
// discarded with the map.
func (t *remoteTrack) dropGroups() {
	t.groupsMu.Lock()
	groups := t.groups
	t.groups = nil
	t.groupsMu.Unlock()
	for _, g := range groups {
		g.Flush()
	}
}

// doneBackfilling releases the live reader. Safe to call from either side and
// more than once.
func (t *remoteTrack) doneBackfilling() {
	t.backfillOnce.Do(func() { close(t.backfilled) })
}

// newRemote subscribes to a newly discovered participant's catalog. The
// media subscriptions follow once the catalog arrives and names them.
func newRemote(parent context.Context, room *Room, id string, ns wire.TrackNamespace) (*remote, error) {
	ctx, cancel := context.WithCancel(parent)
	r := &remote{
		id:     id,
		ns:     ns,
		log:    room.log.With("participant", id),
		room:   room,
		ctx:    ctx,
		cancel: cancel,
	}

	if err := r.subscribeCatalog(); err != nil {
		cancel()
		return nil, err
	}
	return r, nil
}

// subscribeCatalog opens the catalog subscription plus the Joining FETCH
// that backfills it.
//
// The FETCH is not optional here. A catalog object is published once, when
// the participant joins; SUBSCRIBE with the largest-object filter delivers
// only objects *after* the current largest (§5.1.3), so a participant who
// joins later would never see a catalog that was published before they
// arrived. The Relative Joining FETCH (§10.12.2) with JoiningStart=0
// backfills the current group, which is exactly that catalog object.
func (r *remote) subscribeCatalog() error {
	subMsg := &message.Subscribe{
		Namespace: r.ns,
		Name:      []byte(msf.CatalogTrackName),
		Parameters: message.Parameters{
			message.LargestObjectFilter(),
			message.SubscriberPriorityParam(catalogPriority),
		},
	}
	sub, err := r.room.sess.Subscribe(r.ctx, subMsg)
	if err != nil {
		return fmt.Errorf("conf: SUBSCRIBE catalog %s: %w", r.id, err)
	}
	// Guarded, because this is no longer written only once during setup:
	// resubscribeCatalog replaces it from the liveness goroutine while close
	// may be reading it from another.
	r.mu.Lock()
	previous := r.catalogSub
	r.catalogSub = sub
	r.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	r.log.Info("subscribed to catalog", "alias", sub.TrackAlias())

	r.room.router.HandleSubgroups(sub.TrackAlias(), r.readCatalogStream)
	go r.watchLiveness(sub)

	fetchMsg := &message.Fetch{
		FetchType: message.FetchTypeRelativeJoining,
		Joining: &message.JoiningFetch{
			JoiningRequestID: subMsg.RequestID,
			JoiningStart:     0,
		},
	}
	fetch, err := r.room.sess.Fetch(r.ctx, fetchMsg)
	if err != nil {
		// Not fatal — the subscription above is live, so anything they publish
		// from here is seen. But "from here" is the problem: a participant who
		// has settled does not republish, since a catalog goes out only when
		// track availability changes. Without the backfill they sit in the
		// roster with no nickname and no media for as long as they stay
		// unchanged, which for someone already in the call is the whole of it.
		//
		// So it is worth another go before accepting that. Left to the
		// caller's goroutine rather than the request path, which has a
		// subscription to finish setting up.
		r.log.Warn("catalog joining FETCH failed", "err", err)
		go r.retryCatalogFetch(fetchMsg)
		return nil
	}
	r.mu.Lock()
	r.catalogFetch = fetch
	r.mu.Unlock()
	r.room.router.HandleFetch(fetchMsg.RequestID, r.readCatalogFetch)
	return nil
}

// retryCatalogFetch asks again for the backfill this remote did not get.
//
// Stops as soon as a catalog has arrived by any route: the subscription may
// deliver one first if the participant happens to republish, and a second
// backfill would only be the same object again.
func (r *remote) retryCatalogFetch(fetchMsg *message.Fetch) {
	delay := trackRetryDelay
	for attempt := 1; attempt <= trackRetryLimit; attempt++ {
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(delay):
		}

		r.mu.Lock()
		closed, have := r.closed, r.hasCatalog
		r.mu.Unlock()
		if closed || have {
			return
		}

		fetch, err := r.room.sess.Fetch(r.ctx, fetchMsg)
		if err != nil {
			r.log.Warn("catalog joining FETCH failed again", "attempt", attempt, "err", err)
			delay = min(delay*2, trackRetryMax)
			continue
		}

		r.mu.Lock()
		r.catalogFetch = fetch
		r.mu.Unlock()
		r.room.router.HandleFetch(fetchMsg.RequestID, r.readCatalogFetch)
		r.log.Info("catalog backfill recovered", "attempt", attempt)
		return
	}
	r.log.Warn("giving up on a participant's catalog backfill; " +
		"they will appear only if they republish")
}

// watchLiveness treats the end of the catalog subscription as the
// participant leaving.
//
// NAMESPACE_DONE is the intended departure signal, but it cannot be the
// only one. A participant that crashes never withdraws its namespace, and
// the relay only sends NAMESPACE_DONE to the subscribers that were already
// watching when the namespace was announced — so a participant who joined
// *after* someone else never hears about that person leaving. The catalog
// subscription's request stream is the reliable signal: it carries
// PUBLISH_DONE on a clean exit and simply ends when the session dies.
func (r *remote) watchLiveness(sub *session.Subscription) {
	for {
		msg, err := message.Parse(sub)
		if err != nil {
			if r.ctx.Err() != nil {
				return // we tore the subscription down ourselves
			}
			// A dead session takes every subscription with it, and the
			// supervisor is already rebuilding: removing the participant here
			// would be reporting the session's death once per participant.
			select {
			case <-r.room.sess.Done():
				r.log.Debug("catalog subscription ended with the session")
				return
			default:
			}
			// The session is alive, so this is one request stream that ended —
			// a reset, or something unparseable — and the participant may be
			// perfectly present. Departure is the last conclusion to reach,
			// not the first: removing them here is permanent, because the
			// relay will not re-announce a namespace it has already announced,
			// so they would be gone from this client alone until the next full
			// reconnect while everyone else still sees them.
			if r.resubscribeCatalog(err) {
				return
			}
			r.log.Info("catalog subscription could not be re-established, "+
				"treating participant as gone", "err", err)
			r.room.removeRemote(r.id)
			return
		}
		if done, ok := msg.(*message.PublishDone); ok {
			r.log.Info("participant ended its catalog", "status", done.StatusCode)
			r.room.removeRemote(r.id)
			return
		}
		r.log.Debug("catalog request stream message", "type", msg.Type().String())
	}
}

// resubscribeCatalog rebuilds the catalog subscription after its request
// stream ended while the session was still usable. It reports whether it took
// over the watch — true means a fresh watchLiveness is running and this one
// should stand down.
func (r *remote) resubscribeCatalog(cause error) bool {
	delay := trackRetryDelay
	for attempt := 1; attempt <= trackRetryLimit; attempt++ {
		select {
		case <-r.ctx.Done():
			return true // torn down deliberately; nothing to report
		case <-r.room.sess.Done():
			return true // the supervisor has this
		case <-time.After(delay):
		}

		r.mu.Lock()
		closed := r.closed
		r.mu.Unlock()
		if closed {
			return true
		}

		if err := r.subscribeCatalog(); err != nil {
			r.log.Warn("could not re-subscribe to a catalog",
				"attempt", attempt, "cause", cause, "err", err)
			delay = min(delay*2, trackRetryMax)
			continue
		}
		r.log.Info("catalog subscription re-established", "attempt", attempt)
		return true
	}
	return false
}

func (r *remote) readCatalogStream(s *session.IncomingSubgroupStream) {
	for {
		obj, err := s.ReadDecoded()
		if err != nil {
			if !errors.Is(err, io.EOF) && r.ctx.Err() == nil {
				r.log.Warn("catalog read failed", "err", err)
			}
			return
		}
		r.onCatalog(obj.GroupID, obj.Payload)
	}
}

func (r *remote) readCatalogFetch(s *session.IncomingFetchStream) {
	for {
		obj, err := s.ReadDecoded()
		if err != nil {
			if !errors.Is(err, io.EOF) && r.ctx.Err() == nil {
				r.log.Warn("catalog fetch read failed", "err", err)
			}
			return
		}
		// §11.4.4.2 absence markers carry no payload.
		if obj.EndOfNonExistentRange || obj.EndOfUnknownRange {
			continue
		}
		r.onCatalog(obj.GroupID, obj.Payload)
	}
}

// onCatalog reconciles the remote's subscriptions against a freshly
// received catalog: subscribe to tracks that appeared, drop tracks that
// went away, and re-subscribe when a codec config changed.
func (r *remote) onCatalog(group uint64, payload []byte) {
	cat, err := parseCatalog(payload)
	if err != nil {
		r.log.Warn("catalog parse failed", "err", err)
		return
	}

	r.applying.Lock()
	defer r.applying.Unlock()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if r.hasCatalog && group <= r.catalogGroup {
		r.mu.Unlock()
		r.log.Debug("ignoring superseded catalog",
			"group", group, "applied", r.catalogGroup)
		return
	}
	r.catalogGroup, r.hasCatalog = group, true
	if cat.Nickname != "" {
		r.nickname = cat.Nickname
		r.version = cat.Version
	}
	// §11.3 Complete means they have ended the broadcast, so it declares
	// nothing whatever the track fields say.
	r.catalogVideo = !cat.Complete && (cat.Video != nil || cat.VideoLow != nil)
	r.catalogAudio = !cat.Complete && cat.Audio != nil
	r.mu.Unlock()

	r.log.Info("catalog received",
		"nickname", cat.Nickname,
		"video", cat.Video != nil,
		"audio", cat.Audio != nil,
		"complete", cat.Complete)

	if cat.Complete {
		// §11.3: the publisher ended its broadcast. Drop the media
		// subscriptions but keep watching the catalog — the namespace is
		// still announced, and they may start publishing again.
		r.reconcile(nil, nil, nil)
		r.room.publishParticipants()
		return
	}

	r.reconcile(cat.Video, cat.VideoLow, cat.Audio)
	r.room.publishParticipants()
}

// remember records what the current catalog asks for, so a failed subscribe
// has something to be retried against.
func (r *remote) remember(video, videoLow, audio *bridge.TrackConfig) {
	r.mu.Lock()
	r.wantVideo, r.wantVideoLow, r.wantAudio = video, videoLow, audio
	r.mu.Unlock()
}

// missing reports whether a track that should be subscribed is not.
//
// Video counts only while the frontend wants it. Otherwise deliberately not
// subscribing would read as a subscription that failed, and the retry loop
// would spend its whole budget re-establishing a track nobody asked for before
// announcing to the user that it had given up on it.
//
// The want is read before the lock, not under it: publishParticipants holds the
// room's lock and calls into each remote, so a remote that reached back into
// the room while holding its own would close the cycle.
func (r *remote) missing() bool {
	r.mu.Lock()
	video, videoLow, audio := r.wantVideo, r.wantVideoLow, r.wantAudio
	haveVideo, haveAudio := r.video != nil, r.audio != nil
	r.mu.Unlock()

	// Asked of the chooser rather than inferred from the ladder, because there
	// is more than one way to decline a track deliberately: demoted to none,
	// not visible to the frontend, or demoted one step against a publisher who
	// offers nothing smaller — which resolves to no video at all. Reading only
	// the first of those left the third looking like a subscription that
	// failed, so the retry loop spent all five attempts chasing a track this
	// client had decided not to take, and then told the user the participant
	// could not be reached while their audio was working.
	_, wantedVideo, _ := r.chooseVideoLayer(video, videoLow)
	return (wantedVideo != nil && !haveVideo) || (audio != nil && !haveAudio)
}

// scheduleResubscribe starts the retry loop, unless one is already running.
func (r *remote) scheduleResubscribe() {
	r.mu.Lock()
	if r.retrying || r.closed {
		r.mu.Unlock()
		return
	}
	r.retrying = true
	r.mu.Unlock()
	go r.resubscribe()
}

// resubscribe retries the tracks the catalog asked for and did not get.
//
// Without this a single rejected SUBSCRIBE is permanent. Nothing re-runs
// reconcile except the publisher republishing their catalog, and they only do
// that when their track availability changes — which a participant who is
// simply talking never does. So one transient failure and you never see that
// person for the rest of the call, while the frontend renders them as though
// their camera were merely off, because that is what a participant with no
// video track looks like.
func (r *remote) resubscribe() {
	defer func() {
		r.mu.Lock()
		r.retrying = false
		r.mu.Unlock()
	}()

	delay := trackRetryDelay
	for attempt := 1; attempt <= trackRetryLimit; attempt++ {
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(delay):
		}

		r.mu.Lock()
		closed, video, audio := r.closed, r.wantVideo, r.wantAudio
		videoLow := r.wantVideoLow
		r.mu.Unlock()
		if closed || !r.missing() {
			return
		}

		// Through reconcile under the same lock a catalog would take, so a
		// retry and an arriving catalog cannot both subscribe the same track.
		r.applying.Lock()
		r.reconcile(video, videoLow, audio)
		r.applying.Unlock()

		if !r.missing() {
			r.log.Info("track subscription recovered", "attempt", attempt)
			return
		}
		delay = min(delay*2, trackRetryMax)
	}

	r.log.Warn("giving up on a participant's tracks", "attempts", trackRetryLimit)
	r.room.sink.SendControl(&bridge.ServerMessage{
		Type:  bridge.MsgError,
		Error: fmt.Sprintf("Could not subscribe to %s's media.", r.displayName()),
	})
}

// displayName is the nickname if the catalog carried one, else the id.
func (r *remote) displayName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nickname != "" {
		return r.nickname
	}
	return r.id
}

// reconcile brings the media subscriptions in line with the wanted
// configs. A nil config means the track should not be subscribed.
func (r *remote) reconcile(video, videoLow, audio *bridge.TrackConfig) {
	r.remember(video, videoLow, audio)
	name, wanted, layers := r.chooseVideoLayer(video, videoLow)
	r.syncTrack(&r.video, name, bridge.KindVideo, wanted, layers)
	r.syncTrack(&r.audio, AudioTrack, bridge.KindAudio, audio, 0)
}

// chooseVideoLayer picks which of a publisher's video encodings to take.
//
// What they publish and what we can use are different questions. The answer to
// the second is the frontend's — it knows how big each tile will be drawn, and
// which are on screen at all — and it only ever subtracts: a layer the catalog
// does not declare cannot be wanted into existence.
//
// The fallbacks matter more than the preference. A publisher on an older build
// offers one video track and no low layer, and a publisher whose primary
// encoder died may offer only the small one; in both cases taking what exists
// beats taking nothing, because nothing is a permanently blank tile.
// The third return is how many temporal layers to ask for, zero meaning all of
// them. It is what makes the base-only rung a filter on the subscription this
// client already holds rather than a different track: same encoding, same
// decoder, same handle, no keyframe to wait for — the whole reason a temporal
// layer is a cheaper step than a smaller picture.
func (r *remote) chooseVideoLayer(
	video, videoLow *bridge.TrackConfig,
) (string, *bridge.TrackConfig, uint8) {
	// Both room lookups first, and neither under this remote's lock:
	// publishParticipants holds the room's and calls inward, so reaching back
	// the other way would close the cycle.
	if !r.room.wantsVideo(r.id) {
		return VideoTrack, nil, 0
	}
	wantsSmall := r.room.wantsLowLayer(r.id)

	r.mu.Lock()
	level := r.level
	r.mu.Unlock()

	// The demotion only ever lowers what the frontend asked for. It cannot
	// raise it: a tile the size of a thumbnail does not want the full picture
	// because the link happens to be quiet.
	switch {
	case level == videoNone:
		return VideoTrack, nil, 0

	case level == videoBaseOnly &&
		shedsALayer(video, wantsSmall, subgroupFiltersPermitted(r.room.sess)):
		// The cheap step: the same subscription with its top layer declined.
		//
		// Only against a publisher that has one to decline. A rung that resolves
		// to exactly what the rung above it resolves to is not a step down, and
		// taking it as one is how this was got wrong before — the ladder spent a
		// rung, shed nothing, and arrived at the next verdict no lighter than it
		// started. Where there is no layer to drop, this falls through and
		// behaves as the smaller encoding, which is what the ladder did before
		// the rung existed.
		return VideoTrack, video, 1

	case level >= videoSmall || level == videoBaseOnly:
		// Demoted by the relay. If this publisher offers nothing smaller then
		// there is no smaller thing to ask for, and re-asking for the full
		// picture would be offering back the load that was just refused — so
		// the only step down left is off.
		if videoLow != nil {
			return VideoLowTrack, videoLow, 0
		}
		return VideoTrack, nil, 0

	case wantsSmall && videoLow != nil:
		return VideoLowTrack, videoLow, 0

	case video != nil:
		return VideoTrack, video, 0

	case videoLow != nil:
		return VideoLowTrack, videoLow, 0
	}
	return VideoTrack, nil, 0
}

// applyInterest re-runs reconcile against the catalog this remote last
// published, after the frontend has changed what it can see.
func (r *remote) applyInterest() {
	r.mu.Lock()
	video, videoLow, audio, closed := r.wantVideo, r.wantVideoLow, r.wantAudio, r.closed
	r.mu.Unlock()
	if closed {
		return
	}
	// Through the same lock a catalog would take, so an interest change and an
	// arriving catalog cannot both decide the same track's fate at once.
	r.applying.Lock()
	defer r.applying.Unlock()
	r.reconcile(video, videoLow, audio)
}

// syncTrack subscribes, unsubscribes, or resubscribes one track so its
// live state matches want. slot points at r.video or r.audio.
func (r *remote) syncTrack(
	slot **remoteTrack, name string, kind uint8, want *bridge.TrackConfig, layers uint8,
) {
	r.mu.Lock()
	current := *slot
	closed := r.closed
	r.mu.Unlock()

	switch {
	case want == nil && current == nil:
		return

	case want == nil:
		r.dropTrack(slot)
		return

	case current != nil && current.name == name && current.config == *want &&
		current.layers == effectiveLayers(want, layers):
		// Already subscribed to this exact track with this config, and asking
		// for the same layers of it. The layer count belongs in that comparison
		// because the base-only rung changes neither the name nor the config —
		// without it, stepping onto or off that rung would decide nothing had
		// changed and leave the subscription exactly as it was.
		return

	case current != nil:
		// The publisher changed codec or resolution. The frontend keys
		// its decoder off the handle, so tear the old one down and
		// announce a fresh handle rather than reconfiguring in place.
		if current.name == name {
			r.log.Info("track re-encoded, resubscribing", "track", name,
				"from", fmt.Sprintf("%dx%d", current.config.Width, current.config.Height),
				"to", fmt.Sprintf("%dx%d", want.Width, want.Height))
		} else if current.name == name {
			r.log.Info("temporal layers changed, resubscribing",
				"track", name, "from", current.layers,
				"to", effectiveLayers(want, layers))
		} else {
			r.log.Info("layer changed, resubscribing",
				"from", current.name, "to", name)
		}
		r.dropTrack(slot)
	}

	if closed {
		return
	}
	if err := r.subscribeTrack(slot, name, kind, want, layers); err != nil {
		r.log.Warn("track subscribe failed", "track", name, "err", err)
		r.scheduleResubscribe()
	}
}

// effectiveLayers resolves how many subgroups a subscription will actually
// carry: what was asked for, bounded by what the publisher says it has. Zero
// asked for means all of them, and a publisher that declares nothing gets the
// single subgroup every track had before layers existed.
func effectiveLayers(cfg *bridge.TrackConfig, want uint8) uint8 {
	declared := max(cfg.TemporalLayers, 1)
	if want == 0 || want > declared {
		return declared
	}
	return want
}

func (r *remote) subscribeTrack(
	slot **remoteTrack, name string, kind uint8, cfg *bridge.TrackConfig, layers uint8,
) error {
	priority := uint8(videoPriority)
	if kind == bridge.KindAudio {
		priority = audioPriority
	}
	subMsg := &message.Subscribe{
		Namespace: r.ns,
		Name:      []byte(name),
		Parameters: message.Parameters{
			message.LargestObjectFilter(),
			message.SubscriberPriorityParam(priority),
		},
	}
	// Declining the layers above this one, by the §5.1.3 Range Filter that
	// names the subgroups worth sending. Asked of the relay rather than dropped
	// on arrival, because a frame discarded here has already been paid for on
	// the wire, and the wire is what ran out.
	subscribed := effectiveLayers(cfg, layers)
	if declared := max(cfg.TemporalLayers, 1); subscribed < declared {
		subMsg.Parameters = append(subMsg.Parameters,
			message.RangeFilterParam(&message.RangeFilter{
				Type:   message.ParamSubgroupFilter,
				Ranges: []message.Range{{Start: 0, End: uint64(subscribed - 1)}},
			}))
	}
	sub, err := r.room.sess.Subscribe(r.ctx, subMsg)
	if err != nil {
		return fmt.Errorf("conf: SUBSCRIBE %s %s: %w", r.id, name, err)
	}

	handle := r.room.nextHandle()
	label := telemetry.InPrefix + r.id + "/" + name
	track := &remoteTrack{
		handle:     handle,
		kind:       kind,
		name:       name,
		config:     *cfg,
		layers:     subscribed,
		sub:        sub,
		label:      label,
		backfilled: make(chan struct{}),
	}
	if kind != bridge.KindVideo {
		// Only video is backfilled; audio has nothing to wait behind.
		track.doneBackfilling()
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		sub.Close()
		return nil
	}
	*slot = track
	nickname := r.nickname
	r.mu.Unlock()

	counter := r.room.counters.Track(label)
	r.room.router.HandleSubgroups(sub.TrackAlias(), func(s *session.IncomingSubgroupStream) {
		r.readMedia(s, track, counter)
	})

	// Announce the handle before any frame carrying it can arrive, so the
	// frontend always has a configured decoder waiting.
	r.room.sink.SendControl(&bridge.ServerMessage{
		Type: bridge.MsgRemoteTrack,
		Track: &bridge.RemoteTrack{
			Handle:      handle,
			Participant: r.id,
			Nickname:    nickname,
			Config:      *cfg,
		},
	})
	r.log.Info("subscribed to track",
		"track", name, "alias", sub.TrackAlias(), "handle", handle, "codec", cfg.Codec)

	if kind == bridge.KindVideo {
		// On its own goroutine: it is a FETCH round trip with no deadline, and
		// nothing here needs its result — the objects it brings are forwarded
		// from the handler it registers. Done inline it sat in front of
		// everything behind this subscribe, holding `applying` across the trip:
		// the participant's audio waited for it, so did any catalog arriving
		// meanwhile, and against a track with nothing published yet the wait
		// was long enough to time tests out. A congested link is exactly where
		// that trip is slowest and where audio can least afford to queue behind
		// a picture.
		go r.backfillGroup(subMsg.RequestID, track, counter)
	}
	return nil
}

// backfillGroup asks for the group already in progress, so a fresh video
// subscription has a picture now rather than at the next keyframe.
//
// A SUBSCRIBE with the largest-object filter starts at the first object *after*
// whatever exists, which for video is the middle of a GOP — undecodable, since
// playback discards inbound frames until it sees a keyframe. So a new
// subscription showed nothing at all until the publisher's next keyframe, up to
// the whole keyframe interval away, and there is no way to ask a remote
// publisher to produce one sooner. Every layer change, every demotion and every
// tile scrolled back into view paid that.
//
// The Relative Joining FETCH (§10.12.2) with JoiningStart=0 resolves to
// {largest.Group, 0} through {largest.Group, largest.Object + 1} — the current
// group from its keyframe up to exactly where the subscription begins. The two
// ranges are adjacent: no object arrives twice and none is skipped. It is the
// same pairing the catalog subscription has always used.
//
// Video only. The same backfill on audio would deliver up to half a second of
// sound that has already been and gone, straight into a player whose whole job
// is staying near the live edge.
func (r *remote) backfillGroup(
	subscribeID uint64,
	track *remoteTrack,
	counter *telemetry.TrackCounter,
) {
	fetchMsg := &message.Fetch{
		FetchType: message.FetchTypeRelativeJoining,
		Joining: &message.JoiningFetch{
			JoiningRequestID: subscribeID,
			JoiningStart:     0,
		},
		// Not what makes the ordering correct — awaitBackfill does that, and a
		// priority could not: it is a scheduling hint, and two streams read by
		// two goroutines can interleave at the receiver whatever order they
		// were sent in. Measured, the gate alone passes and this alone does
		// not.
		//
		// Level with live video is still where this belongs — it is the same
		// pictures and they are needed first — but see the priority constants:
		// nothing acts on any of them on either transport available here.
		Parameters: message.Parameters{
			message.SubscriberPriorityParam(videoPriority),
		},
	}
	fetch, err := r.room.sess.Fetch(r.ctx, fetchMsg)
	if err != nil {
		// Degraded, not fatal: without it the tile stays blank until the next
		// keyframe, which is exactly what it did before this existed.
		r.log.Debug("video backfill FETCH refused", "handle", track.handle, "err", err)
		track.doneBackfilling()
		return
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		fetch.Close()
		track.doneBackfilling()
		return
	}
	track.fetch = fetch
	r.mu.Unlock()

	// The audio meter, not this track's: drift is measured on audio, and audio
	// is what this burst is about to get in the way of.
	r.room.counters.
		Track(telemetry.InPrefix+r.id+"/"+AudioTrack).
		SuspendSkew(time.Now(), backfillBlind)

	r.room.router.HandleFetch(fetchMsg.RequestID, func(s *session.IncomingFetchStream) {
		r.readMediaFetch(s, track, counter)
	})
}

// readMediaFetch drains a backfilled group, forwarding it exactly as the live
// path does.
func (r *remote) readMediaFetch(
	s *session.IncomingFetchStream,
	track *remoteTrack,
	counter *telemetry.TrackCounter,
) {
	// However this ends — delivered, refused, reset — the live stream stops
	// waiting on it here.
	defer track.doneBackfilling()

	// A FETCH answers in object-ID order, which is decode order only while a
	// group is on one subgroup. Layered, each subgroup owns its own ID range
	// (see layerObjectStride), so the answer arrives as the whole base layer
	// followed by the whole enhancement layer — every enhancement frame after
	// every frame it sits between. Ordering it means having all of it, which is
	// affordable precisely here: a backfill is one group, and the live path is
	// already waiting for the last of it before it delivers anything.
	//
	// Only when there is something to order. An unlayered track streams out as
	// it arrives, exactly as it did before.
	layered := track.layers > 1
	var pending []bridge.MediaFrame
	defer func() {
		slices.SortStableFunc(pending, func(a, b bridge.MediaFrame) int {
			return cmp.Compare(a.Timestamp, b.Timestamp)
		})
		for i := range pending {
			if !r.deliverBackfilled(track, &pending[i]) {
				return
			}
		}
	}()

	counter.AddGroup()
	for {
		obj, err := s.ReadDecoded()
		if err != nil {
			if !errors.Is(err, io.EOF) && r.ctx.Err() == nil {
				r.log.Debug("video backfill ended", "handle", track.handle, "err", err)
			}
			return
		}
		// §11.4.4.2 absence markers carry no payload.
		if obj.EndOfNonExistentRange || obj.EndOfUnknownRange {
			continue
		}

		decoded, err := loc.Decode(obj.Properties, obj.Payload)
		if err != nil {
			r.log.Warn("LOC decode failed in backfill", "handle", track.handle, "err", err)
			continue
		}

		counter.AddObject(len(decoded.Payload))
		frame := bridge.MediaFrame{
			Kind:      track.kind,
			Handle:    track.handle,
			Timestamp: scaleTimestamp(decoded.Properties),
			KeyFrame:  obj.ObjectID == 0,
			Config:    decoded.Properties.VideoConfig,
			Payload:   decoded.Payload,
		}
		if layered {
			pending = append(pending, frame)
			continue
		}
		if !r.deliverBackfilled(track, &frame) {
			return
		}
	}
}

// deliverBackfilled forwards one backfilled frame unless the live path has
// already moved past it, and reports whether the backfill is still worth
// draining.
//
// The backfill exists to fill the group in progress in *front* of the live
// edge, and awaitBackfill holds the live path back so that it can. Under a
// bottleneck it loses that race: a whole group cannot cross in the time the
// gate allows, live frames go first, and what the FETCH eventually delivers is
// a frame the decoder is already past. Measured against a real relay behind a
// 32 kB/s link, every inversion over half a second was one of these, all of
// them in a subscription's opening seconds and none after.
//
// Handing them over anyway is the worst of the options: a decoder fed a frame
// older than one it has decoded either errors or emits a picture presentation
// will discard as stale, and either way the tile is no better for it. So the
// race is conceded rather than papered over — the frames are dropped, and the
// rest of the fetch with them, since a FETCH answers in order and nothing after
// this can be newer than the live edge either.
func (r *remote) deliverBackfilled(track *remoteTrack, frame *bridge.MediaFrame) bool {
	if live := track.delivered.Load(); live > 0 && frame.Timestamp <= live {
		r.log.Debug("backfill lost the race with the live edge",
			"handle", track.handle, "frame", frame.Timestamp, "live", live)
		return false
	}
	track.advance(frame.Timestamp)
	r.room.sink.SendMedia(frame)
	return true
}

// dropTrack closes one media subscription and tells the frontend to
// retire its decoder.
func (r *remote) dropTrack(slot **remoteTrack) {
	// The fetch is read under the same lock that backfillGroup writes it
	// under: that now runs on its own goroutine, so a track dropped while its
	// backfill is still being set up would otherwise race the assignment and
	// leave the FETCH open — still delivering a whole group to a handle the
	// frontend has been told to retire.
	r.mu.Lock()
	track := *slot
	*slot = nil
	var fetch *session.FetchRequest
	if track != nil {
		fetch = track.fetch
	}
	r.mu.Unlock()
	if track == nil {
		return
	}

	// Before the subscription closes, so anything a still-open stream was
	// holding back is painted rather than discarded with the map.
	track.dropGroups()

	track.sub.Close()
	if fetch != nil {
		fetch.Close()
	}
	r.room.counters.Forget(track.label)
	r.room.sink.SendControl(&bridge.ServerMessage{
		Type:      bridge.MsgTrackGone,
		TrackGone: &bridge.RemoteTrackID{Handle: track.handle, Participant: r.id},
	})
}

// readMedia drains one subgroup stream, unwrapping each object's LOC
// container and forwarding the encoded frame to the frontend.
func (r *remote) readMedia(
	s *session.IncomingSubgroupStream,
	track *remoteTrack,
	counter *telemetry.TrackCounter,
) {
	if !r.awaitBackfill(track) {
		return
	}
	// After the stream, not during it: this can rebuild the subscription, and
	// there is nothing left to read by then.
	defer r.checkLagForStream(track, counter)

	// One group can arrive on several subgroup streams at once — one per
	// temporal layer — each on its own goroutine. Objects go through the
	// group's reassembler rather than straight to the frontend so the decoder
	// sees them in decode order however the transport interleaved them.
	group, subgroup := s.Header.GroupID, s.Header.SubgroupID
	reassembler := track.reassemblerFor(group, subgroup)
	defer track.finishSubgroup(group, subgroup)

	// Counted once per group, by the stream that opens it. Every layer of a
	// group is the same group, so counting per stream would report a group
	// count that tracked the layer count instead.
	if subgroup == 0 {
		counter.AddGroup()
	}
	for {
		obj, err := s.ReadDecoded()
		if err != nil {
			r.reportMediaEnd(track, err)
			return
		}

		decoded, err := loc.Decode(obj.Properties, obj.Payload)
		if err != nil {
			r.log.Warn("LOC decode failed", "handle", track.handle, "err", err)
			continue
		}

		frame := bridge.MediaFrame{
			Kind:      track.kind,
			Handle:    track.handle,
			Timestamp: scaleTimestamp(decoded.Properties),
			// The first object of every group opens it, and for video
			// every group opens on a keyframe (see the publisher). Object 0
			// of subgroup 0 specifically: each layer numbers from its own
			// base now (see layerObjectStride), so the enhancement layer has
			// an object 0 too and it is an ordinary delta frame.
			KeyFrame:      subgroup == 0 && obj.ObjectID == 0,
			Payload:       decoded.Payload,
			TemporalLayer: uint8(subgroup),
		}
		switch track.kind {
		case bridge.KindVideo:
			frame.Config = decoded.Properties.VideoConfig
		case bridge.KindAudio:
			frame.Config = decoded.Properties.AudioConfig
			frame.AudioLevel = decoded.Properties.AudioLevel
			frame.HasAudioLevel = decoded.Properties.HasAudioLevel
			// Audio carries the inbound delay trend for this publisher: it is
			// produced on a fixed 20 ms cadence and is the one track never
			// dropped, so the measurement survives whatever else gets shed.
			counter.AddArrival(time.Now(), frame.Timestamp)
		}

		counter.AddObject(len(decoded.Payload))
		// Keyed on the publisher's emission index, not the object ID: each
		// subgroup numbers its objects from its own base, so an ID orders a
		// stream against itself and nothing else. See reorder.go.
		reassembler.Push(subgroup, emissionIndex(obj.ObjectID, decoded.Properties), func() {
			track.advance(frame.Timestamp)
			r.room.sink.SendMedia(&frame)
		})
	}
}

// advance records a frame as delivered, keeping the newest timestamp this
// handle has reached. Only ever forwards: the live path can deliver a group out
// of order relative to another group, and the high-water mark is what the
// backfill needs, not the last thing that happened to go out.
func (t *remoteTrack) advance(timestamp uint64) {
	for {
		was := t.delivered.Load()
		if timestamp <= was || t.delivered.CompareAndSwap(was, timestamp) {
			return
		}
	}
}

// emissionIndex reads the object's position in its group's emission order.
//
// Falls back to the Object ID for a publisher that stamps none, which is right
// for the only kind there is: one subgroup per group, where the IDs count from
// zero without gaps and are the emission order. A layered publisher always
// stamps it — the layout it uses is what makes the ID unusable.
func emissionIndex(objectID uint64, props loc.Properties) uint64 {
	for _, kv := range props.Extras {
		if kv.Type == propEmissionIndex {
			return kv.IntVal
		}
	}
	return objectID
}

// checkLagForStream examines the slip once per audio subgroup.
//
// It used to count objects inside the read loop, which could never fire: that
// loop runs once per stream, one stream is one group, and an audio group is
// twenty-five objects — so a counter looking for fifty never got there and the
// whole escape hatch was unreachable. A group is the natural unit anyway; at
// the 500 ms cadence audio groups run on, this looks about twice a second.
func (r *remote) checkLagForStream(track *remoteTrack, counter *telemetry.TrackCounter) {
	if track.kind != bridge.KindAudio {
		return
	}
	r.checkLag(counter)
}

// awaitBackfill holds a live subgroup until the group in progress has been
// delivered, reporting whether it is still worth reading.
//
// Both arrive on their own stream and the router reads streams concurrently,
// so without this they interleave — and they interleave *most* on the link
// where the backfill takes longest, which is the one it exists for. The
// subscriber then hands its decoder the keyframe, then live frames whose
// reference frames have not arrived, then older frames with timestamps going
// backwards. Playback does not reorder and only gates on the first keyframe,
// so the picture smears and the tile jumps forward and back for the length of
// the backfill.
//
// Bounded, because a backfill that never completes must not silence the track
// for good: past the deadline the live objects go through and the picture is
// whatever the pre-backfill behaviour was — blank until the next keyframe.
func (r *remote) awaitBackfill(track *remoteTrack) bool {
	select {
	case <-track.backfilled:
		return true
	case <-r.ctx.Done():
		return false
	default:
	}

	timer := time.NewTimer(backfillWait)
	defer timer.Stop()
	select {
	case <-track.backfilled:
	case <-r.ctx.Done():
		return false
	case <-timer.C:
		r.log.Debug("backfill did not finish in time; taking the live stream",
			"handle", track.handle)
		// Latched so later groups of this track do not wait again.
		track.doneBackfilling()
	}
	return true
}

// reportMediaEnd says why one media stream stopped.
//
// A group ending is the ordinary case and stays at Debug — every group ends,
// once per GOP. What this exists to separate out is the relay resetting the
// stream because this subscriber could not keep up, which is the only inbound
// capacity signal anybody sends us and was previously logged, at Debug,
// identically to a group finishing normally.
//
// Acting on it is not optional. The relay does not merely reset the stream, it
// terminates the subscription — so a track this happens to is not being
// delivered again, and nothing else would ever rebuild it. That tile stayed
// frozen for the rest of the call.
//
// Rebuilding it as it was would ask for exactly the traffic the relay just
// refused, and would ask again every lag window. So the rebuild steps down
// instead: the full picture becomes the publisher's smaller encoding, and the
// smaller encoding becomes audio only.
func (r *remote) reportMediaEnd(track *remoteTrack, err error) {
	if r.ctx.Err() != nil {
		return // we tore this down ourselves
	}
	if code, ok := streamReset(err); ok {
		if overloadReset(code) {
			// On its own goroutine: this one is draining a stream, and the
			// rebuild waits on a SUBSCRIBE round trip.
			go r.demote(track, resetName(code))
			return
		}
		r.log.Info("media stream reset by the peer",
			"handle", track.handle, "code", resetName(code))
		return
	}
	if !errors.Is(err, io.EOF) {
		r.log.Debug("media read ended", "handle", track.handle, "err", err)
	}
}

// demote rebuilds a subscription the relay gave up on, one step smaller.
//
// Audio is never demoted. There is no smaller version of it, it is what a call
// is, and at 32 kbps it is not what filled the link — so it is rebuilt as it
// was, through the ordinary retry path.
func (r *remote) demote(track *remoteTrack, code string) {
	if r.ctx.Err() != nil {
		return
	}

	if track.kind == bridge.KindAudio {
		// Guarded the way the video path below is. One verdict is reset once
		// per open stream, so it lands more than once; unguarded, a late copy
		// tears down the healthy subscription the first copy just rebuilt.
		r.mu.Lock()
		if r.closed || r.audio == nil || r.audio.handle != track.handle ||
			time.Since(r.audioRebuiltAt) < audioRebuildCooldown {
			r.mu.Unlock()
			return
		}
		r.audioRebuiltAt = time.Now()
		r.mu.Unlock()

		r.log.Warn("the relay stopped forwarding audio: we are not keeping up",
			"handle", track.handle, "code", code, "action", "rebuilding it unchanged")
		r.dropTrack(&r.audio)
		r.scheduleResubscribe()
		return
	}

	// Read before the lock: publishParticipants holds the room's and calls
	// inward, so reaching back the other way under this one would close the
	// cycle — the same ordering chooseVideoLayer keeps.
	wantsSmall := r.room.wantsLowLayer(r.id)
	canFilter := subgroupFiltersPermitted(r.room.sess)

	r.mu.Lock()
	// Only the subscription that is still current, and only once for it. Every
	// open group of a dead subscription is reset separately, so this arrives
	// once per stream for a verdict that was reached once.
	if r.closed || r.video == nil || r.video.handle != track.handle ||
		r.demotedFor == track.handle {
		r.mu.Unlock()
		return
	}
	r.demotedFor = track.handle
	from := r.level
	r.level = stepDown(r.level, r.wantVideo, wantsSmall, canFilter)
	to := r.level
	// Cut off again soon after climbing back: the link has not recovered, so
	// wait longer before believing it has. Doubling rather than resetting is
	// what gives the reduced state somewhere to settle.
	backoff := ""
	if !r.recoveredAt.IsZero() && time.Since(r.recoveredAt) < r.recoveryWait {
		r.recoveryWait = min(r.recoveryWait*2, videoRecoveryMax)
		backoff = r.recoveryWait.String()
	}
	video, videoLow, audio := r.wantVideo, r.wantVideoLow, r.wantAudio
	r.mu.Unlock()

	fields := []any{
		"handle", track.handle, "code", code,
		"from", from.String(), "to", to.String(),
	}
	if backoff != "" {
		fields = append(fields, "next_attempt_in", backoff)
	}
	r.log.Warn("the relay stopped forwarding a track: we are not keeping up", fields...)

	r.applying.Lock()
	r.reconcile(video, videoLow, audio)
	r.applying.Unlock()

	r.scheduleRecovery()
}

// subgroupFiltersPermitted reports whether the peer will accept the §5.1.3
// Range Filter the base-only rung is expressed as.
//
// §10.3.1.6: MAX_FILTER_RANGES is what a peer will accept across all Range
// Filter parameters of one request, and omitting it means zero — filters
// prohibited. Sending one anyway is not ignored, it is rejected: the relay
// answers INVALID_FILTER and the SUBSCRIBE fails outright. Against a relay that
// does not advertise the option, the first congestion event therefore took the
// rung, lost the subscription, exhausted the retry loop and gave up on the
// participant's video for the rest of the call — worse than the demotion it was
// meant to be an improvement on. Measured against a deployed relay; every
// in-process test passed, because the test relay permits them.
func subgroupFiltersPermitted(sess *session.Session) bool {
	for _, kv := range sess.PeerOptions() {
		if kv.Type == uint64(message.SetupOptionMaxFilterRanges) {
			return kv.IntVal >= 1
		}
	}
	return false
}

// shedsALayer reports whether the base-only rung would actually take anything
// off the wire for this publisher.
//
// It only does against one that publishes a layer to decline, only while the
// frontend is asking for the primary encoding — a tile already on the small one
// has taken a bigger step than this rung offers — and only where the peer
// accepts the filter that expresses it. Everywhere else it resolves to exactly
// what the rung below it resolves to.
//
// The ladder skips it there rather than resolving it away, which is a real
// distinction: a rung that changes nothing still costs a step, so the next
// verdict arrives with the link no lighter and one fewer step left to take. That
// is how this was got wrong on the first attempt.
func shedsALayer(video *bridge.TrackConfig, wantsSmall, canFilter bool) bool {
	return canFilter && !wantsSmall && video != nil && video.TemporalLayers > 1
}

// stepDown is the next rung below cur, skipping base-only where it is not one.
func stepDown(cur videoLevel, video *bridge.TrackConfig, wantsSmall, canFilter bool) videoLevel {
	if cur >= videoNone {
		return videoNone
	}
	next := cur + 1
	if next == videoBaseOnly && !shedsALayer(video, wantsSmall, canFilter) {
		next = videoSmall
	}
	return next
}

// stepUp is the next rung above cur, skipping base-only on the way back for the
// same reason stepDown skips it on the way down.
func stepUp(cur videoLevel, video *bridge.TrackConfig, wantsSmall, canFilter bool) videoLevel {
	if cur <= videoFull {
		return videoFull
	}
	next := cur - 1
	if next == videoBaseOnly && !shedsALayer(video, wantsSmall, canFilter) {
		next = videoFull
	}
	return next
}

// scheduleRecovery tries one step back up once the link has been quiet for
// videoRecovery. Without it a single congested moment would hold a participant
// at a thumbnail — or at nothing — for the rest of the call.
func (r *remote) scheduleRecovery() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if r.recovery != nil {
		r.recovery.Stop()
	}
	if r.recoveryWait <= 0 {
		r.recoveryWait = videoRecovery
	}
	r.recovery = time.AfterFunc(r.recoveryWait, r.recover)
	r.mu.Unlock()
}

// recover restores one step of what was demoted, and schedules the next step
// if there is still ground to make up.
//
// One step at a time on purpose. Going straight back to the full picture after
// a demotion to none would re-offer the whole load that the relay refused, and
// find out whether it fits by being cut off again.
func (r *remote) recover() {
	if r.ctx.Err() != nil {
		return
	}

	wantsSmall := r.room.wantsLowLayer(r.id)
	canFilter := subgroupFiltersPermitted(r.room.sess)

	r.mu.Lock()
	if r.closed || r.level == videoFull {
		r.mu.Unlock()
		return
	}
	from := r.level
	r.level = stepUp(r.level, r.wantVideo, wantsSmall, canFilter)
	to := r.level
	r.recoveredAt = time.Now()
	// Cleared so the next verdict on the rebuilt subscription is acted on.
	r.demotedFor = 0
	// All the way back means the link held: start again from the short wait.
	if r.level == videoFull {
		r.recoveryWait = videoRecovery
	}
	video, videoLow, audio := r.wantVideo, r.wantVideoLow, r.wantAudio
	more := r.level != videoFull
	r.mu.Unlock()

	r.log.Info("the link has been quiet; asking for more video again",
		"from", from.String(), "to", to.String())

	r.applying.Lock()
	r.reconcile(video, videoLow, audio)
	r.applying.Unlock()

	if more {
		r.scheduleRecovery()
	}
}

// checkLag rebuilds this participant's subscriptions when they have slipped too
// far behind the live edge.
//
// Measured on audio, because that is the track the drift meter is fed from and
// the one that is never dropped — but acted on for both, since a path that is
// late is late for everything on it, and leaving video where it was would put
// the picture a minute behind the sound.
func (r *remote) checkLag(counter *telemetry.TrackCounter) {
	lag, ok := counter.Lag()
	if !ok || lag < float64(maxLag/time.Millisecond) {
		return
	}

	r.mu.Lock()
	if r.closed || time.Since(r.resyncedAt) < resyncCooldown {
		r.mu.Unlock()
		return
	}
	r.resyncedAt = time.Now()
	video, videoLow, audio := r.wantVideo, r.wantVideoLow, r.wantAudio
	r.mu.Unlock()

	r.log.Warn("this participant has slipped behind the live edge; resubscribing",
		"lag_ms", int(lag), "limit_ms", int(maxLag/time.Millisecond))

	// Dropped rather than reconciled in place: reconcile leaves a track whose
	// config has not changed exactly where it is, which is the whole problem —
	// it is not the wrong track, it is the right track too far back.
	r.dropTrack(&r.video)
	r.dropTrack(&r.audio)

	r.applying.Lock()
	r.reconcile(video, videoLow, audio)
	r.applying.Unlock()
}

// scaleTimestamp converts a LOC timestamp to microseconds, which is what
// WebCodecs expects. A publisher that used a different timescale is
// rescaled rather than misinterpreted.
func scaleTimestamp(p loc.Properties) uint64 {
	if !p.HasTimestamp {
		return 0
	}
	if !p.HasTimescale || p.Timescale == 0 || p.Timescale == timescaleMicros {
		return p.Timestamp
	}
	return p.Timestamp * timescaleMicros / p.Timescale
}

// close ends every subscription for this participant.
func (r *remote) close() {
	r.mu.Lock()
	r.closed = true
	if r.recovery != nil {
		r.recovery.Stop()
		r.recovery = nil
	}
	r.mu.Unlock()

	r.dropTrack(&r.video)
	r.dropTrack(&r.audio)

	// Copied under the lock rather than closed under it: a resubscribe can be
	// replacing catalogSub concurrently, and Close is not something to hold a
	// mutex across.
	r.mu.Lock()
	fetch, sub := r.catalogFetch, r.catalogSub
	r.mu.Unlock()
	if fetch != nil {
		fetch.Close()
	}
	if sub != nil {
		sub.Close()
	}
	r.cancel()
}

// participant renders the remote for the frontend's roster.
func (r *remote) participant() bridge.Participant {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bridge.Participant{
		ID:       r.id,
		Nickname: r.nickname,
		Version:  r.version,
		HasVideo: r.catalogVideo,
		HasAudio: r.catalogAudio,
		// What we are actually taking, which HasVideo above deliberately does
		// not say — it reports what they publish. Without this a participant
		// this client gave up on looked exactly like one who switched their
		// camera off, while HasVideo insisted the camera was on: a blank tile
		// and two signals disagreeing about why.
		VideoLevel: r.level.String(),
	}
}
