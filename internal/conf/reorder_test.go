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

func (c *collector) emitter(index uint64) func() {
	return func() {
		c.mu.Lock()
		c.got = append(c.got, index)
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

// ascending reports the first index at which got stops strictly increasing,
// or -1. Emitting the same index twice fails it too, which is the other way a
// decoder gets something it is already past.
func ascending(got []uint64) int {
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			return i
		}
	}
	return -1
}

// TestReassemblerEmitsTheBaseLayerOnArrival pins the shape every group had
// before temporal layers, and the shape audio still has: a single subgroup,
// already ordered by the transport. Nothing may be held, because holding here
// would add latency to every frame of every call.
func TestReassemblerEmitsTheBaseLayerOnArrival(t *testing.T) {
	g := newGroupReassembler()
	var c collector

	for i := range uint64(5) {
		g.Push(baseSubgroup, i, c.emitter(i))
		if got := c.order(); len(got) != int(i)+1 {
			t.Fatalf("the frame at %d was held; a base-layer object arrives on an "+
				"ordered stream, so nothing of its own layer can precede it", i)
		}
	}
	if held := g.backlog(); held != 0 {
		t.Errorf("backlog holds %d objects, want none", held)
	}
}

// TestReassemblerPlacesTheEnhancementLayer is the case the whole file exists
// for: two temporal layers on two subgroups, arriving in the wrong order,
// emitted in the right one. Subgroup 0 carries the even indices and subgroup 1
// the odd ones — the L1T2 layout — and here subgroup 1 runs ahead.
func TestReassemblerPlacesTheEnhancementLayer(t *testing.T) {
	g := newGroupReassembler()
	var c collector

	// The enhancement layer arrives first, and must wait: the frame at 1 cannot
	// be decoded before the frame at 0, whatever order the transport delivered
	// them in.
	g.Push(1, 1, c.emitter(1))
	g.Push(1, 3, c.emitter(3))
	if got := c.order(); len(got) != 0 {
		t.Fatalf("emitted %v before the base layer arrived; a delta frame ahead of "+
			"the frame it references is undecodable, not merely early", got)
	}

	g.Push(baseSubgroup, 0, c.emitter(0))
	g.Push(baseSubgroup, 2, c.emitter(2))
	g.Push(baseSubgroup, 4, c.emitter(4))

	// Each enhancement frame goes out in front of the base frame that follows
	// it, which is what proves nothing earlier is still to come.
	if got, want := c.order(), []uint64{0, 1, 2, 3, 4}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestReassemblerNeverHoldsTheBaseLayer is the freeze this design exists to
// make impossible. A relay shedding the enhancement layer — which is the whole
// point of publishing it separately — simply never opens that stream, so the
// base layer arrives 0, 2, 4 with nothing between. A design that waited for the
// odd indices showed four seconds of frozen tile followed by four seconds of
// video at once, on any link that made the relay shed.
func TestReassemblerNeverHoldsTheBaseLayer(t *testing.T) {
	g := newGroupReassembler()
	var c collector

	for _, index := range []uint64{0, 2, 4, 6} {
		g.Push(baseSubgroup, index, c.emitter(index))
	}
	if got, want := c.order(), []uint64{0, 2, 4, 6}; !equal(got, want) {
		t.Fatalf("order = %v, want %v — the base layer must go out on arrival "+
			"when the layer above it is not being delivered", got, want)
	}
}

// TestReassemblerKeepsTheBaseLayerWholeUnderAnyBacklog is the regression this
// file was rewritten for.
//
// The enhancement layer arrives in full before the base layer opens at all,
// which is what a backfilled group or a publisher catching up after a stall
// looks like — the relay drains one subgroup nearly to the end before opening
// the next. Far more than the backlog can hold, so the bound is reached and
// objects are given up on.
//
// Whatever is given up must be enhancement frames and never a base frame. The
// design that ordered both layers through one bounded backlog conceded the head
// of the queue instead, which advanced past a base object still travelling
// intact on its own ordered stream; it then arrived below the mark and was
// dropped as late, and every frame referencing it went to the decoder anyway.
// A whole GOP of macroblock garbage, from the app discarding a frame the
// network had delivered.
func TestReassemblerKeepsTheBaseLayerWholeUnderAnyBacklog(t *testing.T) {
	const frames = maxHeldEnhancement * 4
	g := newGroupReassembler()
	var c collector

	for i := range uint64(frames) {
		g.Push(1, i*2+1, c.emitter(i*2+1))
	}
	if held := g.backlog(); held > maxHeldEnhancement {
		t.Fatalf("backlog grew to %d, past the bound of %d", held, maxHeldEnhancement)
	}
	for i := range uint64(frames) {
		g.Push(baseSubgroup, i*2, c.emitter(i*2))
	}

	got := c.order()
	if at := ascending(got); at >= 0 {
		t.Fatalf("emitted out of order at %d: %v", at, got[max(0, at-4):at+1])
	}
	seen := make(map[uint64]bool, len(got))
	for _, index := range got {
		seen[index] = true
	}
	for i := range uint64(frames) {
		if !seen[i*2] {
			t.Fatalf("base-layer frame %d was never emitted; every frame after it "+
				"in the group references it and is garbage without it", i*2)
		}
	}
}

// TestReassemblerPlacesALayerDeliveredWhole covers the same burst inside the
// bound, where nothing has to be given up: a whole enhancement layer arriving
// before its base layer is still placed frame by frame. This is what the
// backlog buys, and why it is not simply zero.
func TestReassemblerPlacesALayerDeliveredWhole(t *testing.T) {
	const frames = maxHeldEnhancement
	g := newGroupReassembler()
	var c collector

	for i := range uint64(frames) {
		g.Push(1, i*2+1, c.emitter(i*2+1))
	}
	for i := range uint64(frames) {
		g.Push(baseSubgroup, i*2, c.emitter(i*2))
	}

	// Everything but the tail: the last enhancement frame has no base frame
	// after it to prove its turn has come, and the group is retired holding it.
	want := make([]uint64, 0, frames*2-1)
	for i := range uint64(frames*2 - 1) {
		want = append(want, i)
	}
	if got := c.order(); !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestReassemblerDropsAnOvertakenEnhancementFrame pins the cost side of the
// trade. An enhancement frame that arrives after the base frame it belongs in
// front of has missed its turn, and is dropped rather than emitted late —
// nothing references it, so it costs exactly itself, where holding the base
// layer for it would cost the picture.
func TestReassemblerDropsAnOvertakenEnhancementFrame(t *testing.T) {
	g := newGroupReassembler()
	var c collector

	g.Push(baseSubgroup, 0, c.emitter(0))
	g.Push(baseSubgroup, 2, c.emitter(2))
	g.Push(1, 1, c.emitter(1)) // overtaken by the frame at 2
	g.Push(baseSubgroup, 4, c.emitter(4))

	if got, want := c.order(), []uint64{0, 2, 4}; !equal(got, want) {
		t.Fatalf("order = %v, want %v — an enhancement frame past its turn must "+
			"not be handed to a decoder that has moved on", got, want)
	}
}

// TestReassemblerDropsLateAndDuplicateFrames covers the §9.5
// redundant-publisher case, where the same object can reach us twice.
func TestReassemblerDropsLateAndDuplicateFrames(t *testing.T) {
	g := newGroupReassembler()
	var c collector

	g.Push(baseSubgroup, 0, c.emitter(0))
	g.Push(baseSubgroup, 2, c.emitter(2))
	g.Push(baseSubgroup, 0, c.emitter(0)) // replay of a frame already emitted
	g.Push(baseSubgroup, 4, c.emitter(4))

	if got, want := c.order(), []uint64{0, 2, 4}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestReassemblerBoundsTheEnhancementBacklog pins the memory bound on a base
// layer that has stalled: its stream stays open owing an object, and everything
// above piles up behind it.
func TestReassemblerBoundsTheEnhancementBacklog(t *testing.T) {
	g := newGroupReassembler()
	var c collector

	// The frame at 1 is the next index due, so it goes out; from 3 on the base
	// layer owes an object and everything above it piles up.
	g.Push(baseSubgroup, 0, c.emitter(0))
	for i := range uint64(maxHeldEnhancement * 10) {
		g.Push(1, i*2+1, c.emitter(i*2+1))
	}
	if held := g.backlog(); held > maxHeldEnhancement {
		t.Errorf("backlog grew to %d, past the bound of %d", held, maxHeldEnhancement)
	}
	if got, want := c.order(), []uint64{0, 1}; !equal(got, want) {
		t.Fatalf("order = %v, want %v — nothing above a stalled base layer may go "+
			"out, whatever the backlog does", got, want)
	}
}

// TestReassemblerHoldsNothingInTheSteadyState pins the case that matters most
// for latency, because it is nearly every frame of every call: the two layers
// arriving in step, in emission order. Each object is the next index due when it
// lands, so none of the reordering machinery engages and nothing waits.
func TestReassemblerHoldsNothingInTheSteadyState(t *testing.T) {
	g := newGroupReassembler()
	var c collector

	for i := range uint64(30) {
		g.Push(i%2, i, c.emitter(i))
		if got := c.order(); len(got) != int(i)+1 {
			t.Fatalf("the frame at %d was held; in emission order every object is "+
				"the one due, and holding it would add latency to every call", i)
		}
	}
	if held := g.backlog(); held != 0 {
		t.Errorf("backlog holds %d objects, want none", held)
	}
}

// TestReassemblerNeedsNoKeyFrameForItsOwnSake pins that a group opening on an
// enhancement object needs no special case.
//
// Each subgroup is read on its own goroutine with no ordering between them, so
// the enhancement layer's reader can reach its first Push before the base
// layer's has produced anything. The earlier design raced here — one open
// stream past index 1 was enough for the gap rule to concede index 1, after
// which the keyframe arrived below the mark and was dropped as late, leaving a
// group of deltas with no keyframe to start on. Nothing is conceded now, so
// there is no state for the two readers to race on.
func TestReassemblerNeedsNoKeyFrameForItsOwnSake(t *testing.T) {
	g := newGroupReassembler()
	var c collector

	g.Push(1, 1, c.emitter(1))
	if got := c.order(); len(got) != 0 {
		t.Fatalf("emitted %v before the group's first object arrived; the "+
			"keyframe is index 0 and a decoder cannot start without it", got)
	}
	g.Push(baseSubgroup, 0, c.emitter(0))
	g.Push(baseSubgroup, 2, c.emitter(2))
	if got, want := c.order(), []uint64{0, 1, 2}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// A subscriber that joins mid-group never receives index 0. Its base layer
// still flows from wherever it joined — the frontend gates on the first
// keyframe, so what it does with undecodable frames is settled there and does
// not need a second answer here.
func TestReassemblerRunsAGroupJoinedPartWayThrough(t *testing.T) {
	g := newGroupReassembler()
	var c collector

	for _, index := range []uint64{4, 6, 8} {
		g.Push(baseSubgroup, index, c.emitter(index))
	}
	if got, want := c.order(), []uint64{4, 6, 8}; !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestReassemblerConcurrentPushesStayOrdered runs the real shape: one goroutine
// per subgroup stream, pushing at once, as the router does. Whatever the
// interleaving, the emitted sequence must ascend and the base layer must be
// whole — the emits happen under the lock precisely so two readers cannot
// interleave their output and undo the ordering. Run this one with -race.
//
// Enhancement frames are not asserted: which of them the base layer overtakes
// is the scheduler's business, and dropping them is the design.
func TestReassemblerConcurrentPushesStayOrdered(t *testing.T) {
	const perLayer = maxHeldEnhancement * 2
	g := newGroupReassembler()
	var c collector

	var wg sync.WaitGroup
	for layer := range uint64(2) {
		wg.Go(func() {
			for i := range uint64(perLayer) {
				index := i*2 + layer
				g.Push(layer, index, c.emitter(index))
			}
		})
	}
	wg.Wait()

	got := c.order()
	if at := ascending(got); at >= 0 {
		t.Fatalf("emitted out of order at %d: %v", at, got[max(0, at-4):at+1])
	}
	seen := make(map[uint64]bool, len(got))
	for _, index := range got {
		seen[index] = true
	}
	for i := range uint64(perLayer) {
		if !seen[i*2] {
			t.Fatalf("base-layer frame %d was never emitted", i*2)
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

	// L1T2 emission: T0, T1, T0, T1... indexed 0,1,2,3 in that order, split
	// across subgroups by layer and here delivered layer by layer rather than
	// in step.
	for _, index := range []uint64{1, 3, 5, 7} {
		g.Push(1, index, c.emitter(index))
	}
	for _, index := range []uint64{0, 2, 4, 6} {
		g.Push(baseSubgroup, index, c.emitter(index))
	}

	want := make([]uint64, 7)
	for i := range want {
		want[i] = uint64(i)
	}
	if got := c.order(); !equal(got, want) {
		t.Fatalf("order = %v, want %v — emission order and index order must "+
			"agree, or the layout has drifted from what reassembly keys on", got, want)
	}
}
