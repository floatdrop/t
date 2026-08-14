package conf

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/floatdrop/moq-go/pkg/moqt/loc"
	"github.com/floatdrop/moq-go/pkg/moqt/message"
	"github.com/floatdrop/moq-go/pkg/moqt/session"
	"github.com/floatdrop/moq-go/pkg/moqt/session/quicconn"
	"github.com/floatdrop/moq-go/pkg/moqt/wire"
)

// The emission index moved off 0x8002 onto 0x3802 — out of the registrable
// range and into the one §2.5 reserves for applications. These cover the
// transition, which is the part with teeth: the property is what reassembly
// keys on, and a peer that cannot find it falls back to the Object ID, which
// for layered video is strided by layerObjectStride and orders nothing. A
// half-done move would therefore manufacture exactly the macroblock garbage
// reorder.go exists to prevent, and only in calls that mix builds.

func TestEmissionIndexAcceptsBothCodePoints(t *testing.T) {
	tests := []struct {
		name     string
		extras   []wire.KVPair
		objectID uint64
		want     uint64
	}{{
		name:     "current code point",
		extras:   []wire.KVPair{{Type: propEmissionIndex, IntVal: 7}},
		objectID: 99,
		want:     7,
	}, {
		// A publisher from before 0.6.3. Reading it is the whole reason the
		// legacy constant still exists.
		name:     "legacy code point only",
		extras:   []wire.KVPair{{Type: propEmissionIndexLegacy, IntVal: 7}},
		objectID: 99,
		want:     7,
	}, {
		// What a current publisher sends. They always agree, so this only
		// pins which one is authoritative if they ever stop agreeing.
		name: "both, current wins",
		extras: []wire.KVPair{
			{Type: propEmissionIndex, IntVal: 7},
			{Type: propEmissionIndexLegacy, IntVal: 7},
		},
		objectID: 99,
		want:     7,
	}, {
		// The flat publisher: one subgroup per group, IDs counting from zero
		// without gaps, which are the emission order.
		name:     "neither, falls back to the object ID",
		extras:   nil,
		objectID: 4,
		want:     4,
	}, {
		// Order is not guaranteed on the wire — wire.Writer.KVPairs sorts by
		// type — but a reader that returned on first match of the wrong one
		// would still pass the "both" case above, so reverse it here.
		name: "both, legacy listed first",
		extras: []wire.KVPair{
			{Type: propEmissionIndexLegacy, IntVal: 3},
			{Type: propEmissionIndex, IntVal: 3},
		},
		objectID: 99,
		want:     3,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := emissionIndex(tc.objectID, loc.Properties{Extras: tc.extras})
			if got != tc.want {
				t.Errorf("emissionIndex = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestEmissionIndexCodePointsAreDistinctAndReserved pins the choice itself.
//
// The bug being fixed was a plausible-looking constant, so the constraints it
// violated are asserted rather than left in a comment: 0x8002 sat in the
// registrable range, where a future registration could give a relay
// processing rules for these objects' properties.
func TestEmissionIndexCodePointsAreDistinctAndReserved(t *testing.T) {
	if propEmissionIndex == propEmissionIndexLegacy {
		t.Fatal("the two code points are the same; the move did not happen")
	}
	// §2.5's two-byte application-specific range, which IANA never allocates.
	if propEmissionIndex < 0x3800 || propEmissionIndex > 0x3FFF {
		t.Errorf("propEmissionIndex %#x is outside the application range [0x3800, 0x3FFF] (§2.5)",
			propEmissionIndex)
	}
	// Even, so the value rides as a varint rather than as bytes (§1.4.3).
	if propEmissionIndex%2 != 0 {
		t.Errorf("propEmissionIndex %#x is odd, so it would carry bytes not a varint (§1.4.3)",
			propEmissionIndex)
	}
	// §14: GREASE values are 0x7F*N + 0x9D, and a peer may discard one.
	if (propEmissionIndex-0x9D)%0x7F == 0 {
		t.Errorf("propEmissionIndex %#x is a GREASE value; a peer may discard it (§14)",
			propEmissionIndex)
	}
	// §2.5.1: inside [0x4000, 0x7FFF] an endpoint that did not understand it
	// would have to drop the track.
	if propEmissionIndex >= 0x4000 && propEmissionIndex <= 0x7FFF {
		t.Errorf("propEmissionIndex %#x is a Mandatory Track Property (§2.5.1)", propEmissionIndex)
	}
}

// TestPublishedObjectsCarryBothEmissionIndexCodePoints is the writer half,
// over a real relay.
//
// A unit test of writeObject would prove the slice has two entries; this
// proves both survive encoding, the relay, and decoding — which is what a peer
// on the other side of the move actually depends on. It is also what fails if
// the legacy pair is dropped before the builds that need it are gone.
func TestPublishedObjectsCarryBothEmissionIndexCodePoints(t *testing.T) {
	relayServer := startRelay(t)
	addr := relayServer.Addr()
	alice := layeredPublisher(t, addr, "emission", "alice")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	sess := rawSubscriber(t, ctx, addr)
	defer sess.Close(0, "bye")

	sub, err := sess.Subscribe(ctx, &message.Subscribe{
		Namespace:  alice.pub.ns,
		Name:       []byte(VideoTrack),
		Parameters: message.Parameters{message.LargestObjectFilter()},
	})
	if err != nil {
		t.Fatalf("SUBSCRIBE %s: %v", VideoTrack, err)
	}
	defer sub.Close()

	seen := make(chan observation, 64)
	demux := session.NewDemux()
	// Each subgroup on its own goroutine, as router.HandleSubgroups does in
	// the app. session.Demux dispatches inline, and these streams stay open
	// for the life of the group — so an inline handler would hold the accept
	// loop on the base layer and the enhancement subgroup would never be
	// read at all.
	demux.HandleTrack(sub.TrackAlias(), func(s *session.IncomingSubgroupStream) {
		go readObservations(s, seen)
	})
	go demux.Run(ctx, sess)

	// Subscribed first, then published: the relay does not replay a group to
	// a subscription that arrived after it. Published on this goroutine
	// because publishLayeredGroup fails the test itself, and the reader keeps
	// draining into the buffered channel meanwhile.
	publishLayeredGroup(t, alice, 0, 6)

	// Six: the group's three base-layer objects and its three enhancement
	// ones. Both subgroups matter — the enhancement layer is the half whose
	// object IDs are strided, so it is the half the fallback would mis-order.
	const want = 6
	for i := range want {
		select {
		case got := <-seen:
			if got.decodeErr != nil {
				t.Fatalf("reading object %d: %v", i, got.decodeErr)
			}
			if !got.haveCurrent {
				t.Errorf("object %d/%d carries no emission index at %#x",
					got.group, got.object, propEmissionIndex)
			}
			if !got.haveLegacy {
				t.Errorf("object %d/%d carries no emission index at the legacy %#x, "+
					"so a peer from before 0.6.3 would fall back to the object ID",
					got.group, got.object, propEmissionIndexLegacy)
			}
			if got.haveCurrent && got.haveLegacy && got.current != got.legacy {
				t.Fatalf("object %d/%d: emission index %d at %#x but %d at %#x — the two "+
					"stamps disagree, so peers on either side of the move would order "+
					"the same group differently",
					got.group, got.object,
					got.current, propEmissionIndex, got.legacy, propEmissionIndexLegacy)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d objects arrived", i, want)
		}
	}
}

// observation is one Object as the raw subscriber saw it. The reader makes no
// assertions of its own — it runs on a goroutine that outlives the test body,
// so everything it sees is carried back and judged on the test goroutine.
type observation struct {
	group, object           uint64
	current, legacy         uint64
	haveCurrent, haveLegacy bool
	decodeErr               error
}

// readObservations drains one subgroup stream, reporting each Object's
// emission-index stamps.
func readObservations(s *session.IncomingSubgroupStream, seen chan<- observation) {
	send := func(o observation) {
		select {
		case seen <- o:
		default: // the test has what it needs
		}
	}
	for {
		obj, err := s.ReadDecoded()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				send(observation{decodeErr: err})
			}
			return
		}
		decoded, err := loc.Decode(obj.Properties, obj.Payload)
		if err != nil {
			send(observation{decodeErr: err})
			return
		}
		got := observation{group: obj.GroupID, object: obj.ObjectID}
		for _, kv := range decoded.Properties.Extras {
			switch kv.Type {
			case propEmissionIndex:
				got.current, got.haveCurrent = kv.IntVal, true
			case propEmissionIndexLegacy:
				got.legacy, got.haveLegacy = kv.IntVal, true
			}
		}
		send(got)
	}
}

// rawSubscriber opens a plain MOQT session against the test relay, below the
// Room API, so a test can look at what is actually on the wire.
func rawSubscriber(t *testing.T, ctx context.Context, addr string) *session.Session {
	t.Helper()
	conn, err := quicconn.Dial(ctx, addr,
		&tls.Config{
			NextProtos: []string{alpnDraft19},
			//nolint:gosec // G402: the test relay's certificate is self-signed and generated per run.
			InsecureSkipVerify: true,
		},
		&quic.Config{EnableDatagrams: true})
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	sess, err := session.Client(ctx, conn, session.WithImplementation("t-test/0.1"))
	if err != nil {
		t.Fatalf("MOQT setup: %v", err)
	}
	return sess
}
