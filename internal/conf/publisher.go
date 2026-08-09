package conf

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/floatdrop/moq-go/pkg/moqt"
	"github.com/floatdrop/moq-go/pkg/moqt/loc"
	"github.com/floatdrop/moq-go/pkg/moqt/message"
	"github.com/floatdrop/moq-go/pkg/moqt/msf"
	"github.com/floatdrop/moq-go/pkg/moqt/session"
	"github.com/floatdrop/moq-go/pkg/moqt/wire"

	"tlmst/internal/bridge"
	"tlmst/internal/telemetry"
)

// ErrNoPublication reports a frame for a track that was never published.
var ErrNoPublication = errors.New("conf: no publication for track")

// ErrAwaitingKeyFrame reports a video frame dropped because no keyframe
// has arrived yet — a group must open on one.
var ErrAwaitingKeyFrame = errors.New("conf: waiting for first keyframe")

// SUBGROUP_DELIVERY_TIMEOUT (§8) on published video: how long after a group
// has been closed we keep trying to get the rest of it there.
//
// Without one, QUIC's reliability is applied to media that has no use for it.
// A group whose tail is still being retransmitted spends capacity on frames
// whose moment has passed, on the same connection as the frames whose moment
// has not — so retransmitting stale video is part of what makes the next video
// stale. The timer starts at Close, when the group is complete and every byte
// is already in the send buffer, so this bounds how long a *finished* group may
// keep occupying the link and nothing about how one is written.
//
// Two seconds because a group is one GOP: the next keyframe, which is where a
// subscriber recovers to regardless, is due two seconds after this one opened.
//
// Audio deliberately has none, and used to. The reasoning for the one it had
// was wrong — it claimed a late group would be discarded on arrival anyway,
// but the player trims on how much is *queued*, not on how late it is, so
// uniformly late audio is played late rather than dropped. What that timeout
// actually did was convert late audio into missing audio, silently: moq-go
// resets the stream from a detached goroutine with no error path back, so up
// to half a second of speech could be abandoned to every subscriber with
// nothing logged and no counter moving. And lateness now has a precise remedy
// — the subscriber notices the slip and rebuilds its subscription at the live
// edge — so the blunt one is not worth the holes it costs.
const videoSubgroupTimeout = 2 * time.Second

// publisher owns the local participant's three publications: the MSF
// catalog and the two LOC media tracks.
type publisher struct {
	// ctx is the room's, so a retry stops when the session does.
	ctx      context.Context
	log      *slog.Logger
	sess     *session.Session
	counters *telemetry.Registry
	nickname string
	// version is the build this participant is running, published in the
	// catalog so the room can see who is on what.
	version string
	ns      wire.TrackNamespace

	catalog *session.Publication
	video   *trackPublisher
	// videoLow is the smaller encoding of the same camera, published beside
	// the primary one so a subscriber with a thumbnail-sized tile can take it
	// instead. Always open; it carries objects only while the frontend is
	// feeding it.
	videoLow *trackPublisher
	audio    *trackPublisher

	// mu guards the declared configs and the catalog republish, which
	// races the two independent encoder-config messages arriving from
	// the frontend.
	mu             sync.Mutex
	videoConfig    *bridge.TrackConfig
	videoLowConfig *bridge.TrackConfig
	audioConfig    *bridge.TrackConfig
	// catalogGroup numbers successive catalog objects. Each catalog is a
	// standalone group so a subscriber's Joining FETCH lands on the
	// newest one.
	catalogGroup uint64
}

// newPublisher opens all three publications, emits the first catalog, and
// only then announces the namespace. The catalog starts out declaring no
// media tracks and is republished each time the frontend declares an
// encoder config.
//
// The order matters and is not merely tidy. PUBLISH_NAMESPACE is what makes
// this participant discoverable, and a peer watching the room reacts to it
// immediately by subscribing to the catalog track. Announcing first
// therefore advertises a participant whose tracks do not exist yet: the
// peer's SUBSCRIBE races the PUBLISH that would let the relay serve it, and
// on a fast loopback the SUBSCRIBE wins and hangs unanswered. Publishing
// first closes that window — by the time anyone can learn this namespace,
// every track is open and the first catalog object is already cached, which
// is also what lets a late joiner's Joining FETCH find something.
func newPublisher(
	ctx context.Context,
	log *slog.Logger,
	sess *session.Session,
	counters *telemetry.Registry,
	cfg Config,
) (*publisher, error) {
	ns := namespaceFor(cfg.Room, cfg.ID)
	p := &publisher{
		ctx:      ctx,
		log:      log,
		sess:     sess,
		counters: counters,
		nickname: cfg.Nickname,
		version:  cfg.Version,
		ns:       ns,
	}

	var err error
	if p.catalog, err = p.publish(ctx, msf.CatalogTrackName); err != nil {
		return nil, err
	}
	if p.video, err = p.newTrack(ctx, VideoTrack, videoSubgroupTimeout); err != nil {
		return nil, err
	}
	if p.videoLow, err = p.newTrack(ctx, VideoLowTrack, videoSubgroupTimeout); err != nil {
		return nil, err
	}
	if p.audio, err = p.newTrack(ctx, AudioTrack, 0); err != nil {
		return nil, err
	}

	if err := p.republishCatalog(); err != nil {
		return nil, err
	}

	// Everything is serveable; now become discoverable. The relay forwards
	// this as a NAMESPACE arrival to everyone watching the room prefix.
	if _, err := sess.PublishNamespace(ctx, &message.PublishNamespace{Namespace: ns}); err != nil {
		return nil, fmt.Errorf("conf: PUBLISH_NAMESPACE %v: %w", ns, err)
	}
	log.Info("announced namespace", "namespace", nsString(ns))

	return p, nil
}

// publish opens one PUBLISH request stream and starts its broker, which
// answers subscriber REQUEST_UPDATEs with the REQUEST_OK §10.9 mandates
// and serializes those replies against a later Done.
func (p *publisher) publish(ctx context.Context, name string) (*session.Publication, error) {
	pub, err := p.sess.Publish(ctx, &message.Publish{
		Namespace:  p.ns,
		Name:       []byte(name),
		TrackAlias: p.sess.AllocOutboundTrackAlias(),
	})
	if err != nil {
		return nil, fmt.Errorf("conf: PUBLISH %s: %w", name, err)
	}
	p.log.Info("publication open", "track", name, "alias", pub.TrackAlias())

	go func() {
		err := pub.Broker().Serve(ctx, func(m message.Message) bool {
			p.log.Debug("publish stream message", "track", name, "type", m.Type().String())
			return true
		})
		if err != nil && ctx.Err() == nil {
			p.log.Debug("publish stream closed", "track", name, "err", err)
		}
	}()
	return pub, nil
}

func (p *publisher) newTrack(
	ctx context.Context,
	name string,
	subgroupTimeout time.Duration, // zero disables it — see videoSubgroupTimeout
) (*trackPublisher, error) {
	pub, err := p.publish(ctx, name)
	if err != nil {
		return nil, err
	}
	return &trackPublisher{
		log:     p.log.With("track", name),
		pub:     pub,
		counter: p.counters.Track(telemetry.OutPrefix + name),
		timeouts: message.DeliveryTimeouts{
			Subgroup: subgroupTimeout,
		},
	}, nil
}

// declareConfig records an encoder configuration the frontend has just
// produced and republishes the catalog so subscribers can configure
// matching decoders.
func (p *publisher) declareConfig(cfg *bridge.TrackConfig) error {
	p.mu.Lock()
	switch cfg.Kind {
	case "video":
		p.videoConfig = cfg
	case KindVideoLow:
		p.videoLowConfig = cfg
	case "audio":
		p.audioConfig = cfg
	default:
		p.mu.Unlock()
		return fmt.Errorf("conf: unknown track kind %q", cfg.Kind)
	}
	p.mu.Unlock()

	// The same description the catalog carries in its initDataList, kept where
	// the objects are written so each group can open with it.
	description, err := decodeDescription(cfg.Description)
	if err != nil {
		return err
	}
	switch cfg.Kind {
	case "video":
		p.video.setConfig(description)
	case KindVideoLow:
		p.videoLow.setConfig(description)
	case "audio":
		p.audio.setConfig(description)
	}

	p.log.Info("local encoder configured",
		"kind", cfg.Kind, "codec", cfg.Codec,
		"width", cfg.Width, "height", cfg.Height,
		"sampleRate", cfg.SampleRate, "descriptionBytes", len(cfg.Description))
	return p.republishCatalog()
}

// undeclareConfig withdraws a local track and republishes the catalog
// without it, which is what tells subscribers to retire their decoders
// rather than sit on a frozen last frame.
//
// The publication itself stays open: turning the camera back on only has to
// declare the track again, not re-PUBLISH it.
func (p *publisher) undeclareConfig(kind string) error {
	p.mu.Lock()
	switch kind {
	case "video":
		if p.videoConfig == nil {
			p.mu.Unlock()
			return nil // already withdrawn
		}
		p.videoConfig = nil
	case KindVideoLow:
		if p.videoLowConfig == nil {
			p.mu.Unlock()
			return nil
		}
		p.videoLowConfig = nil
	case "audio":
		if p.audioConfig == nil {
			p.mu.Unlock()
			return nil
		}
		p.audioConfig = nil
	default:
		p.mu.Unlock()
		return fmt.Errorf("conf: unknown track kind %q", kind)
	}
	p.mu.Unlock()

	p.log.Info("local track withdrawn", "kind", kind)
	return p.republishCatalog()
}

// republishCatalog emits the current catalog as a fresh object in a new
// group. §5 says a catalog object should be published only when track
// availability changes, which is exactly when this is called.
// republishCatalog emits the catalog, retrying a write that fails.
//
// This is the one message that makes a participant decodable: until it lands,
// peers know a namespace exists and nothing about what it carries. A failure
// used to be returned and forgotten, leaving the on-wire catalog stale — or,
// on the first declaration, absent — with nothing to try again.
//
// Retried here rather than left to the caller because the callers are the
// frontend declaring an encoder and the reconnect replaying one, and neither
// is in a position to know that the right response is to wait 500ms.
func (p *publisher) republishCatalog() error {
	var err error
	for attempt := 1; attempt <= catalogRetryLimit; attempt++ {
		if err = p.writeCatalog(); err == nil {
			if attempt > 1 {
				p.log.Info("catalog published after a retry", "attempt", attempt)
			}
			return nil
		}
		p.log.Warn("catalog publish failed", "attempt", attempt, "err", err)

		select {
		case <-p.ctx.Done():
			return err
		case <-time.After(time.Duration(attempt) * catalogRetryDelay):
		}
	}
	return err
}

func (p *publisher) writeCatalog() error {
	p.mu.Lock()
	cat, err := buildCatalog(p.nickname, p.version, p.videoConfig, p.videoLowConfig, p.audioConfig)
	group := p.catalogGroup
	p.catalogGroup++
	p.mu.Unlock()
	if err != nil {
		return err
	}

	payload, err := encodeCatalog(cat)
	if err != nil {
		return err
	}

	sg, err := p.catalog.OpenSubgroup(message.SubgroupHeader{
		SubgroupIDMode: message.SubgroupIDImplicitZero,
		GroupID:        group,
	})
	if err != nil {
		return fmt.Errorf("conf: open catalog subgroup: %w", err)
	}
	if err := sg.WriteObjectAt(0, &message.SubgroupObject{Payload: payload}); err != nil {
		sg.Cancel(moqt.StreamResetInternalError)
		return fmt.Errorf("conf: write catalog object: %w", err)
	}
	if err := sg.Close(); err != nil {
		return fmt.Errorf("conf: close catalog subgroup: %w", err)
	}
	p.counters.Track(telemetry.OutPrefix + msf.CatalogTrackName).AddObject(len(payload))
	p.counters.Track(telemetry.OutPrefix + msf.CatalogTrackName).AddGroup()
	p.log.Info("catalog published", "group", group, "bytes", len(payload),
		"tracks", len(cat.Tracks))
	return nil
}

// writeFrame packages one encoded frame from the frontend as a LOC object
// and writes it to the matching publication.
func (p *publisher) writeFrame(f *bridge.MediaFrame) error {
	switch f.Handle {
	case bridge.HandleLocalVideo:
		if p.video == nil {
			return ErrNoPublication
		}
		return p.video.writeVideo(f)
	case bridge.HandleLocalVideoLow:
		if p.videoLow == nil {
			return ErrNoPublication
		}
		return p.videoLow.writeVideo(f)
	case bridge.HandleLocalAudio:
		if p.audio == nil {
			return ErrNoPublication
		}
		return p.audio.writeAudio(f)
	default:
		return fmt.Errorf("%w: handle %d", ErrNoPublication, f.Handle)
	}
}

// close ends all three publications with PUBLISH_DONE. It reports the
// §11.3 end-of-broadcast to subscribers before the session goes away.
func (p *publisher) close() {
	// The §11.3 terminator catalog tells subscribers the broadcast ended
	// deliberately, rather than leaving them to infer it from the
	// session closing.
	if p.catalog != nil {
		p.mu.Lock()
		group := p.catalogGroup
		p.catalogGroup++
		p.mu.Unlock()
		if payload, err := encodeCatalog(msf.EndBroadcastTerminate(time.Time{})); err == nil {
			if sg, err := p.catalog.OpenSubgroup(message.SubgroupHeader{
				SubgroupIDMode: message.SubgroupIDImplicitZero,
				GroupID:        group,
			}); err == nil {
				if err := sg.WriteObjectAt(0, &message.SubgroupObject{Payload: payload}); err != nil {
					sg.Cancel(moqt.StreamResetInternalError)
				} else {
					_ = sg.Close()
				}
			}
		}
	}

	for _, t := range []*trackPublisher{p.video, p.videoLow, p.audio} {
		if t != nil {
			t.close()
		}
	}
	if p.catalog != nil {
		_ = p.catalog.Done(moqt.PublishDoneTrackEnded, "")
	}
}

// trackPublisher writes one media track's objects, holding the subgroup
// stream open across a group and tracking object numbering within it.
type trackPublisher struct {
	log     *slog.Logger
	pub     *session.Publication
	counter *telemetry.TrackCounter
	// timeouts is applied to every subgroup this track opens — see the
	// delivery-timeout constants.
	timeouts message.DeliveryTimeouts

	// mu serializes writes: the bridge read loop is the only caller
	// today, but a group boundary spans two operations (close the old
	// subgroup, open the new one) that must not interleave.
	mu sync.Mutex
	// subgroups holds one open stream per temporal layer of the current group,
	// indexed by layer. Video with temporal layers writes each frame to the
	// subgroup matching its layer, so a subscriber can decline the upper ones
	// (§5.1.3) and a relay can shed them, without either touching the base.
	// Audio and unlayered video use index 0 alone, which is the shape every
	// track had before layers existed.
	//
	// Opened lazily. An encoder that never emits a layer costs no stream, and a
	// subgroup opened eagerly and left empty is one the subscriber must still
	// wait on before it can release anything ordered after it.
	subgroups [bridge.MaxTemporalLayer + 1]*session.OutgoingSubgroupStream
	group     uint64
	// object numbers the group's objects across every subgroup in it, in
	// emission order. That is what lets a subscriber reassemble decode order
	// from streams that arrive concurrently — see reorder.go, whose ordering is
	// only as correct as this counter's continuity across layers.
	object uint64
	// started distinguishes "no group yet" from "in group 0".
	started bool
	// objectsInGroup counts objects written to the current group, for
	// the audio cadence.
	objectsInGroup int
	// config is the codec description to stamp on the first object of each
	// group. Held here because the frames do not carry one: WebCodecs emits a
	// description on the encoder's first output and never again, so without a
	// copy only the very first group of a track would have one — and it is
	// every group *after* the first that a mid-stream subscriber lands on.
	config []byte
}

// setConfig supplies the codec description to stamp on each group. Called
// from declareConfig, which is also what a reconnect replays, so a new
// session's groups carry it without the encoder having to emit it again.
func (t *trackPublisher) setConfig(config []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.config = config
}

// writeVideo appends a video frame. A keyframe closes the current group
// and opens the next, so every group begins at a decodable point.
func (t *trackPublisher) writeVideo(f *bridge.MediaFrame) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if f.KeyFrame || !t.started {
		if !f.KeyFrame {
			// Nothing decodable has arrived yet: a group that opened on
			// a delta frame would be useless to every subscriber.
			return ErrAwaitingKeyFrame
		}
		if err := t.rotateGroup(); err != nil {
			return err
		}
	}
	return t.writeObject(f)
}

// writeAudio appends an audio frame, rotating the group every
// audioGroupObjects frames.
func (t *trackPublisher) writeAudio(f *bridge.MediaFrame) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.started || t.objectsInGroup >= audioGroupObjects {
		if err := t.rotateGroup(); err != nil {
			return err
		}
	}
	return t.writeObject(f)
}

// rotateGroup closes every open subgroup and starts the next group. The
// caller holds mu.
//
// The base layer's stream is opened here rather than lazily: every group opens
// on it (video's keyframe, audio's first object), so deferring it would buy
// nothing and would leave the group with no stream to report a failure on.
func (t *trackPublisher) rotateGroup() error {
	if t.started {
		t.closeSubgroups()
		t.group++
	}

	t.object = 0
	t.objectsInGroup = 0
	t.started = true
	t.counter.AddGroup()
	_, err := t.openSubgroup(0)
	return err
}

// closeSubgroups ends every open stream of the current group. The caller holds
// mu. A close that fails is logged and dropped: the group is over either way,
// and the next one opens its own streams.
func (t *trackPublisher) closeSubgroups() {
	for layer, sg := range t.subgroups {
		if sg == nil {
			continue
		}
		if err := sg.Close(); err != nil {
			t.log.Debug("close subgroup failed",
				"group", t.group, "layer", layer, "err", err)
		}
		t.subgroups[layer] = nil
	}
}

// openSubgroup opens this group's stream for one temporal layer, or returns the
// one already open. The caller holds mu.
//
// The Subgroup ID is the layer, which is the whole mechanism: §5.1.3 range
// filters and §8 per-subgroup delivery timeouts both address a subgroup by ID,
// so numbering them by layer is what lets a subscriber decline the upper layers
// and a relay shed them. Spelled out explicitly rather than left implicit —
// implicit-zero can only ever name subgroup 0.
func (t *trackPublisher) openSubgroup(layer uint8) (*session.OutgoingSubgroupStream, error) {
	if sg := t.subgroups[layer]; sg != nil {
		return sg, nil
	}
	sg, err := t.pub.OpenSubgroup(message.SubgroupHeader{
		Properties:     true, // LOC metadata rides in Object Properties
		SubgroupIDMode: message.SubgroupIDExplicit,
		SubgroupID:     uint64(layer),
		GroupID:        t.group,
	})
	if err != nil {
		return nil, fmt.Errorf("conf: open subgroup group=%d layer=%d: %w",
			t.group, layer, err)
	}
	// Returns a copy rather than configuring in place, so the timeout only
	// applies if the result is what gets kept.
	t.subgroups[layer] = sg.WithDeliveryTimeouts(t.timeouts)
	return t.subgroups[layer], nil
}

// writeObject LOC-packages f and writes it as the next object in the open
// subgroup. The caller holds mu.
func (t *trackPublisher) writeObject(f *bridge.MediaFrame) error {
	props := loc.Properties{
		Timestamp:    f.Timestamp,
		HasTimestamp: true,
		Timescale:    timescaleMicros,
		HasTimescale: true,
	}
	// The speaking indicator rides on every audio object: LOC §2.3.3.2 is
	// exactly the right place for it, so a subscriber gets it without any
	// side channel and a relay can read it without decoding audio.
	if f.HasAudioLevel && f.Kind == bridge.KindAudio {
		props.AudioLevel = f.AudioLevel
		props.HasAudioLevel = true
	}
	// The codec config goes on the first object of every group, so a
	// subscriber can configure a decoder from the first object it sees
	// without waiting for the catalog to come round again.
	//
	// Which it now does. This used to attach whatever config the frame
	// happened to carry, and a frame carries one only on the encoder's first
	// output — so the claim held for object 0 of group 0 and nothing else. A
	// subscriber joining a call in progress, which is the only kind of
	// subscriber this is for, landed on a group with no config on it at all.
	config := f.Config
	if len(config) > 0 {
		t.config = config
	} else if t.object == 0 {
		config = t.config
	}
	if len(config) > 0 {
		switch f.Kind {
		case bridge.KindVideo:
			props.VideoConfig = config
		case bridge.KindAudio:
			props.AudioConfig = config
		}
	}

	obj := loc.Object{Properties: props, Payload: f.Payload}
	encodedProps, payload := obj.Encode()

	layer := f.TemporalLayer
	if layer > bridge.MaxTemporalLayer {
		// The frontend encodes the layer in two bits, so this cannot arrive
		// over the bridge; it would take a caller constructing a frame by hand.
		// Clamped to the base rather than rejected: a frame on the wrong layer
		// is a picture that stutters, and dropping it is a picture with a hole.
		layer = 0
	}
	sg, err := t.openSubgroup(layer)
	if err != nil {
		return err
	}

	if err := sg.WriteObjectAt(t.object, &message.SubgroupObject{
		Properties: encodedProps,
		Payload:    payload,
	}); err != nil {
		// Only this layer's stream is torn down. The group keeps its object
		// numbering and its other layers: a failure writing an enhancement
		// frame should cost that frame, not the base layer the subscriber is
		// actually watching. Restarting the whole group is reserved for the
		// base layer, which nothing above it can survive without.
		sg.Cancel(moqt.StreamResetInternalError)
		t.subgroups[layer] = nil
		if layer == 0 {
			t.started = false
		}
		return fmt.Errorf("conf: write object group=%d object=%d layer=%d: %w",
			t.group, t.object, layer, err)
	}
	t.object++
	t.objectsInGroup++
	t.counter.AddObject(len(payload))
	return nil
}

func (t *trackPublisher) close() {
	t.mu.Lock()
	t.closeSubgroups()
	t.mu.Unlock()
	_ = t.pub.Done(moqt.PublishDoneTrackEnded, "")
}
