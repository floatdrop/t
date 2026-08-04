package conf

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/floatdrop/moq-go/pkg/moqt/session/quicconn"
	"github.com/floatdrop/moq-go/pkg/relay"

	"tlmst/internal/bridge"
	"tlmst/internal/telemetry"
)

// recorder is a conf.Sink that captures everything a Room delivers, so a
// test can assert on the frames and control messages the frontend would
// have seen.
type recorder struct {
	mu sync.Mutex

	frames []bridge.MediaFrame
	tracks []bridge.RemoteTrack
	peers  []bridge.Participant
	errs   []string
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
	r.objects[f.Handle]++
	r.bytes[f.Handle] += len(f.Payload)
}

func (r *recorder) SendControl(msg *bridge.ServerMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch msg.Type {
	case bridge.MsgRemoteTrack:
		r.tracks = append(r.tracks, *msg.Track)
	case bridge.MsgParticipants:
		r.peers = msg.Participants
	case bridge.MsgError:
		r.errs = append(r.errs, msg.Error)
	}
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

// startRelay runs a moq-go relay on a loopback QUIC listener and returns
// its address. Hermetic: an ephemeral port and a self-signed certificate,
// torn down with the test.
func startRelay(t *testing.T) string {
	t.Helper()

	cert := selfSignedCert(t)
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	ql, err := quic.Listen(udp, &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{alpnDraft19},
	}, &quic.Config{
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true,
	})
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}

	r := relay.New(quicconn.NewListener(ql), relay.Config{
		Logger: testLogger(t).With("component", "relay"),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = ql.Close()
		_ = udp.Close()
		<-done
	})
	return udp.LocalAddr().String()
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tlmst-test"},
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
func joinRoom(t *testing.T, addr, room, nickname string) (*Room, *recorder) {
	t.Helper()
	rec := newRecorder()
	r, err := Join(t.Context(), testLogger(t), rec, telemetry.NewRegistry(), Config{
		Relay:    addr,
		Room:     room,
		Nickname: nickname,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("join as %s: %v", nickname, err)
	}
	t.Cleanup(r.Close)
	return r, rec
}

// waitFor polls until cond holds or the deadline passes. Discovery and
// media delivery are asynchronous, so tests wait on outcomes rather than
// sleeping a fixed amount.
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

// TestTwoParticipants covers the whole path a call depends on: namespace
// discovery, catalog exchange, media subscription, and object delivery
// with LOC metadata intact.
func TestTwoParticipants(t *testing.T) {
	addr := startRelay(t)

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
	waitFor(t, "bob to subscribe to both of alice's tracks", 10*time.Second, func() bool {
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
	waitFor(t, "alice to see bob", 5*time.Second, func() bool {
		_, _, peers, _ := aliceRec.snapshot()
		return len(peers) == 1
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
	addr := startRelay(t)
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
	addr := startRelay(t)

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
	addr := startRelay(t)

	alice, _ := joinRoom(t, addr, "room4", "alice")
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "audio", Codec: "opus", SampleRate: 48000, Channels: 1,
	}); err != nil {
		t.Fatalf("declare audio: %v", err)
	}

	_, bobRec := joinRoom(t, addr, "room4", "bob")
	waitFor(t, "bob to subscribe to audio", 10*time.Second, func() bool {
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
