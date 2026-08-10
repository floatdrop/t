package app

import (
	"testing"
	"time"

	"t/internal/bridge"
)

// The bridge's read goroutine must never wait on a publish.
//
// HandleMedia used to run synchronously all the way down to a QUIC stream
// write, so a write blocked on flow control — which is what a congested uplink
// is — stopped the WebSocket being read at all. Audio then queued behind video
// at the source, before any of the machinery that keeps the two apart
// downstream could see it.
//
// This drives the case directly: a pump whose reader never runs, filled well
// past its depth. Every call must return, and the frames that could not be
// taken must be counted rather than lost quietly.
func TestPublishingNeverBlocksTheReader(t *testing.T) {
	a := newTestApp(t)
	video := newPublishPump(a.log, "video", 4, true, nil)
	audio := newPublishPump(a.log, "audio", 4, false, nil)
	a.videoPump = video
	a.audioPump = audio

	const sent = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range sent {
			_ = a.HandleMedia(t.Context(), &bridge.MediaFrame{
				Kind:      bridge.KindVideo,
				Timestamp: uint64(i),
				KeyFrame:  true, // every frame admissible, so the queue really fills
				Payload:   []byte{byte(i)},
			})
			_ = a.HandleMedia(t.Context(), &bridge.MediaFrame{
				Kind:      bridge.KindAudio,
				Timestamp: uint64(i),
				Payload:   []byte{byte(i)},
			})
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("HandleMedia blocked with nothing draining the pump; a " +
			"congested uplink would stop the bridge being read at all")
	}

	if got := video.dropped.Load(); got == 0 {
		t.Error("nothing was counted as dropped, so this proves nothing about " +
			"what a full queue does")
	}
	if len(audio.frames) != 4 {
		t.Errorf("audio queue holds %d frames, want 4", len(audio.frames))
	}
}

// A queued frame outlives the read buffer its payload came from, so it has to
// be copied on the way in. Without this the publisher writes whatever the next
// WebSocket message happened to leave in that memory.
func TestQueuedFramesDoNotAliasTheReadBuffer(t *testing.T) {
	a := newTestApp(t)
	pump := newPublishPump(a.log, "video", 4, true, nil)
	a.videoPump = pump

	buf := []byte{1, 2, 3}
	if err := a.HandleMedia(t.Context(), &bridge.MediaFrame{
		Kind:     bridge.KindVideo,
		KeyFrame: true,
		Payload:  buf,
		Config:   buf,
	}); err != nil {
		t.Fatalf("HandleMedia: %v", err)
	}
	// The bridge reuses or reclaims the buffer once HandleMedia returns.
	buf[0], buf[1], buf[2] = 9, 9, 9

	queued := <-pump.frames
	if queued.Payload[0] != 1 || queued.Config[0] != 1 {
		t.Errorf("queued frame followed the caller's buffer: payload=%v config=%v",
			queued.Payload, queued.Config)
	}
}

// Frames arriving with no room are discarded, not queued for one that may never
// come. They trail a Leave by a few milliseconds while the encoders wind down.
func TestFramesWithNoRoomAreDropped(t *testing.T) {
	a := newTestApp(t)
	if err := a.HandleMedia(t.Context(), &bridge.MediaFrame{
		Kind:    bridge.KindVideo,
		Payload: []byte{1},
	}); err != nil {
		t.Errorf("HandleMedia with no room = %v, want nil", err)
	}
}

// Shedding video must cut to a keyframe, never take a frame out of the middle.
//
// This is the fault the pump introduced when it started dropping instead of
// blocking. Dropping the oldest is right for audio, where every packet stands
// alone; for video it leaves every frame after the hole referencing something
// the subscriber never received, and H.264 does not report that — it draws it,
// as macroblock smear, until the next keyframe. Measured on a real call over a
// saturated uplink: two thousand frames shed this way and a face that was
// unrecognisable for most of a minute.
//
// So a full video queue abandons the whole backlog and publishes nothing until
// a keyframe starts a group the subscriber can actually decode.
func TestSheddingVideoCutsToAKeyFrame(t *testing.T) {
	a := newTestApp(t)
	asked := 0
	pump := newPublishPump(a.log, "video", 4, true, func() { asked++ })
	a.videoPump = pump

	deltas := func(n int) {
		for i := range n {
			pump.offer(&bridge.MediaFrame{
				Kind: bridge.KindVideo, Timestamp: uint64(i), Payload: []byte{1},
			})
		}
	}

	// A group opens, then the reader stalls and the queue overflows.
	pump.offer(&bridge.MediaFrame{Kind: bridge.KindVideo, KeyFrame: true, Payload: []byte{1}})
	deltas(20)

	if len(pump.frames) != 0 {
		t.Errorf("queue holds %d frames after shedding, want none — the backlog "+
			"belongs to a group no subscriber can finish", len(pump.frames))
	}
	if asked == 0 {
		t.Error("shedding did not ask the encoder for a keyframe, so publishing " +
			"waits for the next scheduled one")
	}

	// Deltas stay refused while the gap lasts: publishing them would be the
	// hole this exists to avoid.
	deltas(5)
	if len(pump.frames) != 0 {
		t.Fatalf("queued %d delta frames while waiting for a keyframe",
			len(pump.frames))
	}

	// The keyframe reopens it, and normal publishing resumes.
	pump.offer(&bridge.MediaFrame{Kind: bridge.KindVideo, KeyFrame: true, Payload: []byte{2}})
	deltas(2)
	if got := len(pump.frames); got != 3 {
		t.Errorf("queue holds %d frames after the keyframe, want 3", got)
	}
	first := <-pump.frames
	if !first.KeyFrame {
		t.Error("the first frame published after a gap is not a keyframe")
	}
}

// Audio sheds the other way, and must keep doing so: every Opus packet stands
// on its own, so the oldest is simply the one worth least and there is no group
// to protect.
func TestSheddingAudioKeepsTheNewest(t *testing.T) {
	a := newTestApp(t)
	pump := newPublishPump(a.log, "audio", 4, false, nil)

	for i := range uint64(10) {
		pump.offer(&bridge.MediaFrame{
			Kind: bridge.KindAudio, Timestamp: i, Payload: []byte{byte(i)},
		})
	}
	if got := len(pump.frames); got != 4 {
		t.Fatalf("audio queue holds %d frames, want 4", got)
	}
	first := <-pump.frames
	if first.Timestamp != 6 {
		t.Errorf("oldest surviving audio packet has timestamp %d, want 6 — the "+
			"queue kept history and discarded the live edge", first.Timestamp)
	}
}
