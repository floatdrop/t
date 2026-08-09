package conf

import "sync"

// Reassembling a group that arrives on more than one subgroup stream.
//
// One group used to mean one subgroup stream, so decode order fell out of the
// transport: a single stream is ordered, and the reader forwarded each object
// as it came. Temporal layers break that. The layers ride separate subgroups so
// a relay can shed the top one without touching the base (a subgroup is the
// smallest thing §5.1.3 lets a subscriber decline and §8 lets a publisher mark
// sheddable), and separate subgroups mean separate streams, each read on its
// own goroutine. Two goroutines interleaving arbitrarily is exactly what a
// video decoder cannot take: a delta frame ahead of the frame it references is
// not late, it is undecodable.
//
// The publisher's emission index is what puts it back together: every object
// carries its position in the group's emission order, counted across every
// subgroup, so ascending index IS decode order — exactly, with no cadence
// assumptions. Object IDs cannot serve, because each subgroup owns a contiguous
// range of its own (see layerObjectStride) and an ID therefore orders a stream
// only against itself. Timestamps could order but not schedule: they say which
// frame comes first and never whether anything earlier is still to come, so
// every frame would wait out the other layer.
//
// The interesting question is when an object is safe to emit rather than how to
// order it. Waiting on a timer would be wrong in both directions: too short
// reorders nothing under load, too long adds latency to every frame, and a
// layer the relay has shed entirely would cost the timeout on every frame it
// was supposed to carry. Streams are ordered individually, so there is an exact
// answer instead — an object is safe once no open stream can still produce
// something earlier, which is knowable from the highest index each open stream
// has delivered. In the common case the object that arrives is the one being
// waited for and nothing is held at all, layered or not.
//
// "No open stream" is the subtle part, and it once meant "no stream this group
// has seen", which is not the same claim. Streams of a group do not arrive
// together: the relay forwards each as it drains, and when a whole group is
// available at once — a backfilled group, or a publisher catching up after a
// stall — it can drain one subgroup nearly to the end before opening the next.
// A group that has only seen the base layer concludes that nothing can precede
// anything and releases the lot, after which every enhancement object arrives
// already too late to be placed. Measured against a real relay that lost half
// the frames of a group written back to back.
//
// A group waited for a layer it had been told to expect, once, and that was
// worse than the problem it solved. A relay shedding the enhancement layer —
// which is the whole point of publishing it separately — simply never opens
// that stream, so the group conceded nothing: base-layer indices arrive 0, 2,
// 4, only the first satisfying "the one being waited for", and the rest sat
// held until the group was retired two groups later. Four seconds of frozen
// tile followed by four seconds of video at once, over and over, on any link
// that made the relay shed. The burst it was protecting against costs one
// group's enhancement layer; this cost the picture.
//
// So an unseen subgroup is not waited for. What can still produce something
// earlier is what has actually been seen and not yet ended, which is the
// question the open streams answer.

// maxHeldObjects bounds one group's reassembly buffer.
//
// Reached only when a stream stops delivering without ending — a publisher that
// stalls mid-group, or a relay holding a subgroup open with nothing on it. The
// other layers keep arriving and pile up behind the object that will never
// come, so this is the release valve: past it the oldest held object goes out
// and the gap is conceded. Sized well above a GOP's frame count (two seconds at
// 30 fps is 60 objects across all layers) so a healthy group never approaches
// it; small enough to bound the memory one wedged publisher can cost.
const maxHeldObjects = 128

// heldObject is one object waiting for its turn, with whatever the caller needs
// to emit it. The payload is opaque here: this file owns ordering, not media.
type heldObject struct {
	index uint64
	emit  func()
}

// groupReassembler orders one group's objects across its subgroup streams.
//
// Safe for concurrent use: one goroutine per subgroup stream calls Push, and
// whichever of them unblocks the next contiguous run performs the emits for it,
// in order, before returning. Emitting under the lock is deliberate — it is
// what keeps two goroutines from interleaving their emits and undoing the
// ordering this exists to establish.
type groupReassembler struct {
	mu sync.Mutex
	// next is the emission index the group is waiting to emit. Objects arrive
	// numbered from the publisher, and a group always opens at 0.
	next uint64
	// held is the out-of-order backlog, kept sorted by index. A slice
	// rather than a heap: it holds one frame per layer in the steady state,
	// where a linear insert beats a heap's constant factor and its allocation.
	held []heldObject
	// open maps a subgroup ID to the highest index it has delivered, for
	// every stream still running. A subgroup absent from it cannot produce
	// anything — it ended, was shed, or was never subscribed — so it does not
	// hold the group back.
	open map[uint64]uint64
	// started marks a subgroup as having delivered at least one object. A
	// stream that has opened but produced nothing yet must block release —
	// its first object could be the one being waited for — which a zero
	// highest-delivered cannot express on its own, since 0 is a real index.
	started map[uint64]bool
}

func newGroupReassembler() *groupReassembler {
	return &groupReassembler{
		open:    make(map[uint64]uint64),
		started: make(map[uint64]bool),
	}
}

// OpenSubgroup registers a stream that may still deliver objects. Called before
// the first Push from that stream, so a subgroup which has opened but not yet
// delivered still holds back objects numbered after its first.
func (g *groupReassembler) OpenSubgroup(subgroup uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.open[subgroup]; !ok {
		g.open[subgroup] = 0
		g.started[subgroup] = false
	}
}

// CloseSubgroup reports a stream as finished — cleanly, reset, or abandoned;
// the distinction does not matter here, only that nothing more will come from
// it. Whatever its absence unblocks is emitted before this returns.
//
// This is what makes a shed layer cost nothing. When a relay drops the top
// subgroup, that stream ends, and from then on the base layer's objects are
// released the moment they arrive rather than each one waiting out a timer for
// a partner that is never coming.
func (g *groupReassembler) CloseSubgroup(subgroup uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.open, subgroup)
	delete(g.started, subgroup)
	g.drainLocked()
}

// Push takes one object from one subgroup stream and emits whatever that makes
// safe, in emission order, before returning.
//
// An object below next is either a duplicate — a redundant publisher, or a
// replayed stream — or one that arrived too late to be placed. Both are
// dropped: handing a decoder a frame it is already past is worse than not
// handing it one at all.
func (g *groupReassembler) Push(subgroup, index uint64, emit func()) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.open[subgroup]; !ok {
		// A stream pushing after it was closed, or one that never announced
		// itself. Track it either way: dropping the object would punch a hole
		// that stalls everything after it.
		g.open[subgroup] = index
		g.started[subgroup] = true
	} else if !g.started[subgroup] || index > g.open[subgroup] {
		g.open[subgroup] = index
		g.started[subgroup] = true
	}

	if index < g.next {
		return
	}
	g.insertLocked(heldObject{index: index, emit: emit})
	g.drainLocked()
}

// insertLocked places obj in held, keeping it sorted by index. The scan
// runs from the back because objects arrive in near-order: the common insert is
// at or near the end, so this is a comparison or two rather than a search.
func (g *groupReassembler) insertLocked(obj heldObject) {
	i := len(g.held)
	for i > 0 && g.held[i-1].index > obj.index {
		i--
	}
	g.held = append(g.held, heldObject{})
	copy(g.held[i+1:], g.held[i:])
	g.held[i] = obj
}

// drainLocked emits every object that is now safe, in order.
func (g *groupReassembler) drainLocked() {
	for len(g.held) > 0 {
		head := g.held[0]
		if !g.releasableLocked(head.index) {
			return
		}
		g.held = g.held[1:]
		g.next = head.index + 1
		head.emit()
	}
}

// releasableLocked reports whether the oldest held object can be emitted.
//
// Three ways to be sure, in the order they matter:
//
// It is the object the group is waiting for, so nothing can precede it. This is
// the common case and it costs nothing.
//
// Or the backlog is full, and something has to give. Here the wait is abandoned
// deliberately rather than run forever — see maxHeldObjects.
//
// Or every open stream has already delivered something later, and no subgroup is
// still expected. Each stream is ordered on its own, so a stream past this index
// can never come back below it, and a stream with nothing left open cannot
// produce anything at all — between them, nothing earlier exists to wait for.
// This is what covers a gap the relay punched, a layer it shed, and a layer this
// subscriber declined alike: the frames around the hole arrive, every stream
// moves past it, and the run continues without the missing index showing up.
func (g *groupReassembler) releasableLocked(index uint64) bool {
	if index == g.next {
		return true
	}
	if len(g.held) >= maxHeldObjects {
		return true
	}
	for subgroup, highest := range g.open {
		if !g.started[subgroup] || highest < index {
			return false
		}
	}
	return true
}

// idle reports whether every subgroup stream of this group has ended, which is
// when the group can be retired.
func (g *groupReassembler) idle() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.open) == 0
}

// Flush emits everything still held, in order, and forgets the group. Called
// when a group is done — every stream ended, or the track went away — so that
// objects held back by a stream that never closed are not simply lost.
func (g *groupReassembler) Flush() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, obj := range g.held {
		g.next = obj.index + 1
		obj.emit()
	}
	g.held = nil
	clear(g.open)
	clear(g.started)
}
