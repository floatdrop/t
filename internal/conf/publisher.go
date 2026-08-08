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

// SUBGROUP_DELIVERY_TIMEOUT (§8) per track: how long after a group has been
// closed we keep trying to get the rest of it there.
//
// Without one, QUIC's reliability is applied to media that has no use for it.
// A group whose tail is still being retransmitted is spending capacity on
// frames whose moment has passed, on the same connection as the frames that
// have not — so the retransmission of stale media is what makes the next media
// stale. The timer starts at Close, when the group is complete and every byte
// of it is already in the send buffer, so this bounds how long a *finished*
// group may keep occupying the link and nothing else.
//
// Both values are the point past which a subscriber would not use the data
// anyway. Audio: the player trims its buffer back to 60 ms once more than
// 250 ms has queued, so a 500 ms group still undelivered a second after it
// closed would be dropped on arrival. Video: a group is one GOP, and the next
// keyframe — which is where a subscriber recovers to regardless — is due two
// seconds after this one opened.
//
// The matching OBJECT_DELIVERY_TIMEOUT is deliberately left off. It reads as
// "how stale may an object be" but moq-go implements it as elapsed time since
// the *first* object of the subgroup, which makes it a cap on how long a group
// may stay open: any value below the keyframe interval would reset every video
// group on a healthy link. The keyframe interval is the frontend's, and this
// side does not know it.
const (
	audioSubgroupTimeout = 1 * time.Second
	videoSubgroupTimeout = 2 * time.Second
)

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
	if p.audio, err = p.newTrack(ctx, AudioTrack, audioSubgroupTimeout); err != nil {
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
	subgroupTimeout time.Duration,
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
	mu       sync.Mutex
	subgroup *session.OutgoingSubgroupStream
	group    uint64
	object   uint64
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

// rotateGroup closes any open subgroup and opens the next group's. The
// caller holds mu.
func (t *trackPublisher) rotateGroup() error {
	if t.subgroup != nil {
		if err := t.subgroup.Close(); err != nil {
			t.log.Debug("close subgroup failed", "group", t.group, "err", err)
		}
		t.subgroup = nil
		t.group++
	}

	sg, err := t.pub.OpenSubgroup(message.SubgroupHeader{
		Properties:     true, // LOC metadata rides in Object Properties
		SubgroupIDMode: message.SubgroupIDImplicitZero,
		GroupID:        t.group,
	})
	if err != nil {
		return fmt.Errorf("conf: open subgroup group=%d: %w", t.group, err)
	}
	// Returns a copy rather than configuring in place, so the timeout only
	// applies if the result is what gets kept.
	t.subgroup = sg.WithDeliveryTimeouts(t.timeouts)
	t.object = 0
	t.objectsInGroup = 0
	t.started = true
	t.counter.AddGroup()
	return nil
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

	if err := t.subgroup.WriteObjectAt(t.object, &message.SubgroupObject{
		Properties: encodedProps,
		Payload:    payload,
	}); err != nil {
		t.subgroup.Cancel(moqt.StreamResetInternalError)
		t.subgroup = nil
		t.started = false
		return fmt.Errorf("conf: write object group=%d object=%d: %w", t.group, t.object, err)
	}
	t.object++
	t.objectsInGroup++
	t.counter.AddObject(len(payload))
	return nil
}

func (t *trackPublisher) close() {
	t.mu.Lock()
	if t.subgroup != nil {
		_ = t.subgroup.Close()
		t.subgroup = nil
	}
	t.mu.Unlock()
	_ = t.pub.Done(moqt.PublishDoneTrackEnded, "")
}
