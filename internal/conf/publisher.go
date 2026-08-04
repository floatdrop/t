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

// publisher owns the local participant's three publications: the MSF
// catalog and the two LOC media tracks.
type publisher struct {
	log      *slog.Logger
	sess     *session.Session
	counters *telemetry.Registry
	nickname string
	ns       wire.TrackNamespace

	catalog *session.Publication
	video   *trackPublisher
	audio   *trackPublisher

	// mu guards the declared configs and the catalog republish, which
	// races the two independent encoder-config messages arriving from
	// the frontend.
	mu          sync.Mutex
	videoConfig *bridge.TrackConfig
	audioConfig *bridge.TrackConfig
	// catalogGroup numbers successive catalog objects. Each catalog is a
	// standalone group so a subscriber's Joining FETCH lands on the
	// newest one.
	catalogGroup uint64
}

// newPublisher announces the local namespace and opens all three
// publications. The catalog is published empty and republished each time
// the frontend declares an encoder config.
func newPublisher(
	ctx context.Context,
	log *slog.Logger,
	sess *session.Session,
	counters *telemetry.Registry,
	cfg Config,
) (*publisher, error) {
	ns := namespaceFor(cfg.Room, cfg.ID)

	// PUBLISH_NAMESPACE is what makes this participant discoverable: the
	// relay forwards it as a NAMESPACE arrival to everyone watching the
	// room prefix.
	if _, err := sess.PublishNamespace(ctx, &message.PublishNamespace{Namespace: ns}); err != nil {
		return nil, fmt.Errorf("conf: PUBLISH_NAMESPACE %v: %w", ns, err)
	}
	log.Info("announced namespace", "namespace", nsString(ns))

	p := &publisher{
		log:      log,
		sess:     sess,
		counters: counters,
		nickname: cfg.Nickname,
		ns:       ns,
	}

	var err error
	if p.catalog, err = p.publish(ctx, msf.CatalogTrackName); err != nil {
		return nil, err
	}
	if p.video, err = p.newTrack(ctx, VideoTrack); err != nil {
		return nil, err
	}
	if p.audio, err = p.newTrack(ctx, AudioTrack); err != nil {
		return nil, err
	}

	if err := p.republishCatalog(); err != nil {
		return nil, err
	}
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

func (p *publisher) newTrack(ctx context.Context, name string) (*trackPublisher, error) {
	pub, err := p.publish(ctx, name)
	if err != nil {
		return nil, err
	}
	return &trackPublisher{
		log:     p.log.With("track", name),
		pub:     pub,
		counter: p.counters.Track(telemetry.OutPrefix + name),
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
	case "audio":
		p.audioConfig = cfg
	default:
		p.mu.Unlock()
		return fmt.Errorf("conf: unknown track kind %q", cfg.Kind)
	}
	p.mu.Unlock()

	p.log.Info("local encoder configured",
		"kind", cfg.Kind, "codec", cfg.Codec,
		"width", cfg.Width, "height", cfg.Height,
		"sampleRate", cfg.SampleRate, "descriptionBytes", len(cfg.Description))
	return p.republishCatalog()
}

// republishCatalog emits the current catalog as a fresh object in a new
// group. §5 says a catalog object should be published only when track
// availability changes, which is exactly when this is called.
func (p *publisher) republishCatalog() error {
	p.mu.Lock()
	cat, err := buildCatalog(p.nickname, p.videoConfig, p.audioConfig)
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

	for _, t := range []*trackPublisher{p.video, p.audio} {
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
	t.subgroup = sg
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
	if len(f.Config) > 0 {
		switch f.Kind {
		case bridge.KindVideo:
			props.VideoConfig = f.Config
		case bridge.KindAudio:
			props.AudioConfig = f.Config
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
