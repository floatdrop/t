package conf

import (
	"sync"
	"testing"
	"time"

	"github.com/floatdrop/moq-go/pkg/moqt/message"
	"github.com/floatdrop/moq-go/pkg/moqt/session"
)

// The router parks a data stream that arrives before its handler is registered,
// and the two can happen at once. These pin that no stream is lost between
// them, which is a bug with no symptom at the point it occurs: the stream is
// held, the handler that would have taken it is registered and never consulted,
// and orphanTTL resets it five seconds later.
//
// What that cost was a catalog. A participant publishes its first catalog
// before announcing its namespace, so a peer that subscribes afterwards can
// only get it from the Joining FETCH — and losing that response leaves the peer
// in the roster with no nickname and no media subscribed, for the rest of the
// call. Observed as one run in three under load, and in the log as "resetting
// unclaimed fetch stream" exactly five seconds before the assertion gave up.
//
// The Demux is what makes the window reachable: it releases its own lock before
// handing an unrouted stream to OnUnknown, so a registration can slip between
// the lookup that missed and the park that follows it. These drive that
// interleaving directly rather than trying to provoke it through a real
// session.

func TestRouterDeliversAFetchStreamParkedAsItsHandlerRegisters(t *testing.T) {
	const rounds = 300
	r := newRouter(testLogger(t))

	for round := range uint64(rounds) {
		id := round + 1
		stream := &session.IncomingFetchStream{
			Header: message.FetchHeader{RequestID: id},
		}

		delivered := make(chan struct{}, 1)
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Go(func() {
			<-start
			r.park(stream)
		})
		wg.Go(func() {
			<-start
			r.HandleFetch(id, func(*session.IncomingFetchStream) {
				select {
				case delivered <- struct{}{}:
				default:
				}
			})
		})
		close(start) // both released together, to interleave them
		wg.Wait()

		select {
		case <-delivered:
		case <-time.After(2 * time.Second):
			// Taken back out of the router first: a stream this test built has
			// no transport under it, so letting the orphan timer reach Cancel
			// would panic the binary five seconds into some later test.
			r.claimFetch(id, stream)
			t.Fatalf("round %d: the FETCH response for request %d was parked and "+
				"never delivered — it was parked after its handler had already "+
				"drained the list, so nothing will claim it before orphanTTL "+
				"resets it", round, id)
		}
	}
}

func TestRouterDeliversASubgroupStreamParkedAsItsHandlerRegisters(t *testing.T) {
	const rounds = 300
	r := newRouter(testLogger(t))

	for round := range uint64(rounds) {
		alias := round + 1
		stream := &session.IncomingSubgroupStream{
			Header: message.SubgroupHeader{TrackAlias: alias},
		}

		delivered := make(chan struct{}, 1)
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Go(func() {
			<-start
			r.park(stream)
		})
		wg.Go(func() {
			<-start
			r.HandleSubgroups(alias, nil, func(*session.IncomingSubgroupStream) {
				select {
				case delivered <- struct{}{}:
				default:
				}
			})
		})
		close(start)
		wg.Wait()

		select {
		case <-delivered:
		case <-time.After(2 * time.Second):
			r.claimSubgroup(alias, stream)
			t.Fatalf("round %d: the subgroup stream for alias %d was parked and "+
				"never delivered; a group of media is lost and reset at orphanTTL",
				round, alias)
		}
	}
}
