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
// Object IDs are what put it back together. The publisher numbers a group's
// objects in emission order across every subgroup in it, so ascending object ID
// IS decode order, exactly — no timestamps, no heuristics, no cadence
// assumptions.
//
// The interesting question is when an object is safe to emit rather than how to
// order it. Waiting on a timer would be wrong in both directions: too short
// reorders nothing under load, too long adds latency to every frame, and a
// layer the relay has shed entirely would cost the timeout on every frame it
// was supposed to carry. Streams are ordered individually, so there is an exact
// answer instead — an object is safe once no open stream can still produce
// something earlier, which is knowable from the highest ID each open stream has
// delivered. In the common case the next object of the other layer has already
// arrived and nothing waits at all.

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
	objectID uint64
	emit     func()
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
	// next is the object ID the group is waiting to emit. Objects arrive
	// numbered from the publisher, and a group always opens at 0.
	next uint64
	// held is the out-of-order backlog, kept sorted by object ID. A slice
	// rather than a heap: it holds one frame per layer in the steady state,
	// where a linear insert beats a heap's constant factor and its allocation.
	held []heldObject
	// open maps a subgroup ID to the highest object ID it has delivered, for
	// every stream still running. A subgroup absent from the map cannot
	// produce anything, whether it ended or was never subscribed, so it does
	// not hold the group back.
	open map[uint64]uint64
	// started marks a subgroup as having delivered at least one object. A
	// stream that has opened but produced nothing yet must block release —
	// its first object could be the one being waited for — which a zero
	// highest-delivered cannot express on its own, since 0 is a real object ID.
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
// safe, in object-ID order, before returning.
//
// An object at or below next has already been emitted — a duplicate from a
// redundant publisher, or a replayed stream — and is dropped rather than
// emitted twice.
func (g *groupReassembler) Push(subgroup, objectID uint64, emit func()) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.open[subgroup]; !ok {
		// A stream pushing after it was closed, or one that never announced
		// itself. Track it either way: dropping the object would punch a hole
		// that stalls everything after it.
		g.open[subgroup] = objectID
		g.started[subgroup] = true
	} else if !g.started[subgroup] || objectID > g.open[subgroup] {
		g.open[subgroup] = objectID
		g.started[subgroup] = true
	}

	if objectID < g.next {
		return
	}
	g.insertLocked(heldObject{objectID: objectID, emit: emit})
	g.drainLocked()
}

// insertLocked places obj in held, keeping it sorted by object ID. The scan
// runs from the back because objects arrive in near-order: the common insert is
// at or near the end, so this is a comparison or two rather than a search.
func (g *groupReassembler) insertLocked(obj heldObject) {
	i := len(g.held)
	for i > 0 && g.held[i-1].objectID > obj.objectID {
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
		if !g.releasableLocked(head.objectID) {
			return
		}
		g.held = g.held[1:]
		g.next = head.objectID + 1
		head.emit()
	}
}

// releasableLocked reports whether the lowest held object can be emitted.
//
// Three ways to be sure, in the order they matter:
//
// It is the object the group is waiting for, so nothing can precede it.
//
// Or every open stream has already delivered something later. Each stream is
// ordered on its own, so a stream past this ID can never come back below it,
// and a stream with nothing left open cannot produce anything at all — between
// them, no earlier object exists to wait for. This is what covers a gap the
// relay punched: the objects around the hole arrive, every stream moves past
// it, and the run continues without the missing ID ever showing up.
//
// Or the backlog is full, and something has to give. Here the gap is conceded
// deliberately rather than waited out — see maxHeldObjects.
func (g *groupReassembler) releasableLocked(objectID uint64) bool {
	if objectID == g.next {
		return true
	}
	if len(g.held) >= maxHeldObjects {
		return true
	}
	for subgroup, highest := range g.open {
		if !g.started[subgroup] || highest < objectID {
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
		g.next = obj.objectID + 1
		obj.emit()
	}
	g.held = nil
	clear(g.open)
	clear(g.started)
}
