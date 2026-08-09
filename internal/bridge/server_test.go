package bridge

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/coder/websocket"
)

// stalledConn is a connection whose writer never runs, which is what a WebView
// looks like while macOS is resizing its window: the socket stops being read,
// the write loop blocks, and the queues fill behind it.
func stalledConn(t *testing.T) (*Server, *conn) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &conn{
		video:   make(chan outbound, videoQueueDepth),
		audio:   make(chan outbound, audioQueueDepth),
		control: make(chan outbound, controlQueueDepth),
		ctx:     ctx,
		cancel:  cancel,
	}
	return &Server{conn: c}, c
}

// videoFrame and audioFrame are frames identifiable by timestamp, so what
// survived a queue can be named rather than counted.
func videoFrame(ts uint64) *MediaFrame {
	return &MediaFrame{
		Kind:      KindVideo,
		Handle:    HandleRemoteBase,
		Timestamp: ts,
		Payload:   []byte{byte(ts)},
	}
}

func audioFrame(ts uint64) *MediaFrame {
	return &MediaFrame{
		Kind:      KindAudio,
		Handle:    HandleRemoteBase + 1,
		Timestamp: ts,
		Payload:   []byte{byte(ts)},
	}
}

// TestMediaQueueKeepsTheNewest covers the policy the whole bridge rests on: a
// full queue discards the oldest frame, not the one being sent.
//
// Getting this backwards is not a lost frame here and there. A WebView that
// stops reading leaves the queue holding stale video and every frame produced
// afterwards — keyframes included — thrown away, so the picture cannot resume
// until the publisher's next scheduled keyframe, long after the stall ended.
func TestMediaQueueKeepsTheNewest(t *testing.T) {
	s, c := stalledConn(t)

	// One frame more than the queue can hold.
	const overflow = 10
	for i := range videoQueueDepth + overflow {
		s.SendMedia(videoFrame(uint64(i)))
	}

	if got, _ := s.DroppedFrames(); got != overflow {
		t.Errorf("dropped %d video frames, want %d", got, overflow)
	}
	if got := len(c.video); got != videoQueueDepth {
		t.Fatalf("queue holds %d frames, want %d", got, videoQueueDepth)
	}

	// The survivors must be the last ones sent. The oldest `overflow` frames
	// are the ones that should have gone.
	first := <-c.video
	f, err := ParseFrame(first.data)
	if err != nil {
		t.Fatalf("parse queued frame: %v", err)
	}
	if f.Timestamp != overflow {
		t.Errorf("oldest surviving frame has timestamp %d, want %d — the queue "+
			"kept history and discarded the live edge", f.Timestamp, overflow)
	}
}

// TestControlSurvivesASaturatedMediaQueue covers the other half: control
// messages are not interchangeable with each other or with frames. A dropped
// remoteTrack is a participant who never gets a decoder, and a dropped state
// is a phase the frontend never learns about — neither is recoverable by
// waiting, the way a lost frame is.
func TestControlSurvivesASaturatedMediaQueue(t *testing.T) {
	s, c := stalledConn(t)

	for i := range videoQueueDepth * 2 {
		s.SendMedia(videoFrame(uint64(i)))
	}

	s.SendControl(&ServerMessage{
		Type:  MsgRemoteTrack,
		Track: &RemoteTrack{Handle: HandleRemoteBase, Participant: "peer"},
	})
	s.SendControl(&ServerMessage{Type: MsgState, State: &SessionState{Phase: PhaseJoined}})

	if got := len(c.control); got != 2 {
		t.Fatalf("control queue holds %d messages, want 2", got)
	}
	for _, want := range []string{MsgRemoteTrack, MsgState} {
		msg := <-c.control
		if msg.typ != websocket.MessageText {
			t.Errorf("control message sent as opcode %v, want text", msg.typ)
		}
		if got := decodeType(t, msg.data); got != want {
			t.Errorf("control message type = %q, want %q", got, want)
		}
	}
}

// TestAudioSurvivesASaturatedVideoQueue is the property the split exists for.
//
// The two shared 256 slots once, so a video backlog evicted audio to make room
// for itself — 32 kbps of sound thrown away to hold stale 1.5 Mbps video. What
// the listener heard was a gap, and then, when the frontend caught up, a burst
// the player had to trim to bound its latency: another gap. Video is now
// discarded from its own queue and audio never notices.
func TestAudioSurvivesASaturatedVideoQueue(t *testing.T) {
	s, c := stalledConn(t)

	// Far past what the video queue can hold, with audio arriving throughout at
	// roughly the ratio a real call produces.
	const audioFrames = audioQueueDepth
	for i := range videoQueueDepth * 4 {
		s.SendMedia(videoFrame(uint64(i)))
		if i%8 == 0 && uint64(i/8) < audioFrames {
			s.SendMedia(audioFrame(uint64(i / 8)))
		}
	}

	video, audio := s.DroppedFrames()
	if video == 0 {
		t.Fatal("no video was dropped, so this proves nothing about audio")
	}
	if audio != 0 {
		t.Errorf("dropped %d audio frames while video was saturated; the two "+
			"queues are meant to be independent", audio)
	}
	if got := len(c.audio); got != audioFrames {
		t.Fatalf("audio queue holds %d frames, want %d", got, audioFrames)
	}

	// And in order, from the first one sent: nothing was evicted from the front.
	for want := range uint64(audioFrames) {
		f, err := ParseFrame((<-c.audio).data)
		if err != nil {
			t.Fatalf("parse queued audio frame: %v", err)
		}
		if f.Timestamp != want {
			t.Fatalf("audio frame %d has timestamp %d; the queue lost or "+
				"reordered sound while video was backing up", want, f.Timestamp)
		}
	}
}

// TestAudioIsWrittenAheadOfVideo pins the ordering half of the split. Separate
// queues stop audio being evicted; taking audio first is what stops it waiting
// behind a video backlog already queued ahead of it in one write loop.
func TestAudioIsWrittenAheadOfVideo(t *testing.T) {
	s, c := stalledConn(t)

	// A full video queue, then one audio packet behind all of it.
	for i := range uint64(videoQueueDepth) {
		s.SendMedia(videoFrame(i))
	}
	s.SendMedia(audioFrame(999))

	msg, ok := c.nextReady()
	if !ok {
		t.Fatal("nothing ready with both queues loaded")
	}
	f, err := ParseFrame(msg.data)
	if err != nil {
		t.Fatalf("parse queued frame: %v", err)
	}
	if f.Kind != KindAudio {
		t.Fatalf("the write loop would send video first; audio queued behind "+
			"%d video frames waits for all of them", videoQueueDepth)
	}

	// And control still outranks both.
	s.SendControl(&ServerMessage{Type: MsgState, State: &SessionState{Phase: PhaseJoined}})
	msg, ok = c.nextReady()
	if !ok {
		t.Fatal("nothing ready with all three queues loaded")
	}
	if msg.typ != websocket.MessageText {
		t.Error("a control message did not outrank queued media")
	}
}

// TestSendOnADeadConnectionDoesNotBlock covers the shutdown edge: the write
// loop is gone, so nothing will ever drain either queue, and a send that
// waited for room would wedge the MOQ read loop that called it.
func TestSendOnADeadConnectionDoesNotBlock(t *testing.T) {
	s, c := stalledConn(t)
	c.cancel() // the connection has gone away

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.SendControl(&ServerMessage{Type: MsgState})
		s.SendMedia(&MediaFrame{Payload: []byte{1}})
	}()
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("send blocked on a connection that is already gone")
	}
}

// TestControlOverflowFailsTheConnectionRatherThanBlocking covers the deadlock
// that a never-dropping control queue invites.
//
// Every goroutine in the process reaches enqueueControl: the log sink emits
// synchronously on whichever goroutine called slog, and slog is the default
// logger, so the transport's own diagnostics arrive here too. If a full queue
// waited for room, a frontend that merely stopped reading its socket would
// stall the metrics sampler, the read loop, whichever conf goroutine was
// mid-subscribe holding its lock, and quic-go underneath it — the whole
// backend, on nothing worse than a window being resized.
func TestControlOverflowFailsTheConnectionRatherThanBlocking(t *testing.T) {
	s, c := stalledConn(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Twice the depth, so the overflow is reached with plenty to spare.
		for range controlQueueDepth * 2 {
			s.SendControl(&ServerMessage{Type: MsgState, State: &SessionState{Phase: PhaseJoined}})
		}
	}()
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("a full control queue blocked the sender")
	}

	if c.ctx.Err() == nil {
		t.Error("the connection survived a control queue that overflowed; " +
			"a frontend that far behind is gone and should be failed, not propped up")
	}
}

// TestControlQueueOutgrowsTheLogBackfill pins a coupling that is invisible at
// either end: HandleConnect replays the whole log ring into this queue the
// moment a frontend attaches, before it has read a byte. Sized under that, a
// reconnecting WebView would overflow its own backlog on arrival and be
// disconnected for it — a reconnect loop that can never converge.
func TestControlQueueOutgrowsTheLogBackfill(t *testing.T) {
	const logRingSize = 2000 // telemetry.logRingSize, unexported
	if controlQueueDepth <= logRingSize {
		t.Fatalf("controlQueueDepth %d must exceed the %d-entry log backfill",
			controlQueueDepth, logRingSize)
	}
}

func decodeType(t *testing.T, data []byte) string {
	t.Helper()
	var msg ServerMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal control message: %v", err)
	}
	return msg.Type
}
