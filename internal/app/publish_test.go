package app

import (
	"testing"
	"time"

	"tlmst/internal/bridge"
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
	video := newPublishPump(a.log, "video", 4)
	audio := newPublishPump(a.log, "audio", 4)
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
	if len(video.frames) != 4 || len(audio.frames) != 4 {
		t.Errorf("queues hold video=%d audio=%d, want 4 each",
			len(video.frames), len(audio.frames))
	}
}

// A queued frame outlives the read buffer its payload came from, so it has to
// be copied on the way in. Without this the publisher writes whatever the next
// WebSocket message happened to leave in that memory.
func TestQueuedFramesDoNotAliasTheReadBuffer(t *testing.T) {
	a := newTestApp(t)
	pump := newPublishPump(a.log, "video", 4)
	a.videoPump = pump

	buf := []byte{1, 2, 3}
	if err := a.HandleMedia(t.Context(), &bridge.MediaFrame{
		Kind:    bridge.KindVideo,
		Payload: buf,
		Config:  buf,
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
