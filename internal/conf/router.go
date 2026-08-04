package conf

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/floatdrop/moq-go/pkg/moqt"
	"github.com/floatdrop/moq-go/pkg/moqt/session"
)

// orphanTTL is how long a data stream that arrived before its handler was
// registered is held before being reset. It only has to cover the gap
// between a request's response and the caller registering its handler —
// microseconds in practice — so this window is generous.
const orphanTTL = 5 * time.Second

// router owns the session's inbound data-stream routing. It wraps
// [session.Demux] to close two gaps that matter for live media.
//
// First, Demux invokes a handler *synchronously* on its accept loop. Our
// handlers read a subgroup stream until it ends, and a live subgroup lasts
// as long as its group — a whole GOP for video. Registering such a handler
// directly lets one open stream block every other stream on the session:
// audio stops arriving for as long as a video group stays open. router
// therefore runs each handler on its own goroutine.
//
// Second, a stream can arrive before its handler exists. A subscriber only
// learns its Track Alias from SUBSCRIBE_OK, and a FETCH's Request ID is
// assigned inside the call, so there is a window — one relay hop wide —
// where the publisher's first stream is already inbound while the caller
// has registered nothing for it. Demux would send those down the unknown
// path to be reset, losing a group (or, for the catalog backfill, the whole
// catalog). router parks them briefly instead and delivers them as soon as
// the matching handler registers.
type router struct {
	log   *slog.Logger
	demux *session.Demux

	mu sync.Mutex
	// subgroups holds streams whose Track Alias has no handler yet;
	// fetches does the same by Request ID.
	subgroups map[uint64][]*session.IncomingSubgroupStream
	fetches   map[uint64][]*session.IncomingFetchStream
}

func newRouter(log *slog.Logger) *router {
	r := &router{
		log:       log,
		demux:     session.NewDemux(),
		subgroups: make(map[uint64][]*session.IncomingSubgroupStream),
		fetches:   make(map[uint64][]*session.IncomingFetchStream),
	}
	r.demux.OnUnknown(r.park)
	return r
}

// HandleSubgroups routes streams bearing alias to h, each on its own
// goroutine, and delivers any stream already parked under that alias.
func (r *router) HandleSubgroups(alias uint64, h func(*session.IncomingSubgroupStream)) {
	r.demux.HandleTrack(alias, func(s *session.IncomingSubgroupStream) { go h(s) })

	r.mu.Lock()
	parked := r.subgroups[alias]
	delete(r.subgroups, alias)
	r.mu.Unlock()

	for _, s := range parked {
		r.log.Debug("delivering parked subgroup stream", "alias", alias)
		go h(s)
	}
}

// HandleFetch routes the FETCH response for requestID to h, on its own
// goroutine, and delivers a response that has already arrived.
func (r *router) HandleFetch(requestID uint64, h func(*session.IncomingFetchStream)) {
	r.demux.HandleFetch(requestID, func(s *session.IncomingFetchStream) { go h(s) })

	r.mu.Lock()
	parked := r.fetches[requestID]
	delete(r.fetches, requestID)
	r.mu.Unlock()

	for _, s := range parked {
		r.log.Debug("delivering parked fetch stream", "request_id", requestID)
		go h(s)
	}
}

// Run drives the accept loop until ctx is cancelled or the session ends.
func (r *router) Run(ctx context.Context, sess *session.Session) error {
	return r.demux.Run(ctx, sess)
}

// park holds a stream that arrived before its handler was registered and
// schedules its expiry. Anything still parked when orphanTTL passes was
// never claimed, so it is reset rather than left holding flow control open.
func (r *router) park(ds session.DataStream) {
	switch s := ds.(type) {
	case *session.IncomingSubgroupStream:
		alias := s.Header.TrackAlias
		r.mu.Lock()
		r.subgroups[alias] = append(r.subgroups[alias], s)
		r.mu.Unlock()
		time.AfterFunc(orphanTTL, func() {
			if r.claimSubgroup(alias, s) {
				r.log.Debug("resetting unclaimed subgroup stream", "alias", alias)
				s.Cancel(moqt.StreamResetInternalError)
			}
		})

	case *session.IncomingFetchStream:
		id := s.Header.RequestID
		r.mu.Lock()
		r.fetches[id] = append(r.fetches[id], s)
		r.mu.Unlock()
		time.AfterFunc(orphanTTL, func() {
			if r.claimFetch(id, s) {
				r.log.Debug("resetting unclaimed fetch stream", "request_id", id)
				s.Cancel(moqt.StreamResetInternalError)
			}
		})

	default:
		r.log.Debug("resetting unroutable data stream")
		ds.Cancel(moqt.StreamResetInternalError)
	}
}

// claimSubgroup removes s from the parked list and reports whether it was
// still there — false means a handler already took it.
func (r *router) claimSubgroup(alias uint64, s *session.IncomingSubgroupStream) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.subgroups[alias]
	for i, candidate := range list {
		if candidate != s {
			continue
		}
		r.storeSubgroups(alias, append(list[:i], list[i+1:]...))
		return true
	}
	return false
}

func (r *router) claimFetch(id uint64, s *session.IncomingFetchStream) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.fetches[id]
	for i, candidate := range list {
		if candidate != s {
			continue
		}
		if remaining := append(list[:i], list[i+1:]...); len(remaining) == 0 {
			delete(r.fetches, id)
		} else {
			r.fetches[id] = remaining
		}
		return true
	}
	return false
}

// storeSubgroups writes back a pruned list, dropping the key when empty so
// the map does not accumulate entries for finished tracks.
func (r *router) storeSubgroups(alias uint64, list []*session.IncomingSubgroupStream) {
	if len(list) == 0 {
		delete(r.subgroups, alias)
		return
	}
	r.subgroups[alias] = list
}
