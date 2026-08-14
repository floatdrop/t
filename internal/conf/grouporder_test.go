package conf

import (
	"sync"
	"testing"
	"time"
)

// key is one object's place in the publisher's order, flattened so a test can
// compare a delivery sequence against the order it was published in.
type key struct {
	group uint64
	index uint64
}

// orderCollector records what was emitted, which is the only thing any of these
// tests is about.
type orderCollector struct {
	mu  sync.Mutex
	got []key
}

func (c *orderCollector) emitter(k key) func() {
	return func() {
		c.mu.Lock()
		c.got = append(c.got, k)
		c.mu.Unlock()
	}
}

func (c *orderCollector) order() []key {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]key(nil), c.got...)
}

// ascendingKeys reports the first position at which got stops strictly
// increasing in publisher order, or -1.
func ascendingKeys(got []key) int {
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if cur.group > prev.group {
			continue
		}
		if cur.group == prev.group && cur.index > prev.index {
			continue
		}
		return i
	}
	return -1
}

// push is the whole path one object takes: the stream is announced, then the
// object arrives.
func push(o *groupOrderer, c *orderCollector, group, index uint64) {
	k := key{group: group, index: index}
	o.Push(group, index, c.emitter(k))
}

// TestOrdererEmitsOneGroupOnArrival pins the case that is every call: one group
// at a time, its stream ordered by the transport. Nothing may be held, because
// holding here would add latency to every packet of every call.
func TestOrdererEmitsOneGroupOnArrival(t *testing.T) {
	o := newGroupOrderer()
	var c orderCollector

	o.Open(7)
	o.settleNow()
	for i := range uint64(5) {
		push(o, &c, 7, i)
		if got := c.order(); len(got) != int(i)+1 {
			t.Fatalf("the object at %d was held; nothing earlier is in flight, "+
				"so there is nothing for it to wait for", i)
		}
	}
	if held := o.backlog(); held != 0 {
		t.Errorf("backlog holds %d objects, want none", held)
	}
}

// TestOrdererHoldsAGroupAheadOfOneStillOpen is what the file exists for: two
// groups in flight, the later one draining first, delivered in publisher order.
func TestOrdererHoldsAGroupAheadOfOneStillOpen(t *testing.T) {
	o := newGroupOrderer()
	var c orderCollector

	// Both streams are in flight before either delivers, which is what a burst
	// looks like.
	o.Open(1)
	o.Open(2)
	o.settleNow()

	// Group 2 runs ahead.
	for i := range uint64(3) {
		push(o, &c, 2, i)
	}
	if got := c.order(); len(got) != 0 {
		t.Fatalf("emitted %d objects of group 2 while group 1 was still open; "+
			"they belong behind it", len(got))
	}

	for i := range uint64(3) {
		push(o, &c, 1, i)
	}
	// Group 1's stream ends, which is what releases what was waiting on it.
	o.Finish(1)

	want := []key{{1, 0}, {1, 1}, {1, 2}, {2, 0}, {2, 1}, {2, 2}}
	got := c.order()
	if len(got) != len(want) {
		t.Fatalf("emitted %d objects, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("emitted %v, want %v", got, want)
		}
	}
	if held := o.backlog(); held != 0 {
		t.Errorf("backlog holds %d objects after both groups went out", held)
	}
}

// TestOrdererGivesUpOnAGroupThatNeverArrives is the bound, and the reason there
// is one.
//
// A relay that sheds a group never opens its stream, so nothing will ever
// finish it. Waiting for that is the failure this project already had once —
// held frames sitting until the group was retired. The backlog fills, the
// orderer moves to the oldest thing it actually holds, and the call carries on.
func TestOrdererGivesUpOnAGroupThatNeverArrives(t *testing.T) {
	o := newGroupOrderer()
	var c orderCollector

	// Group 1 is announced and never delivers a thing.
	o.Open(1)
	o.Open(2)
	o.settleNow()

	for i := range uint64(maxHeldAcrossGroups + 1) {
		push(o, &c, 2, i)
	}

	got := c.order()
	if len(got) == 0 {
		t.Fatal("nothing was emitted: the backlog filled waiting for a group " +
			"that never arrives, which is the case the bound exists for")
	}
	if at := ascendingKeys(got); at != -1 {
		t.Errorf("object %d broke publisher order: %v", at, got)
	}
	if o.abandoned == 0 {
		t.Error("gave up on group 1 without recording it; a wait that expires " +
			"is worth counting")
	}
}

// TestOrdererDropsAGroupWhoseTurnHasPassed. Once a group has been given up on
// and something later has gone out, an object of it cannot be placed: emitting
// it is the inversion the orderer exists to prevent.
func TestOrdererDropsAGroupWhoseTurnHasPassed(t *testing.T) {
	o := newGroupOrderer()
	var c orderCollector

	o.Open(1)
	o.settleNow()
	push(o, &c, 1, 0)
	o.Finish(1)
	o.Open(2)
	push(o, &c, 2, 0)

	before := len(c.order())
	push(o, &c, 1, 1) // late, from a group already finished
	if got := c.order(); len(got) != before {
		t.Errorf("emitted an object of a group already passed: %v", got)
	}
	if o.passed != 1 {
		t.Errorf("passed = %d, want 1", o.passed)
	}
}

// TestOrdererTakesTheLowestGroupOpened is why Open exists rather than the mark
// being taken from the first object.
//
// The streams of a burst are read on separate goroutines, so the first object
// decoded need not belong to the first group. A mark taken from it would put
// every earlier group below the line and drop it — whole seconds of sound.
func TestOrdererTakesTheLowestGroupOpened(t *testing.T) {
	o := newGroupOrderer()
	var c orderCollector

	// The later stream is announced first, which is the race the settle window
	// exists for: both are announced before the window closes, so the mark ends
	// up on the earlier one however they arrived.
	o.Open(9)
	o.Open(8)
	o.settleNow()

	push(o, &c, 9, 0)
	if got := c.order(); len(got) != 0 {
		t.Fatalf("emitted group 9 while group 8 was open: %v", got)
	}
	push(o, &c, 8, 0)
	o.Finish(8)

	want := []key{{8, 0}, {9, 0}}
	got := c.order()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("emitted %v, want %v", got, want)
	}
}

// TestOrdererWillNotRewindAfterEmitting. A stream opening for an earlier group
// once the frontend has already been handed a later one is genuinely late, and
// lowering the mark then would deliver a group behind one already played.
func TestOrdererWillNotRewindAfterEmitting(t *testing.T) {
	o := newGroupOrderer()
	var c orderCollector

	o.Open(5)
	o.settleNow()
	push(o, &c, 5, 0)

	o.Open(4) // late, and after group 5 has already gone out
	push(o, &c, 4, 0)

	got := c.order()
	if len(got) != 1 || got[0] != (key{5, 0}) {
		t.Errorf("emitted %v; group 4 arrived after group 5 was delivered and "+
			"cannot be put in front of it", got)
	}
}

// TestOrdererConcurrentStreamsStayOrdered runs the two streams the way the
// reader does, on their own goroutines, and requires the result to be in
// publisher order however they interleave. Run this one with -race.
func TestOrdererConcurrentStreamsStayOrdered(t *testing.T) {
	const objects = 20
	for range 50 {
		o := newGroupOrderer()
		var c orderCollector

		o.Open(1)
		o.Open(2)
		o.settleNow()

		var wg sync.WaitGroup
		for _, group := range []uint64{1, 2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range uint64(objects) {
					push(o, &c, group, i)
				}
				o.Finish(group)
			}()
		}
		wg.Wait()

		got := c.order()
		if len(got) != 2*objects {
			t.Fatalf("emitted %d objects, want %d", len(got), 2*objects)
		}
		if at := ascendingKeys(got); at != -1 {
			t.Fatalf("object %d broke publisher order: %v", at, got)
		}
	}
}

// TestOrdererDropForgetsTheBacklog. What is held when a track goes away is
// audio waiting on a group that is no longer coming, for a decoder that is
// being retired in the next breath.
func TestOrdererDropForgetsTheBacklog(t *testing.T) {
	o := newGroupOrderer()
	var c orderCollector

	o.Open(1)
	o.Open(2)
	o.settleNow()
	push(o, &c, 2, 0)
	if o.backlog() != 1 {
		t.Fatalf("backlog holds %d, want 1", o.backlog())
	}

	o.Drop()
	if held := o.backlog(); held != 0 {
		t.Errorf("backlog holds %d after Drop, want none", held)
	}
	if got := c.order(); len(got) != 0 {
		t.Errorf("Drop emitted %v; nothing should go out on the way down", got)
	}
}

// TestOrdererKeepsAGroupWhoseReaderIsSlow is the case that got past a bound
// sized at one group's worth, and cost a whole group every time a burst landed.
//
// A burst puts several groups in flight and each is read on its own goroutine,
// so a group can be behind by however long its goroutine waits to be scheduled —
// nothing to do with the data, and on a loaded machine long enough for two later
// groups to arrive whole. Giving up then throws away a group that was about to
// be delivered intact, which is worse than the inversion the orderer exists to
// prevent: the frames were on the wire and the app discarded them.
//
// Two later groups is what CI produced; the bound has to sit above that and
// above anything else the transport legitimately does.
func TestOrdererKeepsAGroupWhoseReaderIsSlow(t *testing.T) {
	o := newGroupOrderer()
	var c orderCollector

	// All three streams arrive; only the first one's reader has not run yet.
	o.Open(0)
	o.Open(1)
	o.Open(2)
	o.settleNow()

	const perGroup = audioGroupObjects
	for _, group := range []uint64{1, 2} {
		for i := range uint64(perGroup) {
			push(o, &c, group, i)
		}
		o.Finish(group)
	}
	if got := c.order(); len(got) != 0 {
		t.Fatalf("emitted %d objects while group 0 was still open; they belong "+
			"behind it", len(got))
	}

	// Group 0's reader finally runs.
	for i := range uint64(perGroup) {
		push(o, &c, 0, i)
	}
	o.Finish(0)

	got := c.order()
	if len(got) != 3*perGroup {
		t.Fatalf("emitted %d of %d objects; a group whose reader was merely late "+
			"must not be given up on", len(got), 3*perGroup)
	}
	if at := ascendingKeys(got); at != -1 {
		t.Errorf("object %d broke publisher order: %v", at, got[:at+1])
	}
	if o.passed != 0 {
		t.Errorf("dropped %d objects as already passed, want none", o.passed)
	}
	if o.abandoned != 0 {
		t.Errorf("gave up on %d groups, want none", o.abandoned)
	}
}

// TestOrdererSettlesBeforeTheFirstDelivery is the race the settle window exists
// for, and it cost a whole group in the released build.
//
// Open runs on the accept loop, in the order the transport delivered the
// streams, but the readers run on their own goroutines — so a reader can push
// its first object before the accept loop has announced a stream that arrived
// just behind it. Once anything has gone out the mark cannot be lowered, because
// a group delivered behind one already played is the inversion all of this
// exists to prevent, and the earlier group is dropped entirely. Measured at one
// run in eight over a three-group burst, silently losing a third of the audio.
//
// Nothing may go out until the window closes, which is what keeps the mark free
// to move down to the group that turns up a moment later.
func TestOrdererSettlesBeforeTheFirstDelivery(t *testing.T) {
	o := newGroupOrderer()
	var c orderCollector

	// The later stream is accepted first and its reader gets there before the
	// earlier stream has been announced at all.
	o.Open(1)
	push(o, &c, 1, 0)
	push(o, &c, 1, 1)
	if got := c.order(); len(got) != 0 {
		t.Fatalf("delivered %v before the settle window closed; the mark is not "+
			"final yet and delivering is what would freeze it", got)
	}

	// Group 0's stream lands inside the window, which is the whole point.
	o.Open(0)
	push(o, &c, 0, 0)
	o.Finish(0)
	o.settleNow()

	want := []key{{0, 0}, {1, 0}, {1, 1}}
	got := c.order()
	if len(got) != len(want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivered %v, want %v", got, want)
		}
	}
	if o.passed != 0 {
		t.Errorf("dropped %d objects as already passed, want none", o.passed)
	}
}

// TestOrdererSettleWindowExpiresOnItsOwn. The tests above close the window by
// hand to stay deterministic, which would hide a settle that never ends — and a
// settle that never ends is a track that never delivers anything at all.
func TestOrdererSettleWindowExpiresOnItsOwn(t *testing.T) {
	o := newGroupOrderer()
	var c orderCollector

	o.Open(4)
	push(o, &c, 4, 0)

	deadline := time.Now().Add(5 * time.Second)
	for len(c.order()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.order(); len(got) != 1 || got[0] != (key{4, 0}) {
		t.Fatalf("delivered %v after the settle window should have expired, want "+
			"the one object pushed into it", got)
	}
}

// TestOrdererDropStopsTheSettle. A settle firing against a track that has gone
// emits into a decoder being retired, and holds the sink alive to do it.
func TestOrdererDropStopsTheSettle(t *testing.T) {
	o := newGroupOrderer()
	var c orderCollector

	o.Open(2)
	push(o, &c, 2, 0)
	o.Drop()

	time.Sleep(2 * settleWindow)
	if got := c.order(); len(got) != 0 {
		t.Errorf("delivered %v after Drop; nothing should go out on the way down", got)
	}
}
