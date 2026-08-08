package conf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
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
const (
	catalogPriority = 10
	audioPriority   = 20
	videoPriority   = 60
)

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
	video        *remoteTrack
	audio        *remoteTrack
	// closed stops late catalog updates from resurrecting subscriptions
	// after the participant has left.
	closed bool
}

// remoteTrack is one subscribed media track of a remote participant.
type remoteTrack struct {
	handle uint32
	kind   uint8
	config bridge.TrackConfig
	sub    *session.Subscription
	label  string
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
	wantsVideo := r.room.wantsVideo(r.id)

	r.mu.Lock()
	defer r.mu.Unlock()
	return (r.wantVideo != nil && r.video == nil && wantsVideo) ||
		(r.wantAudio != nil && r.audio == nil)
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
		r.mu.Unlock()
		if closed || !r.missing() {
			return
		}

		// Through reconcile under the same lock a catalog would take, so a
		// retry and an arriving catalog cannot both subscribe the same track.
		r.applying.Lock()
		r.reconcile(video, audio)
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
func (r *remote) reconcile(video, audio *bridge.TrackConfig) {
	r.remember(video, audio)
	// What they publish and what we can use are different questions. The
	// answer to the second is the frontend's — it knows which tiles are on
	// screen — and it only ever subtracts: a track the catalog does not
	// declare cannot be wanted into existence.
	wanted := video
	if wanted != nil && !r.room.wantsVideo(r.id) {
		wanted = nil
	}
	r.syncTrack(&r.video, VideoTrack, bridge.KindVideo, wanted)
	r.syncTrack(&r.audio, AudioTrack, bridge.KindAudio, audio)
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
func (r *remote) syncTrack(slot **remoteTrack, name string, kind uint8, want *bridge.TrackConfig) {
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

	case current != nil && current.config == *want:
		// Already subscribed with this exact config: nothing to do.
		return

	case current != nil:
		// The publisher changed codec or resolution. The frontend keys
		// its decoder off the handle, so tear the old one down and
		// announce a fresh handle rather than reconfiguring in place.
		r.log.Info("track config changed, resubscribing", "track", name)
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

func (r *remote) subscribeTrack(slot **remoteTrack, name string, kind uint8, cfg *bridge.TrackConfig) error {
	priority := uint8(videoPriority)
	if kind == bridge.KindAudio {
		priority = audioPriority
	}
	sub, err := r.room.sess.Subscribe(r.ctx, &message.Subscribe{
		Namespace: r.ns,
		Name:      []byte(name),
		Parameters: message.Parameters{
			message.LargestObjectFilter(),
			message.SubscriberPriorityParam(priority),
		},
	})
	if err != nil {
		return fmt.Errorf("conf: SUBSCRIBE %s %s: %w", r.id, name, err)
	}

	handle := r.room.nextHandle()
	label := telemetry.InPrefix + r.id + "/" + name
	track := &remoteTrack{
		handle: handle,
		kind:   kind,
		config: *cfg,
		sub:    sub,
		label:  label,
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
	return nil
}

// dropTrack closes one media subscription and tells the frontend to
// retire its decoder.
func (r *remote) dropTrack(slot **remoteTrack) {
	r.mu.Lock()
	track := *slot
	*slot = nil
	r.mu.Unlock()
	if track == nil {
		return
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
	counter.AddGroup()
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
			// every group opens on a keyframe (see the publisher).
			KeyFrame: obj.ObjectID == 0,
			Payload:  decoded.Payload,
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
		r.room.sink.SendMedia(&frame)
	}
}

// reportMediaEnd says why one media stream stopped.
//
// A group ending is the ordinary case and stays at Debug — every group ends,
// once per GOP. What this exists to separate out is the relay resetting the
// stream because this subscriber could not keep up, which is the only inbound
// capacity signal anybody sends us and was previously logged, at Debug,
// identically to a group finishing normally.
//
// It is reported and not acted on. Re-subscribing immediately would ask the
// relay for exactly the traffic it just decided we cannot carry, and would do
// it in a loop; choosing what to subscribe to instead is admission control,
// which does not exist yet. So the tile stays frozen — as it already did — but
// the log now says why, which is the difference between a mystery and a
// measurement.
func (r *remote) reportMediaEnd(track *remoteTrack, err error) {
	if r.ctx.Err() != nil {
		return // we tore this down ourselves
	}
	if code, ok := streamReset(err); ok {
		if overloadReset(code) {
			r.log.Warn("the relay stopped forwarding a track: we are not keeping up",
				"handle", track.handle, "code", resetName(code),
				"consequence", "this track is not being delivered until its subscription is rebuilt")
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
	}
}
