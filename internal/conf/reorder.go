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
// only against itself.
//
// The asymmetry between the layers is what this is built on, and it took a
// while to see. A subgroup is one QUIC stream, and a QUIC stream is ordered and
// retransmitted: it delivers every object it carries, in order, or it is reset.
// §11.4.3 closes the remaining gap by forbidding non-consecutive object IDs on
// one stream, so a relay that skipped an object must open a new stream rather
// than send a hole. The base layer therefore cannot arrive out of order or with
// gaps — only late.
//
// So the base layer needs no reassembly at all. It is emitted the moment it
// arrives, always, and the only thing that ever waits is an enhancement object
// that has overtaken the base frame it references. That is also the only thing
// it is ever safe to give up on: nothing references an enhancement frame, so
// dropping one costs exactly itself, where dropping a base frame costs every
// frame after it until the next keyframe.
//
// This replaced a design that ordered both layers through one backlog with a
// bound on its depth, and conceded the head of the queue when the bound was
// reached. Conceding advances past the missing index, so the base object it was
// waiting for — travelling intact on its own ordered stream, merely behind —
// arrived below the mark and was dropped as late. The frames that referenced it
// went to the decoder anyway. That is what a second of macroblock garbage is,
// and it was the app discarding a frame the network had delivered. A bound on
// waiting was the right instinct; applying it to a layer that cannot be dropped
// without consequence was not.

// baseSubgroup carries the temporal base layer. Every higher subgroup is an
// enhancement layer, referencing this one and referenced by nothing.
//
// Audio and video from an encoder without temporal layers publish on this
// subgroup alone, which is why they pass through here without ever waiting.
const baseSubgroup = 0

// maxHeldEnhancement bounds how many enhancement objects may wait for the base
// frame they belong behind.
//
// In the steady state this holds nothing: the publisher writes base, then
// enhancement, then base, and each arrives in its turn. It fills when a relay
// drains one subgroup well ahead of the other — a backfilled group, or a
// publisher catching up after a stall, where a whole layer can arrive before
// the first object of the other. Sized for that: a GOP is a keyframe interval
// at the capture frame rate, so an L1T2 group at 30 fps with a one-second
// interval carries fifteen enhancement frames.
//
// Overflow drops the oldest, which is the one whose base frame is furthest
// behind and so the likeliest to have been overtaken already. Nothing
// references it, so the cost is that frame and nothing else.
const maxHeldEnhancement = 16

// heldObject is one object waiting for its turn, with whatever the caller needs
// to emit it. The payload is opaque here: this file owns ordering, not media.
type heldObject struct {
	index uint64
	emit  func()
}

// groupReassembler orders one group's objects across its subgroup streams.
//
// Safe for concurrent use: one goroutine per subgroup stream calls Push, and
// whichever of them unblocks a run performs the emits for it, in order, before
// returning. Emitting under the lock is deliberate — it is what keeps two
// goroutines from interleaving their emits and undoing the ordering this exists
// to establish.
type groupReassembler struct {
	mu sync.Mutex
	// next is the lowest emission index not yet emitted. Anything below it has
	// either gone out or been given up on.
	next uint64
	// held is enhancement objects waiting for the base frame they sit behind,
	// sorted by index. A slice rather than a heap: it is empty in the steady
	// state, and a linear insert beats a heap's constant factor and its
	// allocation at the depth this reaches.
	held []heldObject
}

func newGroupReassembler() *groupReassembler {
	return &groupReassembler{}
}

// Push takes one object from one subgroup stream and emits whatever that makes
// safe, in emission order, before returning.
//
// A base-layer object goes out immediately: its stream is ordered, so it cannot
// have overtaken anything of its own layer, and every enhancement object still
// held below its index belongs in front of it.
//
// An enhancement object goes out immediately too when it is the next index due,
// which is what the steady state looks like — base, enhancement, base — so the
// ordinary frame waits for nothing. Only one that has overtaken the base frame
// it references is held, until that frame arrives and proves nothing earlier is
// still to come. It is never waited *for*: the base layer does not stop for one,
// so a shed or stalled enhancement layer costs its own frames and no picture.
//
// An object below next is either a duplicate — a redundant publisher, or a
// replayed stream — or one that arrived too late to be placed. Both are
// dropped: handing a decoder a frame it is already past is worse than not
// handing it one at all.
func (g *groupReassembler) Push(subgroup, index uint64, emit func()) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if index < g.next {
		return
	}
	if subgroup != baseSubgroup && index != g.next {
		g.holdLocked(heldObject{index: index, emit: emit})
		return
	}
	g.releaseBelowLocked(index)
	g.next = index + 1
	emit()
}

// releaseBelowLocked emits every held object numbered before index, in order.
// They reference base frames that have already gone out, and the base frame
// arriving now is what proves none of them can be preceded by anything further.
func (g *groupReassembler) releaseBelowLocked(index uint64) {
	for len(g.held) > 0 && g.held[0].index < index {
		obj := g.held[0]
		g.held = g.held[1:]
		g.next = obj.index + 1
		obj.emit()
	}
}

// holdLocked places obj in held, keeping it sorted by index and inside the
// bound. The scan runs from the back because objects arrive in near-order: the
// common insert is at or near the end, so this is a comparison or two rather
// than a search.
func (g *groupReassembler) holdLocked(obj heldObject) {
	i := len(g.held)
	for i > 0 && g.held[i-1].index > obj.index {
		i--
	}
	g.held = append(g.held, heldObject{})
	copy(g.held[i+1:], g.held[i:])
	g.held[i] = obj

	if len(g.held) > maxHeldEnhancement {
		g.held = g.held[1:]
	}
}

// backlog is how many objects are waiting, for tests that pin the bound.
func (g *groupReassembler) backlog() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.held)
}
