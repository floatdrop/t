package conf

import (
	"sync"
	"time"
)

// Ordering whole groups of one track against each other, which is the level
// above reorder.go.
//
// A reassembler covers one group. Groups are their own streams — a group's
// subgroups and the next group's are all separate QUIC streams, and §11.4.2
// orders objects only within a stream — so nothing stops a group's tail
// arriving behind the next group's head, and nothing in a per-group reassembler
// can see that it has. Measured on a loopback relay: over a burst, whole groups
// swap.
//
// Both media are hurt by it and neither can undo it downstream. Every video
// group opens on a keyframe, a keyframe is an IDR, and an IDR empties the
// decoded picture buffer — so a frame from the group before it references
// pictures that are gone and decodes to macroblocks or to nothing, one flash per
// boundary it happens on. Audio has no such coupling, but the ring buffer in the
// player worklet is contiguous by construction, so a packet handed to it late is
// simply played in the wrong place.
//
// So groups are held rather than dropped: an object of a group ahead of one
// still in flight waits for it. Waiting costs nothing in the case that is every
// call, because a group's stream has ended well before the next group's first
// object arrives — measured at both the frame rate and the audio cadence, where
// nothing is ever held.
//
// **A group's turn is claimed and released by the base layer alone.** Two things
// hang on that. The claim (Open) happens on the accept loop, in the order the
// transport delivered the streams, which is the only place that order still
// exists — once the reader is on its own goroutine per stream, which group
// reaches its first object first is up to the scheduler, and a mark taken there
// puts whole groups below the line. The release (Finish) is the stream *ending*,
// which happens whether it was closed or reset, so nothing waits on a message
// that has to be sent.
//
// And nothing ever waits on the enhancement layer. It is the one a relay delays
// by half a second and sheds outright, so a group that waited for it would hand
// that delay to the next group's keyframe — the disposable layer holding up the
// one that cannot be dropped, which is the priority design backwards. The cost
// is that an enhancement object arriving after its group's base stream has ended
// is dropped, which a burst does produce; that is the layer built to be
// disposable, and losing frames of it is what a burst has always been allowed to
// cost.
//
// What this must never do is wait for something that is not coming. That failure
// is already in this project's history — a group that waited for a layer the
// catalog said to expect sat held until the group was retired, four seconds of
// frozen tile then four seconds at once, because a relay shedding a layer never
// opens its stream at all. Nothing is waited for here until its stream has
// actually arrived, and the backlog is bounded on top of that, so a stream that
// opens and then stalls costs the bound and no more.

// settleWindow is how long the mark may still be lowered after the first stream
// of a track arrives, before anything is delivered.
//
// Open runs on the accept loop, so the streams are announced in the order the
// transport delivered them — but the readers run on their own goroutines, and a
// reader can push its first object before the accept loop has announced a stream
// that arrived just behind it. The mark cannot be lowered once something has
// gone out, because a group delivered behind one already played is the inversion
// all of this exists to prevent, so that race costs the earlier group entirely:
// measured at one run in eight over a three-group burst, silently dropping
// twenty-five of seventy-five objects.
//
// Nothing structural distinguishes "an earlier group is still being announced"
// from "there is no earlier group", so this is the one place that spends time to
// answer it. It is a settle at the start of a track and not a jitter buffer: it
// runs once, it holds only until it expires, and once the mark is set every
// later group is ordered against it with no waiting at all. Fifty milliseconds
// is far longer than an accept loop takes to drain what is already readable, and
// short enough to disappear into the wait for a first keyframe.
const settleWindow = 50 * time.Millisecond

// maxHeldAcrossGroups bounds how many objects may wait for an earlier group.
//
// It is a backstop, not a working limit, and sizing it as one was a bug. Every
// group waited for here has had its stream arrive — Open is what set the mark —
// and an open stream ends, by FIN, by reset, or by the session going away, so in
// the ordinary course nothing needs giving up on at all. What this covers is the
// one case that does not end: a relay that opens a stream, stops sending on it
// and never resets it, where without a ceiling the track would hold everything
// behind it for the rest of the call.
//
// So it has to sit above any backlog the transport legitimately produces. A
// burst puts several groups in flight at once and the reader takes each on its
// own goroutine, so a group is behind by however long its goroutine waits to be
// scheduled — nothing to do with the data, and on a loaded machine long enough
// for two later groups to arrive whole. At one group's worth that is exactly
// what happened: three groups of audio written back to back, the bound reached
// while the first group's reader had not run, and all twenty-five of its objects
// dropped as passed when they landed. Every burst cost a group.
//
// A hundred and twenty-eight is above any burst this stack produces — media
// subscribes at the live edge, so a backfill is a couple of groups — and still a
// ceiling. The pathological case pays for it in latency rather than silence:
// what is held is emitted when the bound trips, not discarded.
const maxHeldAcrossGroups = 128

// heldGroupObject is one object waiting for an earlier group, with whatever the
// caller needs to emit it. Ordered by group first and then by the publisher's
// emission index, which together are the publisher's order exactly.
type heldGroupObject struct {
	group uint64
	index uint64
	emit  func()
}

// before reports whether a sorts ahead of b in the publisher's order.
func (a heldGroupObject) before(b heldGroupObject) bool {
	if a.group != b.group {
		return a.group < b.group
	}
	return a.index < b.index
}

// groupOrderer delivers a track's groups in ascending order.
//
// Safe for concurrent use: one goroutine per subgroup stream calls Push and
// Finish, and whichever of them unblocks a run performs the emits for it, in
// order, before returning. Emitting under the lock is what keeps two goroutines
// from interleaving their emits and undoing the ordering.
type groupOrderer struct {
	mu sync.Mutex
	// next is the oldest group not yet done with. Objects of it go straight
	// out; objects of anything later wait.
	next    uint64
	started bool
	// finished holds groups whose stream has ended while an earlier group was
	// still open, so their turn can be taken as soon as it comes round.
	finished map[uint64]bool
	// held is objects waiting for an earlier group, in publisher order.
	held []heldGroupObject
	// emitted records that something has gone out, after which the mark can no
	// longer be lowered: a stream opening late for an earlier group cannot undo
	// what the frontend has already been handed.
	emitted bool
	// settling is true from the first stream of the track until settleWindow has
	// passed. Everything is held meanwhile, which is what keeps emitted false and
	// so keeps the mark free to move down to whatever else is still arriving.
	settling    bool
	settleTimer *time.Timer
	// passed and abandoned count what ordering cost: objects dropped for
	// arriving after their group's turn had been given up on, and the number of
	// times that happened.
	passed    uint64
	abandoned uint64
}

func newGroupOrderer() *groupOrderer {
	return &groupOrderer{finished: make(map[uint64]bool)}
}

// Open records that a stream carrying one group has arrived, which is what
// establishes where the order starts.
//
// The first *object* cannot establish it. A burst puts several groups in flight
// at once and their streams are read on separate goroutines, so the first object
// to be decoded is not the first group — and a mark taken from it drops every
// group below it as already passed, which on the audio cadence is whole seconds
// of sound. Taking it from the stream instead moves the race from "which group
// was decoded first" to "which goroutine started first", and those goroutines
// start before any of them has read anything.
//
// Only downwards, and only before anything has gone out. A stream that opens for
// an earlier group after the frontend has already been handed a later one is
// genuinely late, and there is nothing to be done about it from here.
func (o *groupOrderer) Open(group uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.started {
		o.next, o.started, o.settling = group, true, true
		o.settleTimer = time.AfterFunc(settleWindow, o.settleNow)
		return
	}
	if group < o.next && (o.settling || !o.emitted) {
		o.next = group
	}
}

// settleNow ends the settle window and releases what it held, in order. Called
// by the timer Open starts, and directly by the tests that pin the ordering.
func (o *groupOrderer) settleNow() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.settling {
		return
	}
	o.settling = false
	o.drainLocked()
}

// Push takes one object and emits whatever that makes due, in order.
//
// Setting the mark here is the fallback for an object whose stream never
// announced itself; Open is what normally does it. Either way it starts at the
// group seen rather than at zero, because a subscriber joins a call in progress
// and lands wherever the publisher has got to — waiting for every group before
// that would be waiting for groups nobody will ever send.
func (o *groupOrderer) Push(group, index uint64, emit func()) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.started {
		// No stream announced this one, so there is no settle to wait for: the
		// mark is whatever arrived.
		o.next, o.started = group, true
	}
	if o.settling {
		// Held rather than placed, and the mark follows anything older that
		// turns up. Nothing may go out until the window closes, because going
		// out is exactly what would freeze the mark too high.
		if group < o.next {
			o.next = group
		}
		o.insertLocked(heldGroupObject{group: group, index: index, emit: emit})
		return
	}
	switch {
	case group < o.next:
		// Its turn has been and gone, which takes a group given up on under the
		// bound — a stream that ends cannot produce more. Emitting it now would
		// be the inversion this exists to prevent, so it is the one thing here
		// that is dropped.
		o.passed++
		return
	case group == o.next:
		o.emitted = true
		emit()
		return
	}

	o.insertLocked(heldGroupObject{group: group, index: index, emit: emit})
	if len(o.held) > maxHeldAcrossGroups {
		// The group being waited for is not coming: its stream was never opened,
		// or was opened and never delivered. Give up on it and take the oldest
		// thing actually in hand, rather than holding the call open for it.
		o.abandoned++
		o.next = o.held[0].group
	}
	o.drainLocked()
}

// Finish records that a group's stream has ended, which is the ordinary way a
// group's turn is over.
//
// The stream ending is the signal rather than an object count, because the
// count is not knowable and the ending is: a subgroup stream ends when the
// publisher closes it or when a relay resets it, and either way nothing more
// can arrive for that group on it. Audio publishes one subgroup per group, so
// one ending is the whole group.
func (o *groupOrderer) Finish(group uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.started || group < o.next {
		return
	}
	o.finished[group] = true
	if o.settling {
		// Recorded, and acted on when the window closes: draining now would
		// deliver, and the window exists so that nothing does.
		return
	}
	o.drainLocked()
}

// drainLocked advances past every group that is done and emits what that makes
// due, in order. The caller holds mu.
func (o *groupOrderer) drainLocked() {
	for {
		for len(o.held) > 0 && o.held[0].group <= o.next {
			obj := o.held[0]
			o.held = o.held[1:]
			o.emitted = true
			obj.emit()
		}
		if !o.finished[o.next] {
			return
		}
		delete(o.finished, o.next)
		o.next++
	}
}

// insertLocked places obj in held, keeping it in publisher order. The scan runs
// from the back because objects arrive in near-order: the common insert is at
// or near the end. The caller holds mu.
func (o *groupOrderer) insertLocked(obj heldGroupObject) {
	i := len(o.held)
	for i > 0 && obj.before(o.held[i-1]) {
		i--
	}
	o.held = append(o.held, heldGroupObject{})
	copy(o.held[i+1:], o.held[i:])
	o.held[i] = obj
}

// Drop forgets everything waiting, for a track that is going away. Nothing is
// emitted on the way out: the decoder these would go to is being retired in the
// next breath.
func (o *groupOrderer) Drop() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.settleTimer != nil {
		// Else the settle fires against a track that is gone and emits into a
		// decoder being retired.
		o.settleTimer.Stop()
		o.settleTimer = nil
	}
	o.settling = false
	o.held = nil
	clear(o.finished)
}

// backlog is how many objects are waiting, for tests that pin the bound.
func (o *groupOrderer) backlog() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.held)
}
