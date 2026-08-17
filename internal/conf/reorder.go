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
//
// # A backfilled group needs none of this
//
// The Joining FETCH that paints a joining subscriber's first picture asks for
// the base layer alone (SUBGROUP_FILTER, see backfillGroup in remote.go). One
// subgroup is one stream and one stream is ordered, so a backfilled group
// arrives already in decode order and passes straight through: every object is
// a base-layer object, and a base-layer object is emitted on arrival. Nothing
// is held, nothing waits, and sawEnhancement stays false so the grace window
// below never engages — there is no second stream for it to be racing.
//
// That is a property of the filter, not a coincidence, and it is what makes the
// backfill affordable at all. A FETCH answers in ascending Object ID, and the
// layers own disjoint ID ranges, so an *unfiltered* backfill arrives as the
// whole base layer followed by the whole enhancement layer — every enhancement
// frame after every frame it belongs between. Ordering that means holding the
// entire group before delivering any of it, which is what the previous backfill
// did and what guaranteed it lost its race with live video.

// baseSubgroup carries the temporal base layer. Every higher subgroup is an
// enhancement layer, referencing this one and referenced by nothing.
//
// Audio and video from an encoder without temporal layers publish on this
// subgroup alone, which is why they pass through here without ever waiting. So
// does a backfilled group, for the same reason.
const baseSubgroup = 0

// maxHeldEnhancement bounds how many enhancement objects may wait for the base
// frame they belong behind.
//
// In the steady state this holds nothing: the publisher writes base, then
// enhancement, then base, and each arrives in its turn. It fills when a relay
// drains one subgroup well ahead of the other — a publisher catching up after a
// stall, or a burst after a pause, where a run of one layer can arrive before
// the other's next object.
//
// Overflow drops the oldest, which is the one whose base frame is furthest
// behind and so the likeliest to have been overtaken already. Nothing
// references it, so the cost is that frame and nothing else.
//
// Deliberately far below a GOP, which at a five-second keyframe interval is
// seventy-five enhancement frames. Holding a GOP's worth would mean holding it
// for every group in flight on every participant, and what it would buy is
// frames whose moment has passed by the time the base layer catches up. Sixteen
// is the burst this stack actually produces; past that, the layer built to be
// disposable is doing what it was built for.
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
// the scheduler.
//
// What that costs is the frame and nothing else. Nothing references an
// enhancement frame — that is the whole reason it is the disposable layer — so
// this is not a correctness fix and it does not prevent macroblock garbage; it
// buys back frame rate. At L1T2 every such drop takes 30 fps to 15 for one
// frame, and a run of them is a picture that stutters while the base layer
// keeps the motion honest. Worth a few milliseconds, worth nothing more, and
// never worth holding the base layer for something that is not coming.
//
// So it is bounded three ways, and the last two matter more than the duration.
// The wait is at most this long; it engages only when the enhancement layer has
// been seen on this group, so a layer a relay shed — which never opens its
// stream at all — is never waited for; and it engages only while the missing
// index is still ahead of everything that layer has delivered, because its
// subgroup is one ordered stream and a higher index having arrived proves the
// gap cannot. See gapWorthWaitingForLocked. Together they are what keeps
// TestReassemblerNeverHoldsTheBaseLayer passing: the freeze this design exists
// to prevent is waiting for something that is not coming.
//
// Five milliseconds is far longer than a goroutine takes to be scheduled on a
// loaded machine, and short enough to disappear into the inter-frame gap at
// 30 fps (33 ms). It is a ceiling rather than a cost: the wait ends the moment
// the missing frame arrives, so what it actually spends is the scheduler skew,
// which is microseconds. Measured on a paced 30 fps interleave it never engages
// at all.
const enhancementGraceWindow = 5 * time.Millisecond

// heldObject is one object waiting for its turn, with whatever the caller needs
// to emit it. The payload is opaque here: this file owns ordering, not media.
type heldObject struct {
	subgroup uint64
	index    uint64
	emit     func()
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
// returning. **Every emit happens under the lock**, and that is not incidental
// — it is the only thing keeping two goroutines from interleaving their emits
// and undoing the ordering this exists to establish. Releasing the lock before
// emitting reintroduces the inversion at once: the enhancement goroutine
// unlocks after advancing next, the base goroutine takes the lock, advances,
// unlocks and emits, and the two emits land in the wrong order. Measured, on
// the plain L1T2 interleave, within a couple of hundred frames.
//
// The one place the lock is released is around the grace window's wait, which
// is exactly so that the other layer's goroutine can Push into it. Nothing has
// been emitted at that point and nothing is emitted until it is reacquired.
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
	// this group, whether or not it was emitted, and enhancementUpTo is the
	// highest index one carried. Together they are the grace window's gate.
	//
	// sawEnhancement answers "is this layer being delivered at all". A relay
	// that sheds it never opens its stream, so this stays false and the base
	// layer is never held for it. A backfilled group is base-layer only by
	// construction, so it stays false there too.
	//
	// enhancementUpTo answers the sharper question: could *this* frame still be
	// coming. An enhancement subgroup is one QUIC stream and a QUIC stream is
	// ordered, so an index above the gap having already arrived proves the gap
	// is not in flight — it was shed by the backlog bound, or the relay never
	// sent it. Waiting for it then is waiting for something that cannot arrive,
	// and it costs the full window on every base frame until the group ends.
	// Which a burst produces exactly: the enhancement layer runs ahead, the
	// backlog bound sheds its oldest, and every base frame behind the shed run
	// would otherwise pay five milliseconds for a frame already discarded.
	sawEnhancement  bool
	enhancementUpTo uint64

	// graceWindow is how long a base frame waits for a missing enhancement
	// frame before advancing past it. Set to enhancementGraceWindow in
	// newGroupReassembler; overridden to zero by tests that exercise the
	// pure-drop semantics the grace window is layered on top of.
	graceWindow time.Duration

	// pushed is closed and replaced by every Push, so a base frame waiting out
	// a gap wakes when the missing frame actually lands rather than on a timer
	// tick. A sleeping poll spent a millisecond answering a question whose
	// answer arrives in microseconds, and spent it holding a subgroup reader
	// off its stream.
	pushed chan struct{}

	// onDrop, when non-nil, is called with the subgroup, index, and reason
	// whenever an object is discarded. Diagnostic: the reassembler is a pure
	// ordering component, so this is the only window into what it gave up on.
	// Supplied at construction rather than assigned afterwards — every subgroup
	// stream of a group reaches the same reassembler, on its own goroutine, so
	// a field set by whichever got there first is a data race with the Push
	// that reads it.
	onDrop func(subgroup, index uint64, reason dropReason)
}

func newGroupReassembler(onDrop func(subgroup, index uint64, reason dropReason)) *groupReassembler {
	return &groupReassembler{
		graceWindow: enhancementGraceWindow,
		pushed:      make(chan struct{}),
		onDrop:      onDrop,
	}
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
// still to come. It is never waited *for* beyond the grace window: the base
// layer does not stop for one, so a shed or stalled enhancement layer costs its
// own frames and no picture.
//
// An object below next is either a duplicate — a redundant publisher, or a
// replayed stream — or one that arrived too late to be placed. Both are
// dropped: handing a decoder a frame it is already past is worse than not
// handing it one at all.
func (g *groupReassembler) Push(subgroup, index uint64, emit func()) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if subgroup != baseSubgroup {
		g.enhancementUpTo = max(g.enhancementUpTo, index)
		g.sawEnhancement = true
	}
	// Waking the waiter is the last thing done with this Push, not the first:
	// see wakeLocked. Announced here so every exit below is covered.
	defer g.wakeLocked()

	if index < g.next {
		g.dropLocked(subgroup, index, dropReasonOvertaken)
		return
	}
	if subgroup != baseSubgroup && index != g.next {
		g.holdLocked(heldObject{subgroup: subgroup, index: index, emit: emit})
		return
	}

	// A base-layer object, or any object that is the next index due. Release
	// everything held below it first, then check for a gap: if the enhancement
	// layer has been seen and there is a missing index between what was just
	// released and this object, the missing frame may be in flight on its own
	// goroutine — merely delayed by the scheduler, not absent. Wait briefly
	// for it; if it does not arrive, proceed and let the late Push drop it.
	g.releaseBelowLocked(index)
	if g.gapWorthWaitingForLocked(index) {
		g.waitForGapLocked(index)
	}
	g.next = index + 1
	emit()
}

// gapWorthWaitingForLocked reports whether the missing index at g.next could
// still arrive, which is the only case the grace window is for. The caller
// holds mu; see sawEnhancement and enhancementUpTo for the two questions.
func (g *groupReassembler) gapWorthWaitingForLocked(index uint64) bool {
	return g.graceWindow > 0 &&
		g.next < index &&
		g.sawEnhancement &&
		g.next > g.enhancementUpTo
}

// waitForGapLocked waits up to graceWindow for the enhancement frame at g.next
// to arrive, releasing the lock while it waits so that the enhancement layer's
// goroutine can Push into it, and re-running releaseBelowLocked on each wake to
// pick up whatever landed. Returns with g.next at or above index, or with the
// window expired.
//
// When the missing frame arrives during the wait, its own Push emits it and
// advances g.next past the gap; the wake that follows fails the loop condition
// and the wait ends. Woken by Push rather than by a ticker, so the wait costs
// the scheduler skew it exists to absorb and not a poll interval.
//
// g.next cannot run past index here, which is what makes the caller's
// unconditional `g.next = index + 1` safe: releaseBelowLocked only releases
// objects below index, and the only other writer is a Push whose index equals
// g.next, which is below index for as long as this loop runs.
func (g *groupReassembler) waitForGapLocked(index uint64) {
	deadline := time.NewTimer(g.graceWindow)
	defer deadline.Stop()

	for g.next < index {
		pushed := g.pushed
		g.mu.Unlock()
		select {
		case <-pushed:
		case <-deadline.C:
			g.mu.Lock()
			return
		}
		g.mu.Lock()
		g.releaseBelowLocked(index)
	}
}

// wakeLocked releases anything waiting in waitForGapLocked and re-arms the
// channel for the next wait. The caller holds mu.
//
// Deferred to the end of Push rather than run at its start, because a waiter
// woken before the arriving object has been placed would find nothing changed
// and go straight back to waiting — on a channel this Push had already closed,
// which is a wake it cannot get twice.
func (g *groupReassembler) wakeLocked() {
	close(g.pushed)
	g.pushed = make(chan struct{})
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
		g.dropLocked(shed.subgroup, shed.index, dropReasonBacklogFull)
	}
}

// dropLocked reports a discarded object. The caller holds mu, and the callback
// runs under it: it is a log line on a path that only runs when something has
// already gone wrong, and calling it outside would mean reporting drops in an
// order unrelated to the order they happened in.
func (g *groupReassembler) dropLocked(subgroup, index uint64, reason dropReason) {
	if g.onDrop != nil {
		g.onDrop(subgroup, index, reason)
	}
}

// backlog is how many objects are waiting, for tests that pin the bound.
func (g *groupReassembler) backlog() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.held)
}
