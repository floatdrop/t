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
		media:   make(chan outbound, sendQueueDepth),
		control: make(chan outbound, controlQueueDepth),
		ctx:     ctx,
		cancel:  cancel,
	}
	return &Server{conn: c}, c
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

	// One frame more than the queue can hold, each identifiable by its
	// timestamp, so what survived can be named rather than counted.
	const overflow = 10
	for i := range sendQueueDepth + overflow {
		s.SendMedia(&MediaFrame{
			Kind:      KindVideo,
			Handle:    HandleRemoteBase,
			Timestamp: uint64(i),
			Payload:   []byte{byte(i)},
		})
	}

	if got := s.DroppedFrames(); got != overflow {
		t.Errorf("dropped %d frames, want %d", got, overflow)
	}
	if got := len(c.media); got != sendQueueDepth {
		t.Fatalf("queue holds %d frames, want %d", got, sendQueueDepth)
	}

	// The survivors must be the last ones sent. The oldest `overflow` frames
	// are the ones that should have gone.
	first := <-c.media
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

	for i := range sendQueueDepth * 2 {
		s.SendMedia(&MediaFrame{Timestamp: uint64(i), Payload: []byte{1}})
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
