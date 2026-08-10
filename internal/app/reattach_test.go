package app

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"t/internal/bridge"
	"t/internal/conf"
)

// heldRoom is a Room that is never dereferenced: these tests are about whether
// one is still held and whether the countdown is armed, not about anything
// done to it. A real one needs a relay, which internal/conf already covers.
func heldRoom() *conf.Room { return &conf.Room{} }

func withServer(t *testing.T, a *App) {
	t.Helper()
	srv, err := bridge.NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), a)
	if err != nil {
		t.Fatalf("bridge server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	a.SetServer(srv)
}

// The reattach window happens between two events seconds apart, so these drive
// the decision points directly rather than through a WebView. What they pin is
// the bookkeeping: whether the countdown is armed, disarmed by the two things
// that should disarm it, and never armed when there is nothing to hold. That
// the room itself survives follows from HandleDisconnect no longer calling
// leave, which is one line and visible in the diff; a test of it would need a
// live session, which internal/conf's relay tests already provide.

// TestReattachArmsTheCountdown covers the window opening: something must
// eventually leave the room, or a WebView that never comes back leaves a
// participant announced in the room forever, publishing nothing.
func TestReattachArmsTheCountdown(t *testing.T) {
	a := newTestApp(t)
	a.room = heldRoom()

	a.holdRoom()

	a.mu.Lock()
	timer := a.reattach
	a.mu.Unlock()
	if timer == nil {
		t.Error("nothing armed to leave the room if no frontend comes back")
	}
}

// TestLeaveDisarmsTheCountdown covers a stray timer outliving the room it was
// watching. Leaving during the window and joining another room would otherwise
// have the old countdown fire into the new call and end it.
func TestLeaveDisarmsTheCountdown(t *testing.T) {
	a := newTestApp(t)
	withServer(t, a)
	a.room = heldRoom()

	a.holdRoom()
	a.room = nil // leave() closes a real room; the bookkeeping is what matters
	a.leave()

	a.mu.Lock()
	timer := a.reattach
	a.mu.Unlock()
	if timer != nil {
		t.Error("a countdown survived the room it was watching, and would fire into the next call")
	}
}

// TestDisconnectWhileIdleArmsNothing covers the ordinary case: a WebView that
// goes away when no call is in progress has nothing to hold, and arming a
// timer for it would schedule a leave against whatever room came next.
func TestDisconnectWhileIdleArmsNothing(t *testing.T) {
	a := newTestApp(t)
	withServer(t, a)

	a.HandleDisconnect() // no room joined

	a.mu.Lock()
	timer := a.reattach
	a.mu.Unlock()
	if timer != nil {
		t.Error("armed a countdown with no room to hold")
	}
}

// TestReattachDisarmsOnReconnect covers the other half: a frontend that comes
// back must find the room still there and the countdown stopped.
func TestReattachDisarmsOnReconnect(t *testing.T) {
	a := newTestApp(t)
	withServer(t, a)
	a.room = heldRoom()

	a.holdRoom()
	a.mu.Lock()
	armed := a.reattach != nil
	a.mu.Unlock()
	if !armed {
		t.Fatal("nothing armed to test")
	}

	a.HandleConnect()

	a.mu.Lock()
	disarmed := a.reattach == nil
	room := a.room
	a.mu.Unlock()
	if !disarmed || room == nil {
		t.Error("a reattaching frontend should find the room held and the countdown stopped")
	}
}

// TestReattachForgetsDeclarations covers what must NOT survive the window.
// The encoders died with the page, so replaying their configs into a relay
// reconnect that lands inside the window would declare tracks nothing is
// feeding — the exact state every peer reads as a frozen tile.
func TestReattachForgetsDeclarations(t *testing.T) {
	a := newTestApp(t)
	a.room = heldRoom()
	a.declared["video"] = &bridge.TrackConfig{Kind: "video"}

	a.holdRoom()

	a.mu.Lock()
	remaining := len(a.declared)
	a.mu.Unlock()
	if remaining != 0 {
		t.Errorf("kept %d declaration(s) for encoders that no longer exist", remaining)
	}
}

// TestReattachGraceIsShorterThanAReconnect keeps the window honest against the
// two timings it sits between: the bridge's own retry, which it must outlast,
// and the relay reconnect backoff, which it should not.
func TestReattachGraceIsShorterThanAReconnect(t *testing.T) {
	const bridgeRetry = time.Second // RECONNECT_DELAY_MS in frontend/src/lib/bridge.ts
	if reattachGrace <= bridgeRetry {
		t.Errorf("reattachGrace %v does not outlast the frontend's %v retry, "+
			"so a reload would be dropped before it could come back",
			reattachGrace, bridgeRetry)
	}
	if reattachGrace > reconnectMaxDelay {
		t.Errorf("reattachGrace %v exceeds the reconnect ceiling %v; a participant "+
			"who quit would linger in every roster publishing nothing",
			reattachGrace, reconnectMaxDelay)
	}
}
