package conf

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/floatdrop/moq-go/pkg/moqt/session"
	"github.com/floatdrop/moq-go/pkg/moqt/session/quicconn"
	"github.com/floatdrop/moq-go/pkg/relay"

	"t/internal/bridge"
	"t/internal/telemetry"
)

// recorder is a conf.Sink that captures everything a Room delivers, so a
// test can assert on the frames and control messages the frontend would
// have seen.
type recorder struct {
	mu sync.Mutex

	frames []bridge.MediaFrame
	// arrived[i] is when frames[i] was delivered, for measuring how long a
	// reordering buffer would have had to hold one frame to let another
	// overtake it. Wall time rather than the media timestamp: what a buffer
	// costs is the waiting, not the span it covers.
	arrived []time.Time
	tracks  []bridge.RemoteTrack
	// gone are the tracks the backend has told the frontend to retire.
	gone  []bridge.RemoteTrackID
	peers []bridge.Participant
	errs  []string
	// objects counts frames per handle, and bytes their payloads.
	objects map[uint32]int
	bytes   map[uint32]int
}

func newRecorder() *recorder {
	return &recorder{objects: map[uint32]int{}, bytes: map[uint32]int{}}
}

func (r *recorder) SendMedia(f *bridge.MediaFrame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Copy the payload: it aliases the read buffer of the subgroup stream.
	copied := *f
	copied.Payload = append([]byte(nil), f.Payload...)
	r.frames = append(r.frames, copied)
	r.arrived = append(r.arrived, time.Now())
	r.objects[f.Handle]++
	r.bytes[f.Handle] += len(f.Payload)
}

func (r *recorder) SendControl(msg *bridge.ServerMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch msg.Type {
	case bridge.MsgRemoteTrack:
		r.tracks = append(r.tracks, *msg.Track)
	case bridge.MsgTrackGone:
		// Recorded alongside rather than removed from tracks, which stays
		// append-only: tests that count subscriptions are counting how many
		// were announced, and a retirement is its own event.
		r.gone = append(r.gone, *msg.TrackGone)
	case bridge.MsgParticipants:
		r.peers = msg.Participants
	case bridge.MsgError:
		r.errs = append(r.errs, msg.Error)
	}
}

// timeline returns the delivered frames with their arrival times, in delivery
// order.
func (r *recorder) timeline() ([]bridge.MediaFrame, []time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bridge.MediaFrame(nil), r.frames...),
		append([]time.Time(nil), r.arrived...)
}

func (r *recorder) snapshot() ([]bridge.MediaFrame, []bridge.RemoteTrack, []bridge.Participant, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bridge.MediaFrame(nil), r.frames...),
		append([]bridge.RemoteTrack(nil), r.tracks...),
		append([]bridge.Participant(nil), r.peers...),
		append([]string(nil), r.errs...)
}

// countsFor returns the frame and byte counts for one track kind, matched
// through the announced handles.
func (r *recorder) countsFor(kind string) (frames, bytes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tracks {
		if t.Config.Kind != kind {
			continue
		}
		frames += r.objects[t.Handle]
		bytes += r.bytes[t.Handle]
	}
	return frames, bytes
}

// testRelay is a moq-go relay on a loopback QUIC listener that can be taken
// down and brought back on the same address, which is what makes relay
// failure testable.
type testRelay struct {
	t    *testing.T
	cert tls.Certificate
	addr string
	// goawayGrace is how long the relay waits after GOAWAY before closing
	// sessions itself. Zero — the default — makes Stop close them at once,
	// which is the abrupt-shutdown case; a non-zero value is what makes the
	// GOAWAY grace period observable.
	goawayGrace time.Duration

	// mu guards running and addr. Tests stop the relay from a goroutine —
	// draining is what they are observing — while t.Cleanup may stop it too.
	// sessionOptions are handed to every session the relay accepts, which is
	// how a test can stand up a relay that refuses §5.1.3 Range Filters — the
	// shape a deployed one turned out to have.
	sessionOptions []session.Option

	mu sync.Mutex
	// running holds what Stop tears down; nil while stopped.
	running *relayInstance
}

type relayInstance struct {
	udp    *net.UDPConn
	ql     *quic.Listener
	server *relay.Relay
	cancel context.CancelFunc
	done   chan struct{}
}

// startRelay brings up a relay on an ephemeral loopback port. Hermetic: its
// own port and a self-signed certificate, torn down with the test.
func startRelay(t *testing.T) *testRelay {
	t.Helper()
	return startRelayWith(t)
}

// startRelayWith is startRelay with session options, for a relay whose setup
// parameters are what the test is about.
func startRelayWith(t *testing.T, opts ...session.Option) *testRelay {
	t.Helper()
	r := &testRelay{t: t, cert: selfSignedCert(t), sessionOptions: opts}
	r.Start()
	t.Cleanup(r.Stop)
	return r
}

// Addr is the host:port to dial, stable across a Stop/Start cycle.
func (r *testRelay) Addr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addr
}

// Start listens and serves. After the first call it reuses the same port, so
// a client's stored relay address stays valid across a restart.
func (r *testRelay) Start() {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running != nil {
		return
	}

	// An explicit port on the second and later calls: the whole point is
	// for a reconnecting client to find the relay where it left it.
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	if r.addr != "" {
		_, port, err := net.SplitHostPort(r.addr)
		if err != nil {
			r.t.Fatalf("split relay addr %q: %v", r.addr, err)
		}
		p, err := net.LookupPort("udp", port)
		if err != nil {
			r.t.Fatalf("parse relay port %q: %v", port, err)
		}
		addr.Port = p
	}

	udp, err := net.ListenUDP("udp", addr)
	if err != nil {
		r.t.Fatalf("listen udp: %v", err)
	}
	ql, err := quic.Listen(udp, &tls.Config{
		Certificates: []tls.Certificate{r.cert},
		NextProtos:   []string{alpnDraft19},
	}, &quic.Config{
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true,
	})
	if err != nil {
		r.t.Fatalf("quic listen: %v", err)
	}
	r.addr = udp.LocalAddr().String()

	instance := relayInstance{udp: udp, ql: ql, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	instance.cancel = cancel
	instance.server = relay.New(quicconn.NewListener(ql), relay.Config{
		Logger:         testLogger(r.t).With("component", "relay"),
		GoawayTimeout:  r.goawayGrace,
		SessionOptions: r.sessionOptions,
	})
	go func() {
		defer close(instance.done)
		_ = instance.server.Start(ctx)
	}()
	r.running = &instance
}

// Stop shuts the relay down the way a redeploy does: it closes the sessions
// it is serving, so clients learn at once. Idempotent.
func (r *testRelay) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running == nil {
		return
	}
	// Relay.Stop closes the listener and then every session. Cancelling the
	// context passed to Start only unblocks the accept loop, which is the
	// silent case — see Kill.
	_ = r.running.server.Stop(context.Background())
	r.teardown()
}

// Kill makes the relay vanish without telling anyone, the way a crash or a
// network partition does. Clients can only notice by timeout.
func (r *testRelay) Kill() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running == nil {
		return
	}
	r.teardown()
}

// teardown releases the running instance. The caller holds mu.
func (r *testRelay) teardown() {
	r.running.cancel()
	_ = r.running.ql.Close()
	_ = r.running.udp.Close()
	<-r.running.done
	r.running = nil
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "t-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// joinRoom brings up one participant against the relay.
//
// The dial is retried a few times because quic-go abandons a handshake after
// five seconds of quiet, and a loaded machine — the race detector, ten tests
// each running a relay and two participants — can stall one past that. The
// attempts are bounded, so a genuinely broken dial still fails the test
// quickly rather than being papered over.
func joinRoom(t *testing.T, addr, room, nickname string) (*Room, *recorder) {
	t.Helper()
	return joinRoomWithKeyFrameHook(t, addr, room, nickname, nil)
}

// joinRoomWithKeyFrameHook is joinRoom for a participant whose response to a
// subscriber's NEW_GROUP_REQUEST is what the test is about.
func joinRoomWithKeyFrameHook(
	t *testing.T, addr, room, nickname string, onKeyFrame func(),
) (*Room, *recorder) {
	t.Helper()
	rec := newRecorder()

	const attempts = 3
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		var r *Room
		r, err = Join(t.Context(), testLogger(t), rec, telemetry.NewRegistry(), Config{
			Relay:             addr,
			Room:              room,
			Nickname:          nickname,
			Insecure:          true,
			OnKeyFrameRequest: onKeyFrame,
		})
		if err == nil {
			t.Cleanup(r.Close)
			return r, rec
		}
		t.Logf("join as %s failed (attempt %d/%d): %v", nickname, attempt, attempts, err)
	}
	t.Fatalf("join as %s: %v", nickname, err)
	return nil, nil
}

// declareBothTracks gives a participant the audio and video configs a
// subscriber needs before it will subscribe to anything.
func declareBothTracks(t *testing.T, r *Room) {
	t.Helper()
	if err := r.DeclareTrack(&bridge.TrackConfig{
		Kind: "audio", Codec: "opus", SampleRate: 48000, Channels: 1,
	}); err != nil {
		t.Fatalf("declare audio: %v", err)
	}
	if err := r.DeclareTrack(&bridge.TrackConfig{
		Kind: "video", Codec: "avc1.42e01f", Width: 640, Height: 360,
	}); err != nil {
		t.Fatalf("declare video: %v", err)
	}
}

// waitFor polls until cond holds or the deadline passes. Discovery and
// media delivery are asynchronous, so tests wait on outcomes rather than
// sleeping a fixed amount.
// subscribeWait is how long any test waits for a subscription to come up
// before calling the setup broken: discovery, a catalog, and the SUBSCRIBE
// round trip.
//
// One budget for all of them, and a generous one, because it costs nothing.
// waitFor polls every 10 ms and returns the moment the condition holds, so this
// only decides how long an already-broken run takes to fail — and it guards
// setup rather than an assertion: no test measures how long subscribing took,
// they all start measuring afterwards.
//
// It had drifted to 10, 15 and 20 seconds across the suite, each chosen against
// whatever the machine was doing that day, and the short ones flaked whenever
// the machine was busy — a CI runner with the race detector on, or a developer
// with two clients of this very app encoding video beside it. Both were
// diagnosed as regressions before being recognised as the clock running out on
// setup.
const subscribeWait = 30 * time.Second

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// videoFrame builds a frame the publisher will accept as video.
func videoFrame(tsMicros uint64, key bool, size int) *bridge.MediaFrame {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	return &bridge.MediaFrame{
		Kind:      bridge.KindVideo,
		Handle:    bridge.HandleLocalVideo,
		Timestamp: tsMicros,
		KeyFrame:  key,
		Payload:   payload,
	}
}

// layeredVideoFrame is videoFrame on a nominated temporal layer, which is what
// decides the subgroup the publisher writes it to.
func layeredVideoFrame(tsMicros uint64, key bool, size int, layer uint8) *bridge.MediaFrame {
	f := videoFrame(tsMicros, key, size)
	f.TemporalLayer = layer
	return f
}

func audioFrame(tsMicros uint64, size int) *bridge.MediaFrame {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(255 - i)
	}
	return &bridge.MediaFrame{
		Kind:      bridge.KindAudio,
		Handle:    bridge.HandleLocalAudio,
		Timestamp: tsMicros,
		Payload:   payload,
	}
}

// TestAudioLevelRoundTrip covers the speaking indicator's transport: the
// RFC 6464 byte has to survive the LOC AudioLevel property in both
// directions, or a remote tile can never light up.
func TestAudioLevelRoundTrip(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice, _ := joinRoom(t, addr, "room5", "alice")
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "audio", Codec: "opus", SampleRate: 48000, Channels: 1,
	}); err != nil {
		t.Fatalf("declare audio: %v", err)
	}

	_, bobRec := joinRoom(t, addr, "room5", "bob")
	waitFor(t, "bob to subscribe to audio", subscribeWait, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 1
	})

	// Voice active at magnitude 20 -dBov, then silent at 90.
	const (
		speaking = 0x80 | 20
		silent   = 90
	)
	for i := range 30 {
		f := audioFrame(uint64(i)*20_000, 80)
		f.HasAudioLevel = true
		if i < 15 {
			f.AudioLevel = speaking
		} else {
			f.AudioLevel = silent
		}
		if err := alice.WriteFrame(f); err != nil {
			t.Fatalf("write audio %d: %v", i, err)
		}
	}

	waitFor(t, "bob to receive all audio", 10*time.Second, func() bool {
		got, _ := bobRec.countsFor("audio")
		return got >= 30
	})

	frames, tracks, _, _ := bobRec.snapshot()
	handle := tracks[0].Handle

	// Keyed by timestamp, not arrival order. A group is one subgroup stream
	// and the router reads each on its own goroutine — deliberately, so a
	// long-lived stream cannot block another track — so two groups in flight
	// at once may be delivered in either order. Publishing 30 frames in a
	// burst puts both audio groups in flight simultaneously, which is
	// exactly that case. What has to hold is that every frame arrives
	// carrying its own level, whenever it arrives.
	levels := make(map[uint64]uint8, 30)
	for _, f := range frames {
		if f.Handle != handle {
			continue
		}
		if !f.HasAudioLevel {
			t.Fatalf("audio frame at %dus arrived without an audio level", f.Timestamp)
		}
		levels[f.Timestamp] = f.AudioLevel
	}
	if len(levels) != 30 {
		t.Fatalf("got %d distinct audio frames, want 30", len(levels))
	}
	for i := range 30 {
		ts := uint64(i) * 20_000
		want := uint8(silent)
		if i < 15 {
			want = speaking
		}
		got, ok := levels[ts]
		if !ok {
			t.Errorf("no frame arrived for timestamp %dus", ts)
			continue
		}
		if got != want {
			t.Errorf("frame at %dus: audio level = %#x, want %#x", ts, got, want)
		}
	}
	// The voice-activity bit is what actually drives the indicator, so
	// assert it separately from the magnitude.
	if levels[0]&0x80 == 0 {
		t.Error("the first speaking frame lost its voice-activity bit")
	}
	if levels[29*20_000]&0x80 != 0 {
		t.Error("the last silent frame gained a voice-activity bit it was not sent with")
	}
}

// TestAudioConfigOpensEveryGroup covers the promise made in conf.go's package
// comment: a subscriber can configure a decoder from the first object it sees,
// without waiting for the catalog to come round again.
//
// The frames themselves cannot carry that, which is the whole point. WebCodecs
// emits a codec description on the encoder's first output and never again, so
// only the opening object of a track has one — and a subscriber that joins a
// call in progress will never see it. Every group has to open with the
// description whether the frame brought one or not.
func TestAudioConfigOpensEveryGroup(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	// OpusHead, as the frontend sends it: base64 in the declaration, and the
	// same bytes are what should appear on the wire.
	opusHead := []byte("OpusHead\x01\x01\x38\x01\x80\xbb\x00\x00\x00\x00\x00")

	alice, _ := joinRoom(t, addr, "room-config", "alice")
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "audio", Codec: "opus", SampleRate: 48000, Channels: 1,
		Description: base64.StdEncoding.EncodeToString(opusHead),
	}); err != nil {
		t.Fatalf("declare audio: %v", err)
	}

	_, bobRec := joinRoom(t, addr, "room-config", "bob")
	waitFor(t, "bob to subscribe to audio", subscribeWait, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 1
	})

	// Three groups' worth at the 25-object cadence, and not one frame carries
	// a config — exactly what a live encoder produces after its first output.
	const frames = audioGroupObjects * 3
	for i := range frames {
		if err := alice.WriteFrame(audioFrame(uint64(i)*20_000, 80)); err != nil {
			t.Fatalf("write audio %d: %v", i, err)
		}
	}

	waitFor(t, "bob to receive every audio frame", 10*time.Second, func() bool {
		got, _ := bobRec.countsFor("audio")
		return got >= frames
	})

	got, _, _, _ := bobRec.snapshot()
	// Keyed by timestamp rather than arrival order, for the reason
	// TestAudioLevelRoundTrip explains: groups in flight together may be
	// delivered in either order.
	configs := make(map[uint64][]byte, frames)
	for _, f := range got {
		if f.Kind == bridge.KindAudio {
			configs[f.Timestamp] = f.Config
		}
	}

	for group := range 3 {
		ts := uint64(group*audioGroupObjects) * 20_000
		config, ok := configs[ts]
		if !ok {
			t.Fatalf("no frame arrived to open group %d (timestamp %dus)", group, ts)
		}
		if !bytes.Equal(config, opusHead) {
			t.Errorf("group %d opened with config %q, want the declared OpusHead %q",
				group, config, opusHead)
		}
	}

	// Only the first object of a group carries it: repeating it on all 25
	// would be 25 copies of a constant per group, for no one.
	if config := configs[20_000]; len(config) != 0 {
		t.Errorf("the second object of group 0 carried a config (%q); only the first should", config)
	}
}

// TestTwoParticipants covers the whole path a call depends on: namespace
// discovery, catalog exchange, media subscription, and object delivery
// with LOC metadata intact.
func TestTwoParticipants(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice, aliceRec := joinRoom(t, addr, "room1", "alice")
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "video", Codec: "avc1.42E01F", Width: 640, Height: 480, Framerate: 30,
	}); err != nil {
		t.Fatalf("alice declare video: %v", err)
	}
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "audio", Codec: "opus", SampleRate: 48000, Channels: 1,
		Description: "T3B1c0hlYWQ=", // base64 "OpusHead"
	}); err != nil {
		t.Fatalf("alice declare audio: %v", err)
	}

	_, bobRec := joinRoom(t, addr, "room1", "bob")

	// Bob must discover alice through SUBSCRIBE_NAMESPACE and pick both of
	// her tracks out of the catalog the joining FETCH backfills.
	waitFor(t, "bob to subscribe to both of alice's tracks", subscribeWait, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 2
	})

	_, tracks, peers, errs := bobRec.snapshot()
	if len(errs) != 0 {
		t.Errorf("bob saw errors: %v", errs)
	}
	if len(peers) != 1 || peers[0].Nickname != "alice" {
		t.Errorf("bob's roster = %+v, want one participant named alice", peers)
	}

	byKind := map[string]bridge.RemoteTrack{}
	for _, tr := range tracks {
		byKind[tr.Config.Kind] = tr
	}
	video, ok := byKind["video"]
	if !ok {
		t.Fatalf("bob has no video track; got %+v", tracks)
	}
	if video.Config.Codec != "avc1.42E01F" || video.Config.Width != 640 || video.Config.Height != 480 {
		t.Errorf("video config = %+v, want avc1.42E01F 640x480", video.Config)
	}
	audio, ok := byKind["audio"]
	if !ok {
		t.Fatalf("bob has no audio track; got %+v", tracks)
	}
	// The Opus description has to survive the catalog's initDataList, or a
	// subscriber cannot configure a decoder at all.
	if audio.Config.Description != "T3B1c0hlYWQ=" {
		t.Errorf("audio description = %q, want the OpusHead we published", audio.Config.Description)
	}
	if audio.Config.SampleRate != 48000 || audio.Config.Channels != 1 {
		t.Errorf("audio config = %+v, want 48000 Hz mono", audio.Config)
	}

	// Alice's own namespace is announced back to her by the relay; she must
	// not subscribe to herself.
	//
	// Waited on by nickname rather than by count, because a participant enters
	// the roster when its subscription opens and gets its nickname when the
	// catalog arrives — two separate publishParticipants calls. Waiting for the
	// count alone is satisfied by the first and then races the second, which
	// fails as a roster holding one nameless peer about one run in twenty.
	waitFor(t, "alice to see bob", 5*time.Second, func() bool {
		_, _, peers, _ := aliceRec.snapshot()
		return len(peers) == 1 && peers[0].Nickname == "bob"
	})
	_, _, alicePeers, _ := aliceRec.snapshot()
	if len(alicePeers) != 1 || alicePeers[0].Nickname != "bob" {
		t.Errorf("alice's roster = %+v, want only bob", alicePeers)
	}

	// Publish a two-GOP video sequence and a run of audio frames.
	const (
		videoFrames = 60
		audioFrames = 60
		videoSize   = 700
		audioSize   = 80
	)
	for i := range videoFrames {
		if err := alice.WriteFrame(videoFrame(uint64(i)*33_000, i%30 == 0, videoSize)); err != nil {
			t.Fatalf("write video %d: %v", i, err)
		}
	}
	for i := range audioFrames {
		if err := alice.WriteFrame(audioFrame(uint64(i)*20_000, audioSize)); err != nil {
			t.Fatalf("write audio %d: %v", i, err)
		}
	}

	waitFor(t, "bob to receive alice's media", 10*time.Second, func() bool {
		v, _ := bobRec.countsFor("video")
		a, _ := bobRec.countsFor("audio")
		if testing.Verbose() {
			t.Logf("progress: video=%d audio=%d", v, a)
		}
		return v >= videoFrames && a >= audioFrames
	})

	// Exactly one inbound object per published frame. Over- or
	// under-counting here means the group/subgroup mapping is wrong.
	gotVideo, gotVideoBytes := bobRec.countsFor("video")
	if gotVideo != videoFrames {
		t.Errorf("bob received %d video objects, want exactly %d", gotVideo, videoFrames)
	}
	if want := videoFrames * videoSize; gotVideoBytes != want {
		t.Errorf("bob received %d video bytes, want %d", gotVideoBytes, want)
	}
	gotAudio, gotAudioBytes := bobRec.countsFor("audio")
	if gotAudio != audioFrames {
		t.Errorf("bob received %d audio objects, want exactly %d", gotAudio, audioFrames)
	}
	if want := audioFrames * audioSize; gotAudioBytes != want {
		t.Errorf("bob received %d audio bytes, want %d", gotAudio, want)
	}

	// LOC metadata must survive the round trip: timestamps are what the
	// receiving decoder schedules on.
	frames, _, _, _ := bobRec.snapshot()
	var checkedVideo, checkedAudio bool
	for _, f := range frames {
		switch f.Handle {
		case video.Handle:
			if f.Timestamp%33_000 != 0 {
				t.Errorf("video timestamp %d is not one we published", f.Timestamp)
			}
			checkedVideo = true
		case audio.Handle:
			if f.Timestamp%20_000 != 0 {
				t.Errorf("audio timestamp %d is not one we published", f.Timestamp)
			}
			checkedAudio = true
		}
	}
	if !checkedVideo || !checkedAudio {
		t.Error("no frames arrived for one of the tracks")
	}
}

// TestVideoNeedsKeyFrame pins the group mapping's precondition: a group
// must open on a keyframe, so leading delta frames are refused rather than
// published into a group no subscriber could decode.
func TestVideoNeedsKeyFrame(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()
	alice, _ := joinRoom(t, addr, "room2", "alice")

	if err := alice.WriteFrame(videoFrame(0, false, 100)); err == nil {
		t.Fatal("writing a delta frame before any keyframe succeeded, want ErrAwaitingKeyFrame")
	} else if !errors.Is(err, ErrAwaitingKeyFrame) {
		t.Fatalf("got %v, want ErrAwaitingKeyFrame", err)
	}

	if err := alice.WriteFrame(videoFrame(0, true, 100)); err != nil {
		t.Fatalf("writing a keyframe: %v", err)
	}
	// Deltas are fine once a group is open.
	if err := alice.WriteFrame(videoFrame(33_000, false, 100)); err != nil {
		t.Fatalf("writing a delta after a keyframe: %v", err)
	}
}

// TestParticipantLeaves checks the departure path: closing a Room must
// withdraw its namespace so peers drop the participant and retire the
// decoders bound to their handles.
func TestParticipantLeaves(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice, _ := joinRoom(t, addr, "room3", "alice")
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "video", Codec: "avc1.42E01F", Width: 320, Height: 240,
	}); err != nil {
		t.Fatalf("alice declare video: %v", err)
	}

	_, bobRec := joinRoom(t, addr, "room3", "bob")
	waitFor(t, "bob to see alice", 10*time.Second, func() bool {
		_, _, peers, _ := bobRec.snapshot()
		return len(peers) == 1
	})

	alice.Close()

	waitFor(t, "bob to drop alice", 10*time.Second, func() bool {
		_, _, peers, _ := bobRec.snapshot()
		return len(peers) == 0
	})
}

// testLogger routes a Room's logs into the test output, so a failure shows
// what the session was doing.
func testLogger(t *testing.T) *slog.Logger {
	if !testing.Verbose() {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// TestAudioOnly isolates the audio group cadence from the video path.
func TestAudioOnly(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice, _ := joinRoom(t, addr, "room4", "alice")
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "audio", Codec: "opus", SampleRate: 48000, Channels: 1,
	}); err != nil {
		t.Fatalf("declare audio: %v", err)
	}

	_, bobRec := joinRoom(t, addr, "room4", "bob")
	waitFor(t, "bob to subscribe to audio", subscribeWait, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 1
	})

	for i := range 60 {
		if err := alice.WriteFrame(audioFrame(uint64(i)*20_000, 80)); err != nil {
			t.Fatalf("write audio %d: %v", i, err)
		}
	}

	waitFor(t, "audio to arrive", 10*time.Second, func() bool {
		got, _ := bobRec.countsFor("audio")
		if testing.Verbose() {
			t.Logf("audio received: %d", got)
		}
		return got >= 60
	})
}

// TestTrackReconfiguration covers what an in-call device or resolution switch
// does on the wire: the publisher re-declares its video track, which
// republishes the catalog, and the subscriber has to retire the old
// subscription and pick up the new one under a fresh handle. Without that the
// frontend would keep feeding a decoder configured for the old geometry.
func TestTrackReconfiguration(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice, _ := joinRoom(t, addr, "room6", "alice")
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "video", Codec: "avc1.42E01F", Width: 640, Height: 480, Framerate: 30,
	}); err != nil {
		t.Fatalf("declare video: %v", err)
	}

	_, bobRec := joinRoom(t, addr, "room6", "bob")
	waitFor(t, "bob to subscribe to the first configuration", subscribeWait, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 1
	})

	_, tracks, _, _ := bobRec.snapshot()
	first := tracks[0]
	if first.Config.Width != 640 {
		t.Fatalf("first config width = %d, want 640", first.Config.Width)
	}

	for i := range 30 {
		if err := alice.WriteFrame(videoFrame(uint64(i)*33_000, i%30 == 0, 500)); err != nil {
			t.Fatalf("write video %d: %v", i, err)
		}
	}
	waitFor(t, "media on the first configuration", 10*time.Second, func() bool {
		got, _ := bobRec.countsFor("video")
		return got >= 30
	})

	// The switch: same track, different geometry.
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "video", Codec: "avc1.42E01F", Width: 1280, Height: 720, Framerate: 30,
	}); err != nil {
		t.Fatalf("redeclare video: %v", err)
	}

	waitFor(t, "bob to resubscribe under a new handle", subscribeWait, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		for _, tr := range tracks {
			if tr.Config.Width == 1280 && tr.Handle != first.Handle {
				return true
			}
		}
		return false
	})

	_, tracks, _, _ = bobRec.snapshot()
	var second bridge.RemoteTrack
	for _, tr := range tracks {
		if tr.Config.Width == 1280 {
			second = tr
		}
	}
	if second.Handle == 0 {
		t.Fatal("no track announced for the new configuration")
	}
	if second.Handle == first.Handle {
		t.Error("reconfigured track reused its handle; the frontend keys decoders off it")
	}
	if second.Config.Height != 720 {
		t.Errorf("new config height = %d, want 720", second.Config.Height)
	}

	// And media must resume on the new subscription, not merely be announced.
	before, _ := bobRec.countsFor("video")
	for i := range 30 {
		if err := alice.WriteFrame(videoFrame(uint64(1000+i)*33_000, i%30 == 0, 900)); err != nil {
			t.Fatalf("write video after switch %d: %v", i, err)
		}
	}
	waitFor(t, "media on the new configuration", 10*time.Second, func() bool {
		got, _ := bobRec.countsFor("video")
		return got > before
	})
}

// TestSessionLossIsDetected covers the signal a reconnect depends on: when
// the relay goes away, Lost has to close. Everything else about recovery is
// built on noticing at all.
func TestSessionLossIsDetected(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice, _ := joinRoom(t, addr, "room7", "alice")

	select {
	case <-alice.Lost():
		t.Fatal("Lost closed while the relay was still up")
	case <-time.After(200 * time.Millisecond):
	}

	relayServer.Stop()

	select {
	case <-alice.Lost():
	case <-time.After(10 * time.Second):
		t.Fatal("Lost did not close after the relay went away")
	}
}

// TestSessionLossOnSilentOutage covers the failure that has no notification
// at all: the relay stops answering without closing anything, as a crash or a
// network partition does. Nothing arrives to say so, so detection is the QUIC
// idle timeout — and that timeout is how long a call sits dead before a
// reconnect can even start, which is why it is set where it is in dial.go.
//
// Necessarily slower than the graceful case; it is waiting out a real timeout.
func TestSessionLossOnSilentOutage(t *testing.T) {
	relayServer := startRelay(t)
	alice, _ := joinRoom(t, relayServer.Addr(), "room7b", "alice")

	relayServer.Kill()

	// Generous against the configured MaxIdleTimeout so a loaded machine
	// does not fail the test, while still failing if detection regresses to
	// the tens of seconds it used to take.
	select {
	case <-alice.Lost():
	case <-time.After(20 * time.Second):
		t.Fatal("Lost did not close after the relay stopped answering")
	}
}

// TestLeavingIsNotLoss pins the other half of that signal: Close must not
// look like a failure, or a supervisor would reconnect to a room the user
// deliberately left.
func TestLeavingIsNotLoss(t *testing.T) {
	relayServer := startRelay(t)
	alice, _ := joinRoom(t, relayServer.Addr(), "room8", "alice")

	alice.Close()

	select {
	case <-alice.Lost():
		t.Fatal("Lost closed for a deliberate Close")
	case <-time.After(500 * time.Millisecond):
	}
}

// TestRejoinAfterRelayRestart walks the sequence the reconnect supervisor
// performs — notice the loss, close the dead room, re-join the same room on
// the same address — and checks the call genuinely comes back: both
// participants rediscover each other and media flows again.
func TestRejoinAfterRelayRestart(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	videoConfig := &bridge.TrackConfig{
		Kind: "video", Codec: "avc1.42E01F", Width: 640, Height: 480, Framerate: 30,
	}

	alice, _ := joinRoom(t, addr, "room9", "alice")
	if err := alice.DeclareTrack(videoConfig); err != nil {
		t.Fatalf("alice declare video: %v", err)
	}
	_, bobRec := joinRoom(t, addr, "room9", "bob")

	waitFor(t, "bob to see alice's track", 10*time.Second, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 1
	})
	for i := range 30 {
		if err := alice.WriteFrame(videoFrame(uint64(i)*33_000, i%30 == 0, 400)); err != nil {
			t.Fatalf("write video %d: %v", i, err)
		}
	}
	waitFor(t, "media before the outage", 10*time.Second, func() bool {
		got, _ := bobRec.countsFor("video")
		return got >= 30
	})

	// The outage — a redeploy, so sessions are closed rather than stranded.
	relayServer.Stop()
	for name, room := range map[string]*Room{"alice": alice} {
		select {
		case <-room.Lost():
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not notice the relay going away", name)
		}
	}
	alice.Close()

	relayServer.Start()
	if got := relayServer.Addr(); got != addr {
		t.Fatalf("relay came back on %s, want the original %s", got, addr)
	}

	// What the supervisor does: re-join, then replay the declarations so the
	// new session's catalog describes the same tracks.
	alice2, _ := joinRoom(t, addr, "room9", "alice")
	if err := alice2.DeclareTrack(videoConfig); err != nil {
		t.Fatalf("alice re-declare video: %v", err)
	}

	// bob never left, so his own reconnect is what rediscovers alice. He is
	// still on the old session here, so stand him up again too.
	_, bobRec2 := joinRoom(t, addr, "room9", "bob")

	waitFor(t, "bob to rediscover alice after the restart", 15*time.Second, func() bool {
		_, tracks, _, _ := bobRec2.snapshot()
		return len(tracks) == 1
	})

	before, _ := bobRec2.countsFor("video")
	for i := range 30 {
		if err := alice2.WriteFrame(videoFrame(uint64(1000+i)*33_000, i%30 == 0, 400)); err != nil {
			t.Fatalf("write video after restart %d: %v", i, err)
		}
	}
	waitFor(t, "media after the restart", 15*time.Second, func() bool {
		got, _ := bobRec2.countsFor("video")
		return got > before
	})
}

// TestGoawayIsActedOnBeforeTheCutoff covers GOAWAY (§10.4). A relay draining
// gracefully tells us to move and then waits before closing the session; the
// whole value of the message is learning about it during that window, not
// after. So the test asserts not merely that we notice, but that we notice
// while the session is still usable — a client that only reacted to the close
// would spend the entire grace period publishing into a session on its way
// out.
func TestGoawayIsActedOnBeforeTheCutoff(t *testing.T) {
	relayServer := startRelay(t)
	// Long enough that "before the cutoff" is a real claim rather than a
	// coincidence of scheduling.
	relayServer.goawayGrace = 3 * time.Second
	relayServer.Stop()
	relayServer.Start()

	alice, _ := joinRoom(t, relayServer.Addr(), "room10", "alice")

	// Draining begins.
	go relayServer.Stop()

	select {
	case <-alice.Migrating():
	case <-alice.Lost():
		t.Fatal("the session closed before GOAWAY was reported; the grace period was wasted")
	case <-time.After(10 * time.Second):
		t.Fatal("GOAWAY was never reported")
	}

	// Still usable at the moment we were told, which is what makes acting
	// early worth anything.
	select {
	case <-alice.Lost():
		t.Error("session was already gone when GOAWAY surfaced")
	default:
	}

	m := alice.Migration()
	if m.Grace != 3*time.Second {
		t.Errorf("grace = %s, want 3s — the relay's timeout did not survive the wire", m.Grace)
	}
	// This relay names no replacement, which means "reconnect to me".
	if m.Relay != "" {
		t.Errorf("new relay = %q, want empty", m.Relay)
	}
}

// TestNoGoawayOnAbruptLoss keeps the two signals apart: a relay that vanishes
// says nothing, so Migrating must stay silent and only Lost fire. Confusing
// the two would have the supervisor wait for a grace period nobody granted.
func TestNoGoawayOnAbruptLoss(t *testing.T) {
	relayServer := startRelay(t)
	alice, _ := joinRoom(t, relayServer.Addr(), "room11", "alice")

	relayServer.Kill()

	select {
	case <-alice.Lost():
	case <-time.After(20 * time.Second):
		t.Fatal("Lost did not close after the relay stopped answering")
	}
	select {
	case <-alice.Migrating():
		t.Error("Migrating closed although no GOAWAY was ever sent")
	default:
	}
}
