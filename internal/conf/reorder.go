package conf

import (
	"sync"
	"time"
)

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
// at the capture frame rate, so an L1T2 group at 30 fps with a five-second
// interval carries seventy-five enhancement frames.
//
// Overflow drops the oldest, which is the one whose base frame is furthest
// behind and so the likeliest to have been overtaken already. Nothing
// references it, so the cost is that frame and nothing else. The bound is
// deliberately below a full GOP: a backfill that dumps the entire enhancement
// layer at once is a relay pathology, not the steady state, and holding 75
// frames for each of several groups is memory the call cannot spare.
const maxHeldEnhancement = 16

// enhancementGraceWindow is how long a base-layer frame waits for a missing
// enhancement frame whose index sits between the reassembler's current
// position and the base frame's own index, when the enhancement layer has
// already been seen on this group.
//
// Two temporal layers ride separate QUIC streams, each read on its own
// goroutine with no ordering between them. The base layer can deliver several
// frames before the enhancement layer's reader is scheduled, and the
// reassembler's next pointer advances past the missing enhancement index —
// when it finally arrives it is below the mark and dropped as "overtaken",
// even though nothing was wrong: the frame was in flight, merely delayed by
// the scheduler. The decoder then receives frames that reference the dropped
// one, producing macroblock garbage until the next keyframe.
//
// The grace window closes that gap. It is bounded: the wait is at most this
// long, and it only engages when the enhancement layer has already been seen
// on this group — proving the layer is being delivered, not shed by a relay.
// A shed layer never opens its stream, so sawEnhancement stays false and the
// base layer goes out on arrival, exactly as before. This is what keeps
// TestReassemblerNeverHoldsTheBaseLayer passing: the freeze that design
// exists to prevent is the case where the layer is not coming at all.
//
// Five milliseconds is far longer than a goroutine takes to be scheduled on a
// loaded machine, and short enough to disappear into the inter-frame gap at
// 30 fps (33 ms). The cost is one grace window per genuine gap, and a genuine
// gap — an enhancement frame that is actually missing rather than merely
// delayed — is the case the bound is sized for: after the window, the base
// frame goes out and the missing enhancement frame is dropped on arrival, as
// before.
const enhancementGraceWindow = 5 * time.Millisecond

// heldObject is one object waiting for its turn, with whatever the caller needs
// to emit it. The payload is opaque here: this file owns ordering, not media.
type heldObject struct {
	index uint64
	emit  func()
}

// dropReason is why the reassembler discarded an object, for diagnostic logging.
type dropReason string

const (
	dropReasonOvertaken   dropReason = "overtaken"   // index < next: arrived after its turn
	dropReasonBacklogFull dropReason = "backlogFull" // maxHeldEnhancement exceeded, oldest shed
)

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

	// sawEnhancement is true once any enhancement object has been pushed for
	// this group, whether or not it was emitted. It gates the grace window:
	// the wait engages only when the enhancement layer has been seen, proving
	// it is being delivered rather than shed. A shed layer never opens its
	// stream, so this stays false and the base layer is never held.
	sawEnhancement bool

	// graceWindow is how long a base frame waits for a missing enhancement
	// frame before advancing past it. Set to enhancementGraceWindow in
	// newGroupReassembler; overridden to zero by tests that exercise the
	// pure-drop semantics the grace window is layered on top of.
	graceWindow time.Duration

	// onDrop, when non-nil, is called with the subgroup, index, and reason
	// whenever an object is discarded. Diagnostic: the reassembler is a pure
	// ordering component, so this is the only window into what it gave up on.
	// Wired by readMedia in remote.go, which has the logger and track context.
	onDrop func(subgroup, index uint64, reason dropReason)
}

func newGroupReassembler() *groupReassembler {
	return &groupReassembler{graceWindow: enhancementGraceWindow}
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

	if subgroup != baseSubgroup {
		g.sawEnhancement = true
	}

	if index < g.next {
		reason := dropReasonOvertaken
		drop := g.onDrop
		g.mu.Unlock()
		if drop != nil {
			drop(subgroup, index, reason)
		}
		return
	}
	if subgroup != baseSubgroup && index != g.next {
		g.holdLocked(heldObject{index: index, emit: emit})
		g.mu.Unlock()
		return
	}

	// A base-layer object, or any object that is the next index due. Release
	// everything held below it first, then check for a gap: if the enhancement
	// layer has been seen and there is a missing index between what was just
	// released and this object, the missing frame may be in flight on its own
	// goroutine — merely delayed by the scheduler, not absent. Wait briefly
	// for it; if it does not arrive, proceed as before and let the late Push
	// drop it.
	g.releaseBelowLocked(index)
	if g.sawEnhancement && g.next < index && g.graceWindow > 0 {
		g.waitForGapLocked(index)
	}
	g.next = index + 1
	g.mu.Unlock()
	emit()
}

// waitForGapLocked waits up to graceWindow for the enhancement frame at g.next
// to arrive, sleeping in short increments and re-running releaseBelowLocked
// on each wake to pick up anything that landed. The lock is released during
// each sleep so the enhancement layer's goroutine can Push; reacquired on
// wake. Returns with g.next at or above index, or with the window expired.
//
// When the missing frame arrives during a sleep, its own Push emits it and
// advances g.next past the gap; on the next wake the loop condition fails and
// the wait ends.
func (g *groupReassembler) waitForGapLocked(index uint64) {
	deadline := time.Now().Add(g.graceWindow)
	for g.next < index && time.Now().Before(deadline) {
		g.mu.Unlock()
		time.Sleep(1 * time.Millisecond)
		g.mu.Lock()
		g.releaseBelowLocked(index)
	}
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
		shed := g.held[0]
		g.held = g.held[1:]
		if g.onDrop != nil {
			g.onDrop(baseSubgroup+1, shed.index, dropReasonBacklogFull)
		}
	}
}

// backlog is how many objects are waiting, for tests that pin the bound.
func (g *groupReassembler) backlog() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.held)
}
