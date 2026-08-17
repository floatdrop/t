package conf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/floatdrop/moq-go/pkg/moqt"
	"github.com/floatdrop/moq-go/pkg/moqt/loc"
	"github.com/floatdrop/moq-go/pkg/moqt/message"
	"github.com/floatdrop/moq-go/pkg/moqt/msf"
	"github.com/floatdrop/moq-go/pkg/moqt/session"
	"github.com/floatdrop/moq-go/pkg/moqt/wire"

	"t/internal/bridge"
	"t/internal/telemetry"
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

// videoBackfillTimeout is the ceiling on how long live video waits behind a
// joining FETCH that has not finished.
//
// The backfill goes in front of live because that is the order a decoder needs
// them in, and the price of that ordering is that a backfill which stalls holds
// the picture it was supposed to produce. So the wait is bounded, and the bound
// is what the backfill costs when the relay is not answering rather than what
// it costs when it is: a base-layer GOP is a couple of hundred kilobytes and
// arrives in well under a second on any link this app stays joined on.
//
// Three seconds because the fallback is not silence — it is the next keyframe,
// which the new-group request has already been sent for. Waiting past the point
// where that would have arrived buys nothing.
const videoBackfillTimeout = 3 * time.Second

// audioRebuildCooldown is the shortest gap between rebuilding audio after the
// relay has refused it. Audio is never demoted — there is nothing smaller and
// it is not what filled the link — so the only response is to ask again, and
// the only protection against asking forever is to ask less often.
const audioRebuildCooldown = 5 * time.Second

// videoGiveUpAfter is how many times the relay may give up on a participant's
// video inside videoGiveUpWindow before this client stops asking for it.
//
// More than one is deliberate. A single verdict is often a burst rather than a
// verdict about the link, and the honest first answer is to ask again — a fresh
// SUBSCRIBE starts at the live edge, and the §8 timeout on the enhancement
// layer means what comes back is lighter than what was refused without anyone
// negotiating it. Three inside a minute is a link that has answered the
// question.
const (
	videoGiveUpAfter  = 3
	videoGiveUpWindow = time.Minute
)

// videoRecovery is how long video stays off before this client asks again.
//
// Long enough that a congested minute is not spent flapping: a step up costs a
// fresh SUBSCRIBE, a new handle, a new decoder and a wait for the next keyframe
// — up to the publisher's keyframe interval of blank tile, with no way to ask
// for one sooner. Short enough that a burst of congestion does not pin someone
// to a thumbnail for the rest of the call.
const videoRecovery = 30 * time.Second

// videoRecoveryMax is as long as the wait between attempts to ask for video
// again ever gets.
//
// There has to be a ceiling on the *wait*, not on the attempts. A link that
// cannot hold the picture will otherwise never settle: it asks on schedule, is
// cut off a lag window later, asks again — a black tile for most of every
// cycle, each turn costing a SUBSCRIBE, a decoder and a wait for a keyframe. So
// an attempt that does not survive lengthens the next wait.
//
// It used to lengthen to five minutes, and that was too far. Nothing here is
// permanent — a link that recovers gets its video back — but five minutes of
// audio-only is indistinguishable from having given up, and a call that was
// briefly congested should not spend the rest of the hour proving it. A minute
// is long enough to stop being the thing anyone notices and short enough that
// the picture comes back while the conversation is still happening.
const videoRecoveryMax = time.Minute

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
	wantVideo *bridge.TrackConfig
	wantAudio *bridge.TrackConfig
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
	os           string
	video        *remoteTrack
	audio        *remoteTrack
	// level is how much of this participant's video is still being asked for,
	// after any demotion the relay has forced on us. It only ever lowers what
	// the frontend asked for, never raises it.
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
	// rebuilds counts how often the relay has given up on this participant's
	// video lately, and rebuiltSince is when that count started. Enough of them
	// in one window means the link is not going to carry it, and video is set
	// aside until the recovery timer tries again.
	rebuilds     int
	rebuiltSince time.Time
	// videoOff is set while video has been given up on, so reconcile stops
	// asking for it.
	videoOff bool
	// recoveryWait is how long the next attempt waits. It grows on a rebuild
	// that did not survive — see videoRecoveryMax.
	recoveryWait time.Duration
	// recoveredAt is when video was last turned back on, so a rebuild can tell
	// one that survived from one that did not.
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
	// config is what the publisher declared.
	config bridge.TrackConfig
	sub    *session.Subscription
	label  string

	// backfilled is the group the joining FETCH replays, and hasBackfill says
	// there is one. Set before any stream can arrive and never written again,
	// so both are read without a lock.
	//
	// It is also the boundary the live path treats specially: that group has two
	// sources — the FETCH for what came before the subscribe, the live stream
	// for what came after — and they have to be ordered against each other
	// inside one group. See backfillGate and readMedia.
	backfilled  uint64
	hasBackfill bool
	// backfill coordinates the FETCH with the live stream continuing the same
	// group. Nil when there is nothing to backfill.
	backfill *backfillGate
	// fetch is the joining FETCH backfilling that group. Written and read under
	// the remote's lock, so a track dropped mid-setup cannot leave it open.
	fetch *session.FetchRequest

	// groupsMu guards groups, which holds one reassembler per group currently
	// in flight — keyed by Group ID, entered by every subgroup stream of that
	// group and retired once the publisher has moved two groups past it.
	//
	// More than one at a time only during the overlap where a group's tail is
	// still arriving as the next one opens, so this holds two or three entries
	// in practice. Video with temporal layers is what makes it necessary at all:
	// a group then arrives on several concurrent streams and decode order has to
	// be put back together — see reorder.go.
	groupsMu sync.Mutex
	groups   map[uint64]*groupReassembler

	// order holds this track's groups in the order the publisher wrote them,
	// which a per-group reassembler cannot: see grouporder.go.
	order *groupOrderer

	// onDrop, when non-nil, reports an object the reassembler gave up on. Held
	// on the track rather than assigned to each reassembler after the fact:
	// every subgroup stream of a group reaches the same reassembler on its own
	// goroutine, so a field set by whichever arrived first races the Push that
	// reads it. Wired once in subscribeTrack, which has the logger.
	onDrop func(group, subgroup, index uint64, reason dropReason)
}

// reassemblerFor returns the reassembler for one group, creating it on the
// first stream to arrive for that group.
func (t *remoteTrack) reassemblerFor(group uint64) *groupReassembler {
	t.groupsMu.Lock()
	defer t.groupsMu.Unlock()
	if t.groups == nil {
		t.groups = make(map[uint64]*groupReassembler)
	}
	g, ok := t.groups[group]
	if !ok {
		var onDrop func(subgroup, index uint64, reason dropReason)
		if t.onDrop != nil {
			onDrop = func(subgroup, index uint64, reason dropReason) {
				t.onDrop(group, subgroup, index, reason)
			}
		}
		g = newGroupReassembler(onDrop)
		t.groups[group] = g
		t.retireSupersededLocked(group)
	}
	return g
}

// retireSupersededLocked forgets groups far enough behind newest that nothing
// more can be coming for them. The caller holds groupsMu.
//
// A group is retired by the publisher moving on rather than by its streams
// ending, because the streams are the unreliable part: a relay resets one
// without notice, and one it sheds never opens at all. A newer group could only
// have been opened by the publisher, which closes every subgroup of a group
// before opening the next.
//
// One whole group of slack, not none. A group's streams do not end together and
// the relay drains them in whatever order it likes, so the next group opening is
// no proof that this one is finished. Two groups on is proof enough.
//
// What a retired group loses is whatever it was still holding, and a group only
// holds an enhancement object that overtook the base frame it references. In
// emission order nothing is held at all; the case that survives to retirement is
// one that overtook at the very tail of a group, where no later base frame comes
// to release it. A frame of the layer built to be disposable, against carrying a
// reassembler per GOP for the length of the call.
func (t *remoteTrack) retireSupersededLocked(newest uint64) {
	for id := range t.groups {
		if id+2 > newest {
			continue
		}
		delete(t.groups, id)
	}
}

// dropGroups forgets every group still in flight, called when the track goes
// away. Nothing is emitted on the way out: what a group still holds is
// enhancement frames waiting on a base frame that is no longer coming, and the
// decoder they would go to is about to be retired too.
func (t *remoteTrack) dropGroups() {
	t.groupsMu.Lock()
	t.groups = nil
	t.groupsMu.Unlock()
	t.order.Drop()
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

	r.room.router.HandleSubgroups(sub.TrackAlias(), nil, r.readCatalogStream)
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
		r.os = cat.OS
	}
	// §11.3 Complete means they have ended the broadcast, so it declares
	// nothing whatever the track fields say.
	r.catalogVideo = !cat.Complete && cat.Video != nil
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
		r.reconcile(nil, nil)
		r.room.publishParticipants()
		return
	}

	r.reconcile(cat.Video, cat.Audio)
	r.room.publishParticipants()
}

// remember records what the current catalog asks for, so a failed subscribe
// has something to be retried against.
func (r *remote) remember(video, audio *bridge.TrackConfig) {
	r.mu.Lock()
	r.wantVideo, r.wantAudio = video, audio
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
	video, audio := r.wantVideo, r.wantAudio
	haveVideo, haveAudio := r.video != nil, r.audio != nil
	r.mu.Unlock()

	// Asked of the chooser rather than assumed, because declining video is a
	// decision this client makes in two ways — the tile is not on screen, or
	// video has been given up on — and neither is a subscription that failed.
	// Reading it wrongly sent the retry loop chasing a track nobody asked for
	// and then told the user the participant could not be reached while their
	// audio was working.
	wantedVideo := r.chooseVideo(video)
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
		closed := r.closed
		r.mu.Unlock()
		if closed || !r.missing() {
			return
		}

		// Through reconcile under the same lock a catalog would take, so a
		// retry and an arriving catalog cannot both subscribe the same track.
		r.reapply()

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

// reapply brings the subscriptions back in line with whatever the last catalog
// asked for.
//
// The wants are read *inside* applying, which is the whole point of it existing
// as a method. Every caller used to read them, then block on applying, then
// reconcile — and reconcile begins by remembering what it was handed. So a
// catalog that arrived while a caller was blocked had its configs written back
// over by the older snapshot the caller was still holding: a publisher that
// re-encoded from 720p to 360p had the 360p subscription torn down and rebuilt
// under the 720p config it no longer publishes, and stayed that way until it
// republished.
func (r *remote) reapply() {
	r.applying.Lock()
	defer r.applying.Unlock()

	r.mu.Lock()
	video, audio, closed := r.wantVideo, r.wantAudio, r.closed
	r.mu.Unlock()
	if closed {
		return
	}
	r.reconcile(video, audio)
}

// reconcile brings the media subscriptions in line with the wanted
// configs. A nil config means the track should not be subscribed.
func (r *remote) reconcile(video, audio *bridge.TrackConfig) {
	r.remember(video, audio)
	r.syncTrack(&r.video, VideoTrack, bridge.KindVideo, r.chooseVideo(video))
	r.syncTrack(&r.audio, AudioTrack, bridge.KindAudio, audio)
}

// videoStateOf names what this client is taking, for the roster.
func videoStateOf(off bool) string {
	if off {
		return "none"
	}
	return "full"
}

// chooseVideo decides whether to subscribe this participant's video.
//
// There used to be a choice of encodings and a ladder that walked down them.
// Both are gone. A publisher offers one video track, and what a link that
// cannot carry it gets instead is the same track with its enhancement layer
// shed — by the relay, under the §8 timeout the publisher marks it with, with
// nobody asking. Degrading is something the transport does now, not something
// this client negotiates.
//
// So the only questions left are whether the frontend can see the tile and
// whether video has been given up on for this participant.
func (r *remote) chooseVideo(video *bridge.TrackConfig) *bridge.TrackConfig {
	// Before the lock: publishParticipants holds the room's and calls inward, so
	// reaching back the other way would close the cycle.
	if !r.room.wantsVideo(r.id) {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.videoOff {
		return nil
	}
	return video
}

// applyInterest re-runs reconcile against the catalog this remote last
// published, after the frontend has changed what it can see.
func (r *remote) applyInterest() {
	r.mu.Lock()
	video, audio, closed := r.wantVideo, r.wantAudio, r.closed
	r.mu.Unlock()
	if closed {
		return
	}
	// Through the same lock a catalog would take, so an interest change and an
	// arriving catalog cannot both decide the same track's fate at once.
	r.applying.Lock()
	defer r.applying.Unlock()
	r.reconcile(video, audio)
}

// syncTrack subscribes, unsubscribes, or resubscribes one track so its
// live state matches want. slot points at r.video or r.audio.
func (r *remote) syncTrack(
	slot **remoteTrack, name string, kind uint8, want *bridge.TrackConfig,
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

	case current != nil && current.name == name && current.config == *want:
		// Already subscribed to this exact track with this config.
		return

	case current != nil:
		// The publisher changed codec or resolution. The frontend keys
		// its decoder off the handle, so tear the old one down and
		// announce a fresh handle rather than reconfiguring in place.
		if current.name == name {
			r.log.Info("track re-encoded, resubscribing", "track", name,
				"from", fmt.Sprintf("%dx%d", current.config.Width, current.config.Height),
				"to", fmt.Sprintf("%dx%d", want.Width, want.Height))
		} else {
			r.log.Info("layer changed, resubscribing",
				"from", current.name, "to", name)
		}
		r.dropTrack(slot)
	}

	if closed {
		return
	}
	if err := r.subscribeTrack(slot, name, kind, want); err != nil {
		r.log.Warn("track subscribe failed", "track", name, "err", err)
		r.scheduleResubscribe()
	}
}

func (r *remote) subscribeTrack(
	slot **remoteTrack, name string, kind uint8, cfg *bridge.TrackConfig,
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
	sub, err := r.room.sess.Subscribe(r.ctx, subMsg)
	if err != nil {
		return fmt.Errorf("conf: SUBSCRIBE %s %s: %w", r.id, name, err)
	}

	handle := r.room.nextHandle()
	label := telemetry.InPrefix + r.id + "/" + name
	track := &remoteTrack{
		handle: handle,
		kind:   kind,
		name:   name,
		config: *cfg,
		sub:    sub,
		label:  label,
	}
	track.order = newGroupOrderer()
	// §10.2.16: the publisher MUST send LARGEST_OBJECT in SUBSCRIBE_OK once
	// anything has been published on the track. Its absence therefore means an
	// empty track — the subscription starts at the beginning, there is no group
	// in progress, and there is nothing to backfill.
	if largest, ok := sub.OK.Parameters.Find(message.ParamLargestObject); ok {
		track.backfilled, track.hasBackfill = largest.Group, true
		group := largest.Group
		track.backfill = newBackfillGate(func() { track.order.Finish(group) })
	}
	if kind == bridge.KindVideo {
		// Video only: audio has no layers, so its reassembler never holds
		// anything and never has anything to give up on. Set before any stream
		// can reach reassemblerFor, which is what keeps it off the racy path
		// the reassembler used to take it on.
		track.onDrop = func(group, subgroup, index uint64, reason dropReason) {
			r.log.Warn("reassembler dropped frame",
				"handle", handle, "group", group, "subgroup", subgroup,
				"index", index, "reason", reason)
		}
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
	// The arrival hook claims each group's turn in the order the transport
	// delivered its stream, which is the order the publisher opened them in.
	// Doing it inside readMedia instead would claim them in whatever order the
	// scheduler ran the goroutines, and a burst then reads as the later group
	// having come first — see groupOrderer.Open.
	arrived := func(s *session.IncomingSubgroupStream) {
		if s.Header.SubgroupID != baseSubgroup {
			return
		}
		track.order.Open(s.Header.GroupID)
		if track.backfill != nil {
			// Which side of the backfilled group this stream falls on decides
			// who ends that group's turn. Recorded here for the same reason
			// Open is: this is where the transport's delivery order still
			// exists. See backfillGate.
			track.backfill.announce(s.Header.GroupID, track.backfilled)
		}
	}
	r.room.router.HandleSubgroups(sub.TrackAlias(), arrived,
		func(s *session.IncomingSubgroupStream) {
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

	if kind == bridge.KindVideo && track.hasBackfill {
		// Claimed here, before the FETCH is even sent, so the mark starts at the
		// group being backfilled rather than at the first group live delivers.
		// groupOrderer.Open normally runs off the accept loop where the arrival
		// order still exists; there is no stream to hang it on here, and the
		// order is not in doubt — the backfilled group precedes every group the
		// subscription can carry. Released by backfillGroup, however it ends.
		track.order.Open(track.backfilled)

		// On its own goroutine: it is a FETCH round trip, and nothing here needs
		// its result — the objects it brings are forwarded from the handler it
		// registers. Done inline it sat in front of everything behind this
		// subscribe, holding `applying` across the trip: the participant's audio
		// waited for it, so did any catalog arriving meanwhile, and against a
		// track with nothing published yet the wait was long enough to time
		// tests out. A congested link is exactly where that trip is slowest and
		// where audio can least afford to queue behind a picture.
		go r.backfillGroup(subMsg.RequestID, track, counter)
	}
	if kind == bridge.KindVideo {
		// Independent of the backfill and useful even when there was nothing to
		// backfill: ask the publisher to cut a new group, which for video is a
		// keyframe. Also on its own goroutine — it is a REQUEST_UPDATE round
		// trip through the relay.
		go r.requestNewGroup(track)
	}
	return nil
}

// backfillGate coordinates the two sources of one backfilled group: the FETCH
// replaying what the publisher wrote before the subscribe, and the live stream
// carrying what it wrote after. It answers two questions, and they are not the
// same one.
//
// **Which goes first.** Both carry the base layer of one group, and to the
// reassembler a base object is one to emit on arrival — so live going first
// would advance the mark past the whole backfill and drop it as late. Wait
// holds the live stream until the FETCH is done. Nothing else can arrange this:
// the group orderer works a group at a time, and this is inside one.
//
// **When the group's turn ends**, which is what lets the next group through.
// The live stream ends it if there is one, at the moment its stream ends, which
// is the same rule every other group follows. If there is not one, something
// still has to, and the signal for "there is not one" is the publisher having
// moved on: a base stream announced for a later group. The publisher closes
// every subgroup of a group before opening the next, so a later group's stream
// existing proves this one is finished — the same reasoning retireSupersededLocked
// runs on.
//
// Evidence rather than a timer, and that distinction is the whole of it. A
// deadline here was wrong in a way that only showed under load: a live stream
// that arrives late — because the relay was still draining what the publisher
// had already written — is not absent, and cutting it off to end the turn on
// schedule discards the frames it was carrying and strands the group short.
type backfillGate struct {
	// done closes when the FETCH has finished with the group, however it
	// finished. Read by wait, closed once by release.
	done        chan struct{}
	releaseOnce sync.Once

	mu sync.Mutex
	// claimed is true once a live stream for this group has been announced, so
	// that stream is the one that will end the turn. movedOn is true once a
	// stream for a later group has been, which is the proof that no live stream
	// for this one is coming. finished is true once the turn has been ended.
	claimed  bool
	movedOn  bool
	released bool
	finished bool
	// end is what ends the group's turn — track.order.Finish bound to the group.
	// Supplied at construction, so the gate owns no ordering of its own.
	end func()
}

func newBackfillGate(end func()) *backfillGate {
	return &backfillGate{done: make(chan struct{}), end: end}
}

// announce records that a base-layer stream has arrived for group, which is
// either the backfilled group or one after it. Called from the router's arrival
// hook, so it runs on the accept loop in the order the transport delivered the
// streams — the only place that order still exists, and the reason the decision
// is made here rather than when a reader goroutine happens to be scheduled.
//
// Nothing is emitted here. Ending the turn drains the orderer, which emits, and
// the accept loop is what every other stream on the session is queued behind —
// so when the evidence is complete the finish goes to its own goroutine.
func (g *backfillGate) announce(group, backfilled uint64) {
	g.mu.Lock()
	if group <= backfilled {
		g.claimed = true
	} else {
		g.movedOn = true
	}
	due := g.finishDueLocked()
	g.mu.Unlock()
	if due {
		go g.finish()
	}
}

// finishDueLocked reports whether nothing more is coming for the backfilled
// group: the FETCH is done, the publisher has moved past it, and no live stream
// ever turned up to carry the rest. The caller holds mu.
func (g *backfillGate) finishDueLocked() bool {
	return g.released && g.movedOn && !g.claimed && !g.finished
}

// wait blocks until the FETCH has finished with the group, reporting false if
// the remote went away first.
func (g *backfillGate) wait(ctx context.Context) bool {
	select {
	case <-g.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// stale reports whether the group's turn is already over, so a stream still
// carrying it has nothing the orderer would accept. It is the case the group
// orderer documents and cannot recover from either: a stream opened for an
// earlier group after the mark has moved past it.
func (g *backfillGate) stale() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.finished
}

// finish ends the group's turn, once.
func (g *backfillGate) finish() {
	g.mu.Lock()
	if g.finished {
		g.mu.Unlock()
		return
	}
	g.finished = true
	g.mu.Unlock()
	g.end()
}

// release records that the FETCH is done with the group, letting through the
// live stream that continues it — and ending the turn here if the publisher has
// already moved on and no such stream exists.
func (g *backfillGate) release() {
	g.releaseOnce.Do(func() {
		g.mu.Lock()
		g.released = true
		due := g.finishDueLocked()
		g.mu.Unlock()
		close(g.done)
		if due {
			g.finish()
		}
	})
}

// backfillGroup replays the group already in progress, so a fresh video
// subscription has a picture now rather than at the publisher's next keyframe.
//
// A SUBSCRIBE with the largest-object filter starts at the first object *after*
// whatever exists, which for video is the middle of a GOP — undecodable, since
// playback discards inbound frames until it sees a keyframe. So a new
// subscription showed nothing at all until the next keyframe, up to the whole
// keyframe interval away. Every layer change, every demotion, every lag resync
// and every tile scrolled back into view paid it.
//
// The Relative Joining FETCH (§10.12.2) with JoiningStart=0 resolves to
// {largest.Group, 0} through {largest.Group, largest.Object + 1} — the current
// group from its keyframe up to exactly where the subscription begins. The two
// ranges are adjacent: no object arrives twice and none is skipped. It is the
// same pairing the catalog subscription has always used.
//
// # Why this one can win where the last one could not
//
// There was a backfill here before and it was removed, because it *raced* live
// video and lost: it delivered into the same handle behind a high-water mark on
// what had already been shown, and every backfilled frame is older than every
// live frame by construction, so the first live object to land made the whole
// replay stale — after a round trip and a group of bytes. It paid the entire
// cost of the thing it existed to avoid.
//
// The ranges were never racing, though. They are adjacent and disjoint, which
// makes them a concatenation: backfill, then live. So the backfill claims its
// group's turn in the track's groupOrderer (see subscribeTrack) and live waits
// behind it, through the same mechanism that already orders any group against
// any later one. There is still exactly one path from the wire to the decoder —
// which is what removing the last backfill was really for — and this feeds into
// it rather than running alongside.
//
// # Why it is affordable
//
// SUBGROUP_FILTER (§5.1.3) narrows it to the base layer. That is the reference
// chain and nothing else: every base frame back to the keyframe is needed to
// decode the next one, and no enhancement frame in the past is referenced by
// anything at all. Roughly halves the bytes, and every byte left is load-bearing.
//
// It is also what lets the response stream. A FETCH answers in ascending Object
// ID, and the layers own disjoint ID ranges (see layerObjectStride), so an
// unfiltered backfill arrives as the whole base layer followed by the whole
// enhancement layer — decode order for neither, so it has to be buffered whole
// and sorted before any of it can go out. Filtered to one subgroup, ascending
// Object ID *is* decode order, and each frame goes to the decoder as it lands.
// The old backfill buffered; that is most of why it was always last.
//
// Video only. The same backfill on audio would deliver up to half a second of
// sound that has already been and gone, into a player whose whole job is
// staying near the live edge.
func (r *remote) backfillGroup(
	subscribeID uint64,
	track *remoteTrack,
	counter *telemetry.TrackCounter,
) {
	// The group's turn is claimed before the FETCH is sent and has to be handed
	// on however this ends — refused, reset, drained, or timed out. Left
	// unreleased, the live stream continuing this group never starts and every
	// later group sits behind one nothing more is coming for, until the
	// cross-group backlog gives up on it: seconds of video held for nothing.
	finish := track.backfill.release
	// The FETCH itself has no deadline, and the objects arrive on a stream that
	// can simply stall. This is the ceiling on how long live video waits for a
	// picture that would have been nicer to have first.
	timer := time.AfterFunc(videoBackfillTimeout, func() {
		r.log.Debug("video backfill timed out", "handle", track.handle,
			"group", track.backfilled)
		finish()
	})
	defer timer.Stop()

	fetchMsg := &message.Fetch{
		FetchType: message.FetchTypeRelativeJoining,
		Joining: &message.JoiningFetch{
			JoiningRequestID: subscribeID,
			JoiningStart:     0,
		},
		Parameters: message.Parameters{
			// Level with live video: it is the same pictures and they are needed
			// first. See the priority constants for how much that is worth on
			// either transport available here.
			message.SubscriberPriorityParam(videoPriority),
			// The base layer alone — see the doc comment. One contiguous range,
			// well inside any sane MAX_FILTER_RANGES; a relay advertising zero
			// prohibits Range Filters outright and refuses the FETCH, which is
			// handled below as any other refusal.
			message.RangeFilterParam(&message.RangeFilter{
				Type:   message.ParamSubgroupFilter,
				Ranges: []message.Range{{Start: baseSubgroup, End: baseSubgroup}},
			}),
		},
	}
	fetch, err := r.room.sess.Fetch(r.ctx, fetchMsg)
	if err != nil {
		// Degraded, not fatal: without it the tile stays blank until the next
		// keyframe, which is what it did before this existed.
		r.log.Debug("video backfill FETCH refused", "handle", track.handle, "err", err)
		finish()
		return
	}

	// Both sides under the lock, because a track dropped while its backfill was
	// still being set up would otherwise leave the FETCH open, still delivering
	// a whole group to a handle the frontend has been told to retire.
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		fetch.Close()
		finish()
		return
	}
	track.fetch = fetch
	r.mu.Unlock()

	r.room.router.HandleFetch(fetchMsg.RequestID, func(s *session.IncomingFetchStream) {
		defer finish()
		r.readMediaFetch(s, track, counter)
	})
}

// readMediaFetch drains a backfilled group, forwarding it exactly as the live
// path does — through the group's reassembler and the track's group orderer, so
// there is one ordering to reason about and not two.
//
// The reassembler is a pass-through here and that is by construction: the FETCH
// is filtered to the base layer, a base-layer object is emitted on arrival, and
// nothing ever waits. See reorder.go.
func (r *remote) readMediaFetch(
	s *session.IncomingFetchStream,
	track *remoteTrack,
	counter *telemetry.TrackCounter,
) {
	group := track.backfilled
	reassembler := track.reassemblerFor(group)
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
		index := emissionIndex(obj.ObjectID, decoded.Properties)
		frame := bridge.MediaFrame{
			Kind:      track.kind,
			Handle:    track.handle,
			Timestamp: scaleTimestamp(decoded.Properties),
			// The object that opened the group, which for video is the keyframe.
			// Emission index rather than Object ID for the reason the live path
			// gives; the filter means every object here is a base-layer one, so
			// the subgroup half of that test is already answered.
			KeyFrame:      index == 0,
			Config:        decoded.Properties.VideoConfig,
			Payload:       decoded.Payload,
			TemporalLayer: baseSubgroup,
		}
		reassembler.Push(baseSubgroup, index, func() {
			track.order.Push(group, index, func() { r.room.sink.SendMedia(&frame) })
		})
	}
}

// requestNewGroup asks the publisher to cut a new group, which for video is a
// keyframe — the other half of arriving with a picture, and the half that does
// not depend on anything being cached.
//
// The backfill answers "show me what I missed"; this answers "start me a fresh
// one". It is what decouples the keyframe interval from the join latency: a
// subscriber no longer waits out the publisher's own schedule, so the interval
// becomes a bitrate-and-loss trade decided on its own merits rather than the
// thing a blank tile is measured in.
//
// §10.2.13 NEW_GROUP_REQUEST, carried on a REQUEST_UPDATE rather than on the
// SUBSCRIBE itself. A relay only forwards the SUBSCRIBE-borne form upstream
// when the SUBSCRIBE makes it open a *new* upstream subscription, and in this
// topology it never does: every participant PUBLISHes to the relay, so the
// upstream for the track already exists and is shared by everyone watching.
//
// The relay does the rate limiting, which is the reason to let it: it forwards
// only a request above the current largest group and only when none of equal or
// greater value is outstanding, so a whole room joining at once costs the
// publisher one keyframe rather than one each. The publisher rate-limits again
// on its own side (see App.requestKeyFrame).
//
// Best effort throughout. A publisher that does not advertise DYNAMIC_GROUPS
// (§12.6) — an older build, or a track that cannot honour it — makes the relay
// decline silently, and the backfill and the next scheduled keyframe are what
// remain. That is exactly the behaviour before this existed.
func (r *remote) requestNewGroup(track *remoteTrack) {
	// The value is the group being asked for: one past the largest known, or
	// zero for "no group information", which §10.2.13 defines as always
	// forwardable. Asking for a group that already exists is not forwarded, so
	// the +1 is what makes the request mean anything.
	var want uint64
	if track.hasBackfill {
		want = track.backfilled + 1
	}
	if _, err := track.sub.Update(r.ctx, message.Parameters{
		message.NewGroupRequestParam(want),
	}); err != nil {
		if r.ctx.Err() == nil {
			r.log.Debug("new-group request declined",
				"handle", track.handle, "group", want, "err", err)
		}
		return
	}
	r.log.Debug("asked the publisher for a new group",
		"handle", track.handle, "group", want)
}

// dropTrack closes one media subscription and tells the frontend to
// retire its decoder.
func (r *remote) dropTrack(slot **remoteTrack) {
	r.mu.Lock()
	track := *slot
	*slot = nil
	var fetch *session.FetchRequest
	if track != nil {
		fetch = track.fetch
		track.fetch = nil
	}
	r.mu.Unlock()
	if track == nil {
		return
	}

	// The groups in flight go with the track. What one still holds is
	// enhancement objects waiting on a base frame that is no longer coming, and
	// the decoder they would go to is being retired in the next breath.
	track.dropGroups()

	// Ahead of the subscription, because a backfill still draining would
	// otherwise keep delivering a whole group to a handle the frontend has just
	// been told to retire.
	if fetch != nil {
		fetch.Close()
	}
	track.sub.Close()
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
	// After the stream, not during it: this can rebuild the subscription, and
	// there is nothing left to read by then.
	defer r.checkLagForStream(track, counter)

	// One group can arrive on several subgroup streams at once — one per
	// temporal layer — each on its own goroutine. Objects go through the
	// group's reassembler rather than straight to the frontend so the decoder
	// sees them in decode order however the transport interleaved them.
	group, subgroup := s.Header.GroupID, s.Header.SubgroupID

	// The group the backfill replays is the one place the two delivery paths meet,
	// and they meet *inside* a group, where the group orderer — which works group
	// by group — cannot separate them.
	//
	// What arrives here for that group is the base layer, and it is decodable:
	// the largest object at subscribe time is a base object (see objectIDFor), so
	// the objects after it continue the chain the backfill is replaying, and the
	// enhancement layer of that group is what the filter withholds instead. It
	// used to be the other way round, and everything live carried for this group
	// was undecodable.
	//
	// It has to land behind the backfill, though, and nothing downstream can
	// arrange that: to the reassembler a base object is one to emit on arrival,
	// so this would go out first and the whole backfill would then arrive below
	// the mark and be dropped as late. So it waits, and takes over responsibility
	// for ending the group's turn — see backfillGate.
	//
	// Anything else for that group is past. An enhancement object can only reach
	// here from a publisher on the older ID layout, where it is undecodable for
	// the reason above; the stream is reset rather than drained so the relay
	// stops spending the link on it.
	backfilledGroup := track.hasBackfill && group <= track.backfilled
	if backfilledGroup {
		if subgroup != baseSubgroup || track.backfill.stale() {
			s.Cancel(moqt.StreamResetInternalError)
			return
		}
		// Through the gate rather than the plain Finish below, because this
		// group has two sources and the turn ends when the last of them lets go.
		defer track.backfill.finish()
		if !track.backfill.wait(r.ctx) {
			return
		}
	}

	// The end of this stream is the end of its group's turn, however it ended —
	// closed by the publisher or reset by a relay. Deferred so a read that fails
	// part-way releases the groups waiting behind it rather than stranding them.
	// The group's turn was claimed on the accept loop, where the arrival order
	// still exists; this is the other end of it.
	//
	// The base layer alone, matching the Open there: nothing may ever wait on
	// the enhancement layer. It is the one a relay delays and sheds outright, so
	// a group that waited for it would hand that delay to the next group's
	// keyframe — the disposable layer holding up the one that cannot be dropped,
	// which is the priority design backwards.
	if subgroup == baseSubgroup && !backfilledGroup {
		defer track.order.Finish(group)
	}

	reassembler := track.reassemblerFor(group)

	// Counted once per group, by the stream that opens it. Every layer of a
	// group is the same group, so counting per stream would report a group
	// count that tracked the layer count instead.
	if subgroup == baseSubgroup {
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

		// Keyed on the publisher's emission index, not the object ID: each
		// subgroup numbers its objects from its own base, so an ID orders a
		// stream against itself and nothing else. See reorder.go.
		index := emissionIndex(obj.ObjectID, decoded.Properties)

		frame := bridge.MediaFrame{
			Kind:      track.kind,
			Handle:    track.handle,
			Timestamp: scaleTimestamp(decoded.Properties),
			// The first object emitted in a group opens it, and for video every
			// group opens on a keyframe: writeVideo rotates the group *on* the
			// keyframe, so the two cannot come apart.
			//
			// Emission index rather than Object ID, which is what this used to
			// read. The ID says nothing on its own — each layer numbers from its
			// own base (see layerObjectStride), so "object 0" exists once per
			// layer and only one of them is the keyframe, and which range the
			// base layer occupies has already changed once. The emission index
			// is defined as the position in the group's emission order, so index
			// zero is the object that opened the group whatever the ID layout,
			// and for a publisher that stamps no index it falls back to the ID —
			// which is right for the unlayered case that is the only one without
			// one. The subgroup test is then redundant and kept as a guard.
			KeyFrame:      subgroup == baseSubgroup && index == 0,
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
		// Two gates, and they answer different questions. The reassembler puts
		// this group's subgroups back into decode order; the group orderer holds
		// that order across the group boundary, where there is no reassembler to
		// do it. See reorder.go and grouporder.go.
		reassembler.Push(subgroup, index, func() {
			track.order.Push(group, index, func() { r.room.sink.SendMedia(&frame) })
		})
	}
}

// emissionIndex reads the object's position in its group's emission order.
//
// Both code points are accepted while the property moves off 0x8002: a build
// from before 0.6.3 stamps only the legacy one, and reading it is what lets a
// call span the move. See propEmissionIndexLegacy for why it moved and when
// this half goes away. The current code point wins when both are present,
// which is what a peer new enough to stamp both sends.
//
// Falls back to the Object ID for a publisher that stamps neither, which is
// right for the only kind there is: one subgroup per group, where the IDs
// count from zero without gaps and are the emission order. A layered publisher
// always stamps it — the layout it uses is what makes the ID unusable, so the
// fallback is not a safety net for layered video and must not be reached by
// one.
func emissionIndex(objectID uint64, props loc.Properties) uint64 {
	var legacy uint64
	var haveLegacy bool
	for _, kv := range props.Extras {
		switch kv.Type {
		case propEmissionIndex:
			return kv.IntVal
		case propEmissionIndexLegacy:
			legacy, haveLegacy = kv.IntVal, true
		}
	}
	if haveLegacy {
		return legacy
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
// Rebuilding it as it was asks for exactly the traffic the relay just refused,
// so it is not repeated indefinitely: after enough rebuilds inside one window
// this client gives up on the participant's video and comes back to it later.
// There is no smaller encoding to step down onto — the publisher sends one, and
// what degrades under pressure is the relay shedding its enhancement layer.
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

		// Rebuilt here rather than handed to the retry loop. That loop is
		// guarded by a flag it clears in a defer, after its last check for
		// anything missing — so a verdict landing in that window found a loop
		// that had already decided there was nothing to do, and was dropped.
		// Nothing else would have re-run reconcile: checkLag only fires from
		// audio streams, which by then do not exist, and a publisher who is
		// merely talking never republishes their catalog. That participant went
		// silent for the rest of the call.
		r.dropTrack(&r.audio)
		r.reapply()
		return
	}

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

	// A run of these means the link is not going to carry this participant's
	// video, and asking again immediately would offer back what was just
	// refused. Counted over a window rather than forever, so an hour-long call
	// is not judged on one bad minute.
	if r.rebuiltSince.IsZero() || time.Since(r.rebuiltSince) > videoGiveUpWindow {
		r.rebuiltSince, r.rebuilds = time.Now(), 0
	}
	r.rebuilds++
	givingUp := r.rebuilds >= videoGiveUpAfter
	backoff := ""
	if givingUp {
		r.videoOff = true
		r.rebuilds = 0
		heldFor := time.Duration(0)
		if !r.recoveredAt.IsZero() {
			heldFor = time.Since(r.recoveredAt)
		}
		r.recoveryWait = nextRecoveryWait(r.recoveryWait, heldFor, !r.recoveredAt.IsZero())
		backoff = r.recoveryWait.String()
	}
	r.mu.Unlock()

	fields := []any{"handle", track.handle, "code", code}
	if givingUp {
		fields = append(fields, "action", "audio only", "next_attempt_in", backoff)
	} else {
		fields = append(fields, "action", "rebuilding it unchanged")
	}
	r.log.Warn("the relay stopped forwarding a track: we are not keeping up", fields...)

	// Dropped before reconciling, and that is not a detail: the track name and
	// config are the same ones, so reconcile on its own would compare them
	// against the subscription the relay has just killed, conclude nothing had
	// changed, and leave the dead one in place. Nothing else would notice
	// either — missing() sees a track object and reports it present — so the
	// tile stayed blank for the rest of the call.
	//
	// Rebuilt as it was, not smaller. There is nothing smaller to ask for, and
	// there does not need to be: a fresh SUBSCRIBE starts at the live edge, and
	// on a link this tight the relay shortly sheds the enhancement layer, so
	// what comes back is half the frame rate of the same picture.
	if !givingUp {
		r.dropTrack(&r.video)
	}
	r.reapply()

	if givingUp {
		// Said out loud. VideoLevel exists so a participant this client gave up
		// on does not look like one who switched their camera off — and it only
		// reaches the roster when the roster is republished, which nothing here
		// used to do. The tile went blank while the roster still reported full
		// video, which is the pair of disagreeing signals VideoLevel was added
		// to prevent.
		r.room.publishParticipants()
		r.scheduleRecovery()
	} else {
		r.scheduleResubscribe()
	}
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

// nextRecoveryWait is how long to leave video off, given how long the last
// attempt at it lasted.
//
// It grows when video comes back and is given up on again sooner than the wait
// that preceded it: the link has not recovered, and the reduced state needs
// somewhere to settle rather than something to bounce off. Doubling to a
// ceiling is what gives it that.
//
// And it shrinks again, which it did not for a while. Nothing lowered it, so
// two bad minutes early in a call raised the wait to a couple of minutes and
// left it there — an hour of a perfect link later, one burst still cost a
// blank tile for two minutes. videoRecoveryMax's own reasoning is that a link
// which genuinely recovered must not be written off for the evening, and a
// wait that only ever grows writes it off. Video that held for longer than the
// wait it was given is the evidence that the link recovered, so that resets it.
func nextRecoveryWait(current, heldFor time.Duration, hasRecovered bool) time.Duration {
	if current <= 0 {
		return videoRecovery
	}
	if hasRecovered && heldFor < current {
		return min(current*2, videoRecoveryMax)
	}
	return videoRecovery
}

// recover turns video back on once the link has been quiet, after it was given
// up on.
//
// Nothing steps back up any more, because nothing stepped down: the answer to
// a relay giving up is the same subscription again, and the answer to that
// failing repeatedly is to stop asking for a while. This is the end of the
// while.
func (r *remote) recover() {
	if r.ctx.Err() != nil {
		return
	}

	r.mu.Lock()
	if r.closed || !r.videoOff {
		r.mu.Unlock()
		return
	}
	r.videoOff = false
	r.recoveredAt = time.Now()
	// Cleared so the next verdict on the rebuilt subscription is acted on.
	r.demotedFor = 0
	r.rebuilds, r.rebuiltSince = 0, time.Time{}
	r.mu.Unlock()

	r.log.Info("the link has been quiet; asking for video again")

	r.reapply()

	// The roster still says this participant has no video until it is told
	// otherwise; see the same call on the way down.
	r.room.publishParticipants()
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
	r.mu.Unlock()

	r.log.Warn("this participant has slipped behind the live edge; resubscribing",
		"lag_ms", int(lag), "limit_ms", int(maxLag/time.Millisecond))

	// Dropped rather than reconciled in place: reconcile leaves a track whose
	// config has not changed exactly where it is, which is the whole problem —
	// it is not the wrong track, it is the right track too far back.
	r.dropTrack(&r.video)
	r.dropTrack(&r.audio)

	r.reapply()
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
		OS:       r.os,
		HasVideo: r.catalogVideo,
		HasAudio: r.catalogAudio,
		// What we are actually taking, which HasVideo above deliberately does
		// not say — it reports what they publish. Without this a participant
		// this client gave up on looked exactly like one who switched their
		// camera off, while HasVideo insisted the camera was on: a blank tile
		// and two signals disagreeing about why.
		VideoLevel: videoStateOf(r.videoOff),
	}
}
