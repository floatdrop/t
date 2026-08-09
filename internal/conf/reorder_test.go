package conf

import (
	"sync"
	"testing"
)

// collector records the order objects were emitted in, which is the only thing
// any of these tests is about.
type collector struct {
	mu  sync.Mutex
	got []uint64
}

func (c *collector) emitter(ts uint64) func() {
	return func() {
		c.mu.Lock()
		c.got = append(c.got, ts)
		c.mu.Unlock()
	}
}

func (c *collector) order() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]uint64(nil), c.got...)
}

func equal(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReassemblerOrdersAcrossSubgroups is the case the whole file exists for:
// two temporal layers on two subgroups, arriving in the wrong order, emitted in
// the right one. Subgroup 0 carries the even timestamps and subgroup 1 the odd
// ones — the L1T2 layout — and here subgroup 1 runs ahead.
func TestReassemblerOrdersAcrossSubgroups(t *testing.T) {
	g := newGroupReassembler(2)
	var c collector
	g.OpenSubgroup(0)
	g.OpenSubgroup(1)

	// The enhancement layer arrives first, and must wait: the frame at 1 cannot
	// be decoded before the frame at 0, whatever order the transport delivered
	// them in.
	g.Push(1, 1, c.emitter(1))
	g.Push(1, 3, c.emitter(3))
	if got := c.order(); len(got) != 0 {
		t.Fatalf("emitted %v before the base layer arrived; a delta frame ahead of "+
			"the frame it references is undecodable, not merely early", got)
	}

	g.Push(0, 0, c.emitter(0))
	g.Push(0, 2, c.emitter(2))

	// Everything up to 2 goes out. Not 3: subgroup 0 has only reached 2, so it
	// could still deliver something between the two — and one frame in hand is
	// what ordering two live streams costs.
	if got, want := c.order(), []uint64{0, 1, 2}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}

	// Once the base layer moves past it, nothing can precede 3 any more — and
	// 4 takes its place as the one frame in hand.
	g.Push(0, 4, c.emitter(4))
	if got, want := c.order(), []uint64{0, 1, 2, 3}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestReassemblerEmitsImmediatelyWithOneSubgroup pins that the ordering
// machinery costs nothing in the shape every group had before temporal layers:
// a single subgroup, already ordered by the transport. Nothing may be held
// back, because holding here would add latency to every frame of every call —
// audio included.
func TestReassemblerEmitsImmediatelyWithOneSubgroup(t *testing.T) {
	g := newGroupReassembler(1)
	var c collector
	g.OpenSubgroup(0)

	for i := range uint64(5) {
		g.Push(0, i, c.emitter(i))
		if got := c.order(); len(got) != int(i)+1 {
			t.Fatalf("the frame at %d was held; with one open subgroup nothing can "+
				"precede an arriving object, so it is releasable on arrival", i)
		}
	}
}

// A group must wait for a layer the publisher declared and the transport has
// not delivered yet, even when every stream it has actually seen has gone
// quiet. This is the burst: the relay drains one subgroup well ahead of the
// other, and a group that judged safety by what it had seen would release the
// base layer entire and then have nowhere to put the enhancement layer.
func TestReassemblerWaitsForADeclaredLayerItHasNotSeen(t *testing.T) {
	g := newGroupReassembler(2)
	var c collector
	g.OpenSubgroup(0)

	// The whole base layer arrives before the enhancement layer's stream shows
	// up at all.
	for _, ts := range []uint64{0, 2, 4, 6} {
		g.Push(0, ts, c.emitter(ts))
	}
	if got := c.order(); len(got) != 0 {
		t.Fatalf("emitted %v while a declared layer had not been heard from; "+
			"until that stream says something, any frame it holds could belong "+
			"in front of what has arrived — including in front of the first", got)
	}

	g.OpenSubgroup(1)
	for _, ts := range []uint64{1, 3, 5} {
		g.Push(1, ts, c.emitter(ts))
	}
	if got, want := c.order(), []uint64{0, 1, 2, 3, 4, 5}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestReassemblerDoesNotWaitOnAShedLayer is the pathological case a
// timeout-based design would fail. When a relay sheds the enhancement layer,
// its stream ends; from then on every base-layer frame must go out on arrival.
// A design that waited for the missing odd frames would pay that wait once per
// frame, turning a shed layer into a stall on a link already struggling.
func TestReassemblerDoesNotWaitOnAShedLayer(t *testing.T) {
	g := newGroupReassembler(2)
	var c collector
	g.OpenSubgroup(0)
	g.OpenSubgroup(1)

	g.Push(0, 0, c.emitter(0))
	g.Push(1, 1, c.emitter(1))
	// The relay gives up on the enhancement layer part way through the group.
	g.CloseSubgroup(1)

	for _, ts := range []uint64{2, 4, 6} {
		g.Push(0, ts, c.emitter(ts))
	}
	if got, want := c.order(), []uint64{0, 1, 2, 4, 6}; !equal(got, want) {
		t.Fatalf("order = %v, want %v — the base layer must not wait on a subgroup "+
			"that has ended", got, want)
	}
}

// TestReassemblerReleasesOverAGap covers a hole the relay punched mid-group:
// the frame simply never arrives. Every open stream moving past it is what
// proves nothing earlier is coming, so the run continues without it and without
// a timer.
func TestReassemblerReleasesOverAGap(t *testing.T) {
	g := newGroupReassembler(2)
	var c collector
	g.OpenSubgroup(0)
	g.OpenSubgroup(1)

	g.Push(0, 0, c.emitter(0))
	// The frame at 1 is dropped in flight. The one at 3 arriving proves it is
	// not coming: subgroup 1 is ordered, so it can never go back before 3.
	g.Push(1, 3, c.emitter(3))
	g.Push(0, 2, c.emitter(2))

	if got, want := c.order(), []uint64{0, 2}; !equal(got, want) {
		t.Fatalf("order = %v, want %v — a dropped frame must not stall the group", got, want)
	}
}

// TestReassemblerHoldsForAnOpenButSilentSubgroup pins the other half of that
// rule. A stream that has opened and delivered nothing could still produce the
// earliest frame of all, so anything after it must wait — a subgroup is not
// "finished" merely because a sibling has run ahead of it.
func TestReassemblerHoldsForAnOpenButSilentSubgroup(t *testing.T) {
	g := newGroupReassembler(2)
	var c collector
	g.OpenSubgroup(0)
	g.OpenSubgroup(1)

	g.Push(1, 1, c.emitter(1))
	if got := c.order(); len(got) != 0 {
		t.Fatalf("emitted %v while subgroup 0 was open and silent; its first frame "+
			"could still come before that one", got)
	}
	// 0 goes out; 1 takes its place in hand, because subgroup 0 has still only
	// reached 0 and could deliver something between the two.
	g.Push(0, 0, c.emitter(0))
	if got, want := c.order(), []uint64{0}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	g.Push(0, 2, c.emitter(2))
	if got, want := c.order(), []uint64{0, 1}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestReassemblerBoundsTheBacklog pins the release valve. A publisher that
// stalls mid-group leaves its stream open forever, and everything behind the
// frame it owes piles up. Past maxHeldObjects the group gives up waiting rather
// than growing without limit.
func TestReassemblerBoundsTheBacklog(t *testing.T) {
	g := newGroupReassembler(2)
	var c collector
	g.OpenSubgroup(0)
	g.OpenSubgroup(1)

	// Subgroup 0 never delivers anything; subgroup 1 keeps going.
	for i := range uint64(maxHeldObjects + 4) {
		g.Push(1, i+1, c.emitter(i+1))
	}
	got := c.order()
	if len(got) == 0 {
		t.Fatal("nothing was emitted: a stalled publisher held the group forever")
	}
	if got[0] != 1 {
		t.Fatalf("first emitted = %d, want 1 (the oldest held frame)", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("emitted out of order at %d: %v", i, got[:i+1])
		}
	}
}

// TestReassemblerDropsLateAndDuplicateFrames covers the §9.5 redundant-publisher
// case, where the same object can reach us twice, and the frame that arrives
// after the group has already moved past it. Emitting either would hand a
// decoder something it is already beyond.
func TestReassemblerDropsLateAndDuplicateFrames(t *testing.T) {
	g := newGroupReassembler(1)
	var c collector
	g.OpenSubgroup(0)

	g.Push(0, 0, c.emitter(0))
	g.Push(0, 1, c.emitter(1))
	g.Push(0, 0, c.emitter(0)) // replay of a frame already emitted
	g.Push(0, 2, c.emitter(2))

	if got, want := c.order(), []uint64{0, 1, 2}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestReassemblerFlushEmitsTheBacklog pins that ending a group does not
// silently discard what is still held: frames already received and decodable
// should be painted, not dropped because a sibling stream never closed.
func TestReassemblerFlushEmitsTheBacklog(t *testing.T) {
	g := newGroupReassembler(2)
	var c collector
	g.OpenSubgroup(0)
	g.OpenSubgroup(1)

	g.Push(1, 1, c.emitter(1))
	g.Push(1, 3, c.emitter(3))
	g.Flush()

	if got, want := c.order(), []uint64{1, 3}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestReassemblerConcurrentPushesStayOrdered runs the real shape: one goroutine
// per subgroup stream, pushing at once, as the router does. Whatever the
// interleaving, the emitted sequence must be ascending and complete — the emits
// happen under the lock precisely so two readers cannot interleave their output
// and undo the ordering. Run this one with -race.
//
// Deliberately below maxHeldObjects, so that one goroutine winning the race
// outright still leaves the backlog inside its bound. Above it the valve fires
// and frames are dropped by design — which is worth testing, and is what
// TestReassemblerBoundsTheBacklog does; asserting completeness there as well
// would only be asserting that the scheduler stayed fair.
func TestReassemblerConcurrentPushesStayOrdered(t *testing.T) {
	const perLayer = maxHeldObjects / 2
	g := newGroupReassembler(2)
	var c collector
	g.OpenSubgroup(0)
	g.OpenSubgroup(1)

	var wg sync.WaitGroup
	for layer := range uint64(2) {
		wg.Go(func() {
			for i := range uint64(perLayer) {
				ts := i*2 + layer
				g.Push(layer, ts, c.emitter(ts))
			}
			g.CloseSubgroup(layer)
		})
	}
	wg.Wait()
	g.Flush()

	got := c.order()
	if len(got) != perLayer*2 {
		t.Fatalf("emitted %d frames, want %d", len(got), perLayer*2)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("emitted out of order at %d: ...%v", i, got[max(0, i-4):i+1])
		}
	}
}

// Guards the assumption every test above is written against: the timestamp is
// the ordering key, and it is the only one available. Object IDs cannot be —
// each subgroup numbers from its own base, so the enhancement layer's first
// object and the base layer's first object are both "object 0" of their stream
// while sitting a frame apart in time. Stated as a test so a change to the
// layout trips something rather than silently reordering video.
func TestReassemblerOrdersOnTimestampsNotArrival(t *testing.T) {
	g := newGroupReassembler(2)
	var c collector
	g.OpenSubgroup(0)
	g.OpenSubgroup(1)

	// L1T2 emission: T0, T1, T0, T1... at 0,1,2,3 in time, split across
	// subgroups by layer and here delivered layer by layer rather than in step.
	for _, ts := range []uint64{0, 2, 4, 6} {
		g.Push(0, ts, c.emitter(ts))
	}
	for _, ts := range []uint64{1, 3, 5, 7} {
		g.Push(1, ts, c.emitter(ts))
	}
	g.CloseSubgroup(0)
	g.CloseSubgroup(1)

	want := make([]uint64, 8)
	for i := range want {
		want[i] = uint64(i)
	}
	if got := c.order(); !equal(got, want) {
		t.Fatalf("order = %v, want %v — emission order and timestamp order must "+
			"agree, or the layout has drifted from what reassembly keys on", got, want)
	}
}
