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
	g := newGroupReassembler()
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

	// Everything goes out, and nothing waits: each is in turn the index the
	// group was waiting for, which is what emission-ordered indices buy over
	// ordering on a timestamp.
	if got, want := c.order(), []uint64{0, 1, 2, 3}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}

	// 6 skips ahead of 4 and 5, so it must wait: subgroup 1 has only reached 3
	// and could still deliver 5.
	g.Push(0, 6, c.emitter(6))
	if got, want := c.order(), []uint64{0, 1, 2, 3}; !equal(got, want) {
		t.Fatalf("order = %v, want %v — 6 was released while subgroup 1 could "+
			"still deliver 5", got, want)
	}

	// Once the enhancement layer ends, nothing can precede 6 any more.
	g.CloseSubgroup(1)
	if got, want := c.order(), []uint64{0, 1, 2, 3, 6}; !equal(got, want) {
		t.Fatalf("after closing the enhancement layer, order = %v, want %v", got, want)
	}
}

// TestReassemblerEmitsImmediatelyWithOneSubgroup pins that the ordering
// machinery costs nothing in the shape every group had before temporal layers:
// a single subgroup, already ordered by the transport. Nothing may be held
// back, because holding here would add latency to every frame of every call —
// audio included.
func TestReassemblerEmitsImmediatelyWithOneSubgroup(t *testing.T) {
	g := newGroupReassembler()
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

// A layer the relay sheds simply never opens its stream, and the group must not
// wait for it. This is the shape that froze the picture: with the enhancement
// layer gone the base arrives 0, 2, 4, only the first of which is the index
// being waited for, so a group that held the rest until something else retired
// it showed four seconds of nothing and then four seconds at once.
//
// The cost of not waiting is one group's enhancement layer when a burst
// delivers the base first — a GOP at half the frame rate, against a frozen tile.
func TestReassemblerDoesNotHoldForALayerThatNeverArrives(t *testing.T) {
	g := newGroupReassembler()
	var c collector
	g.OpenSubgroup(0)

	for _, index := range []uint64{0, 2, 4, 6} {
		g.Push(0, index, c.emitter(index))
	}
	if got, want := c.order(), []uint64{0, 2, 4, 6}; !equal(got, want) {
		t.Fatalf("order = %v, want %v — the base layer must go out on arrival "+
			"when the layer above it is not being delivered", got, want)
	}
}

// TestReassemblerDoesNotWaitOnAShedLayer is the pathological case a
// timeout-based design would fail. When a relay sheds the enhancement layer,
// its stream ends; from then on every base-layer frame must go out on arrival.
// A design that waited for the missing odd frames would pay that wait once per
// frame, turning a shed layer into a stall on a link already struggling.
func TestReassemblerDoesNotWaitOnAShedLayer(t *testing.T) {
	g := newGroupReassembler()
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
	g := newGroupReassembler()
	var c collector
	g.OpenSubgroup(0)
	g.OpenSubgroup(1)

	g.Push(0, 0, c.emitter(0))
	// The frame at 1 is dropped in flight. The one at 3 arriving proves it is
	// not coming: subgroup 1 is ordered, so it can never go back before 3.
	g.Push(1, 3, c.emitter(3))
	g.Push(0, 2, c.emitter(2))

	if got, want := c.order(), []uint64{0, 2, 3}; !equal(got, want) {
		t.Fatalf("order = %v, want %v — a dropped frame must not stall the group", got, want)
	}
}

// TestReassemblerHoldsForAnOpenButSilentSubgroup pins the other half of that
// rule. A stream that has opened and delivered nothing could still produce the
// earliest frame of all, so anything after it must wait — a subgroup is not
// "finished" merely because a sibling has run ahead of it.
func TestReassemblerHoldsForAnOpenButSilentSubgroup(t *testing.T) {
	g := newGroupReassembler()
	var c collector
	g.OpenSubgroup(0)
	g.OpenSubgroup(1)

	g.Push(1, 1, c.emitter(1))
	if got := c.order(); len(got) != 0 {
		t.Fatalf("emitted %v while subgroup 0 was open and silent; its first frame "+
			"could still come before that one", got)
	}
	g.Push(0, 0, c.emitter(0))
	if got, want := c.order(), []uint64{0, 1}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestReassemblerBoundsTheBacklog pins the release valve. A publisher that
// stalls mid-group leaves its stream open forever, and everything behind the
// frame it owes piles up. Past maxHeldObjects the group gives up waiting rather
// than growing without limit.
func TestReassemblerBoundsTheBacklog(t *testing.T) {
	g := newGroupReassembler()
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

// TestReassemblerDropsLateAndDuplicateFrames covers the §9.5
// redundant-publisher case, where the same object can reach us twice, and the
// frame that arrives after the group has already moved past it. Emitting either
// would hand a decoder something it is already beyond.
func TestReassemblerDropsLateAndDuplicateFrames(t *testing.T) {
	g := newGroupReassembler()
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
	g := newGroupReassembler()
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
	g := newGroupReassembler()
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

// Guards the assumption every test above is written against: the publisher's
// emission index is the ordering key, and it counts across the subgroups of a
// group. Object IDs cannot serve — each subgroup numbers from its own base, so
// the enhancement layer's first object and the base layer's first object are
// both "object 0" of their own stream while sitting a frame apart. Stated as a
// test so a change to the layout trips something rather than silently
// reordering video.
func TestReassemblerOrdersOnEmissionIndex(t *testing.T) {
	g := newGroupReassembler()
	var c collector
	g.OpenSubgroup(0)
	g.OpenSubgroup(1)

	// L1T2 emission: T0, T1, T0, T1... indexed 0,1,2,3 in that order, split
	// across subgroups by layer and here delivered layer by layer rather than
	// in step.
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
		t.Fatalf("order = %v, want %v — emission order and index order must "+
			"agree, or the layout has drifted from what reassembly keys on", got, want)
	}
}

// The keyframe must not be conceded, and the streams of a group race to say so.
//
// Each subgroup is read on its own goroutine with no ordering between them, so
// the enhancement layer's reader can reach its first Push before the base
// layer's reader has registered its subgroup at all. The group then knows about
// one stream, that stream is past index 1, and the rule that concedes a gap
// once every open stream has moved past it would let index 1 out with nothing
// emitted yet — after which the keyframe arrives below next and is dropped as
// late.
//
// What comes of that is not a degraded picture but an undecodable one: a group
// of deltas with no keyframe to start on. Nothing precedes a group's first
// object, so nothing may go out before it has arrived.
func TestReassemblerNeverConcedesTheKeyFrame(t *testing.T) {
	g := newGroupReassembler()
	var c collector

	// The enhancement layer's reader wins the race: its subgroup is the only
	// one this group has heard of when its first object lands.
	g.OpenSubgroup(1)
	g.Push(1, 1, c.emitter(1))
	if got := c.order(); len(got) != 0 {
		t.Fatalf("emitted %v before the group's first object arrived; the "+
			"keyframe is index 0 and a decoder cannot start without it", got)
	}

	// The base layer's reader catches up.
	g.OpenSubgroup(0)
	g.Push(0, 0, c.emitter(0))
	if got, want := c.order(), []uint64{0, 1}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// A subscriber that joins mid-group never receives index 0, and must not wait
// for it for ever. The backlog valve is what lets go — the frames it releases
// are ones the decoder will refuse anyway, having no keyframe, so the cost is
// a quarter of a second of nothing.
func TestReassemblerGivesUpOnAFirstObjectThatIsNotComing(t *testing.T) {
	g := newGroupReassembler()
	var c collector
	g.OpenSubgroup(0)

	for i := range uint64(maxHeldObjects + 2) {
		g.Push(0, i+4, c.emitter(i+4)) // joined at index 4; 0..3 never arrive
	}
	got := c.order()
	if len(got) == 0 {
		t.Fatal("nothing was emitted: a group joined part-way through waited " +
			"for a first object that was never going to arrive")
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("emitted out of order at %d: %v", i, got[:i+1])
		}
	}
}
