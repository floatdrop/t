package conf

import (
	"testing"
	"time"

	"t/internal/bridge"
)

// Ordering across a group boundary, which reorder.go's unit tests and the
// layered ones over a relay both leave alone on purpose: they filter to a
// single group, because a group's own streams are where the two concurrent
// readers are.
//
// The boundary is a second place decode order can be lost, and a worse one. A
// reassembler is per group, so nothing compares an object against a group other
// than its own — and remoteTrack keeps a group's reassembler alive for one whole
// group *after* the publisher has moved on, precisely so a tail still in flight
// can still be placed. Placed, and then emitted: straight to a decoder that is
// already past the next group's keyframe.
//
// A keyframe is an IDR, and an IDR empties the decoded picture buffer. A frame
// of the previous group arriving after it references pictures the decoder no
// longer holds, so it decodes to garbage or not at all — one flash of
// macroblocks per group boundary it happens on, which on a five-second GOP is
// once every five seconds.
//
// A burst is what makes it show. Nothing then paces the two subgroup streams
// against each other or a group's streams against the next group's, and the
// relay drains whichever it likes: the same freedom §11.4.2 gives it, arriving
// all at once instead of a frame at a time.

// TestGroupsDoNotOverlapAtTheDecoder publishes several layered groups back to
// back and requires the frames that reach the frontend to be in decode order
// across the whole run, not merely within each group.
//
// Timestamps are the assertion because they are what the decoder is handed. A
// frame whose timestamp is below one already delivered is a frame the decoder
// is past, and no player recovers from being handed one.
func TestGroupsDoNotOverlapAtTheDecoder(t *testing.T) {
	alice, bobRec := layeredPair(t, "crossgroup1")

	const (
		groups   = 4
		perGroup = 10 // even, so each group ends on the enhancement layer
		total    = groups * perGroup
	)
	// Written as fast as the bridge takes it. Paced at the frame rate each
	// group has drained well before the next one opens, which is the case that
	// already works; the burst is the publisher catching up after a stall.
	for i := range total {
		f := layeredVideoFrame(
			uint64(i)*frameStep, i%perGroup == 0, 300, temporalLayerFor(i%perGroup))
		if err := alice.WriteFrame(f); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}
	// Closes the last group, so nothing under test is still open.
	closer := uint64(total) * frameStep
	if err := alice.WriteFrame(
		layeredVideoFrame(closer, true, 300, 0)); err != nil {
		t.Fatalf("write the keyframe that closes the last group: %v", err)
	}

	waitFor(t, "the burst to reach bob", 20*time.Second, func() bool {
		return len(videoFrames(bobRec)) > 0
	})
	time.Sleep(3 * time.Second) // let the rest of it land

	got := videoFrames(bobRec)
	if len(got) < perGroup {
		t.Fatalf("only %d video frames arrived; too few to say anything about "+
			"ordering across %d groups", len(got), groups)
	}

	// Reported rather than failed on one and stopped: how many boundaries
	// invert, and by how far, is the difference between a rare race and a
	// structural one.
	var inversions int
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp > got[i-1].Timestamp {
			continue
		}
		inversions++
		if inversions <= 5 {
			t.Errorf("frame %d went backwards: timestamp %d (layer %d) after %d "+
				"(layer %d) — a frame of an earlier group behind the next group's "+
				"keyframe, which the decoder has no reference for",
				i, got[i].Timestamp, got[i].TemporalLayer,
				got[i-1].Timestamp, got[i-1].TemporalLayer)
		}
	}
	if inversions > 0 {
		t.Errorf("%d of %d delivered frames arrived out of decode order across "+
			"group boundaries", inversions, len(got))
	}
	t.Logf("%d of %d frames delivered, %d out of order", len(got), total, inversions)

	// A frame delivered twice is the other way a decoder is handed something it
	// is past, and a recreated reassembler is how it would happen: a group
	// retired while its stream was still draining starts counting from zero
	// again.
	seen := make(map[uint64]bool, len(got))
	for _, f := range got {
		if seen[f.Timestamp] {
			t.Errorf("frame at %d was delivered twice", f.Timestamp)
		}
		seen[f.Timestamp] = true
	}

	var base int
	for _, f := range got {
		if f.TemporalLayer == 0 {
			base++
		}
	}
	if base == 0 {
		t.Error("no base-layer frames arrived at all")
	}
}

// TestPacedGroupsLoseNothingToTheBoundary is the other half of the burst test,
// and the one that says what ordering the groups costs.
//
// Holding an object for a group still in flight is right when it is needed and
// latency added to a live call when it is not. At the frame rate a group's base
// stream has ended well before the next group opens, so nothing should ever be
// held and nothing should be dropped — if either happens here, the cure is
// worse than the artefact it prevents.
func TestPacedGroupsLoseNothingToTheBoundary(t *testing.T) {
	alice, bobRec := layeredPair(t, "crossgroup2")

	const (
		groups   = 3
		perGroup = 10 // even, so each group ends on the enhancement layer
		total    = groups * perGroup
	)
	for g := range groups {
		publishLayeredGroup(t, alice, uint64(g*perGroup)*frameStep, perGroup)
	}
	closer := uint64(total) * frameStep
	if err := alice.WriteFrame(
		layeredVideoFrame(closer, true, 300, 0)); err != nil {
		t.Fatalf("write the keyframe that closes the last group: %v", err)
	}

	closed := func() []bridge.MediaFrame {
		var got []bridge.MediaFrame
		for _, f := range videoFrames(bobRec) {
			if f.Timestamp < closer {
				got = append(got, f)
			}
		}
		return got
	}
	waitFor(t, "every frame of every closed group to reach bob", 10*time.Second, func() bool {
		return len(closed()) >= total
	})

	got := closed()
	if len(got) != total {
		t.Errorf("bob received %d of %d paced frames; ordering the groups is "+
			"costing frames a decoder could have used", len(got), total)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp <= got[i-1].Timestamp {
			t.Errorf("frame %d went backwards: timestamp %d after %d",
				i, got[i].Timestamp, got[i-1].Timestamp)
		}
	}
}

// audioPair brings up a publisher with an audio track and a subscriber watching
// it. Audio alone, because audio is what the group orderer is for and a video
// track alongside it would only add streams to the same connection.
func audioPair(t *testing.T, room string) (*Room, *recorder) {
	t.Helper()
	relayServer := startRelay(t)
	addr := relayServer.Addr()

	alice, _ := joinRoom(t, addr, room, "alice")
	if err := alice.DeclareTrack(&bridge.TrackConfig{
		Kind: "audio", Codec: "opus", SampleRate: 48000, Channels: 1,
	}); err != nil {
		t.Fatalf("declare audio: %v", err)
	}
	_, bobRec := joinRoom(t, addr, room, "bob")
	waitFor(t, "bob to subscribe to alice's audio", subscribeWait, func() bool {
		_, tracks, _, _ := bobRec.snapshot()
		return len(tracks) == 1
	})
	return alice, bobRec
}

// audioFrames pulls the received audio out of a recorder, in arrival order.
func audioFrames(rec *recorder) []bridge.MediaFrame {
	frames, _, _, _ := rec.snapshot()
	var got []bridge.MediaFrame
	for _, f := range frames {
		if f.Kind == bridge.KindAudio {
			got = append(got, f)
		}
	}
	return got
}

// TestAudioGroupsArriveInOrderAfterABurst is the audio half of the boundary.
//
// Audio rotates its group every audioGroupObjects frames rather than on a
// keyframe, and each group is a stream of its own, so a burst puts three of them
// in flight at once and the relay drains whichever it likes. Nothing about that
// is loss — every object arrives — but a stretch of sound delivered behind one
// published a second later is played in the order it lands, and the ring buffer
// in the player worklet is contiguous by construction, so it has no way to put
// it back.
//
// Waited for rather than dropped, which is the opposite of what video does at
// the same boundary and for the reason grouporder.go gives: an Opus packet
// stands on its own, so the late one is playable and a gap would be the more
// expensive answer. Nothing may be lost here.
func TestAudioGroupsArriveInOrderAfterABurst(t *testing.T) {
	alice, bobRec := audioPair(t, "crossgroup3")

	const total = 3 * audioGroupObjects
	for i := range total {
		if err := alice.WriteFrame(audioFrame(uint64(i)*20_000, 120)); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}
	waitFor(t, "the burst to reach bob", 15*time.Second, func() bool {
		return len(audioFrames(bobRec)) >= total
	})
	time.Sleep(time.Second) // let anything still coming land

	got := audioFrames(bobRec)
	if len(got) != total {
		t.Errorf("bob received %d of %d audio frames; ordering the groups must "+
			"not cost any, because a gap in the sound is the thing it is "+
			"cheaper to avoid", len(got), total)
	}
	var inversions int
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp > got[i-1].Timestamp {
			continue
		}
		inversions++
		if inversions <= 5 {
			t.Errorf("frame %d went backwards: timestamp %d after %d — a group "+
				"delivered behind one published later, which the player's ring "+
				"buffer cannot reorder",
				i, got[i].Timestamp, got[i-1].Timestamp)
		}
	}
	t.Logf("%d of %d audio frames delivered, %d out of order", len(got), total, inversions)
}

// TestPacedAudioWaitsForNothing is what the orderer costs when it is not needed.
//
// At the 20 ms cadence a group's stream has ended well before the next group's
// first object arrives, so nothing should ever be held: this is latency added to
// a live conversation, and it has to be zero in the case that is every call.
func TestPacedAudioWaitsForNothing(t *testing.T) {
	alice, bobRec := audioPair(t, "crossgroup4")

	const total = 2 * audioGroupObjects
	for i := range total {
		if err := alice.WriteFrame(audioFrame(uint64(i)*20_000, 120)); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, "the audio to reach bob", 10*time.Second, func() bool {
		return len(audioFrames(bobRec)) >= total
	})

	got := audioFrames(bobRec)
	if len(got) != total {
		t.Errorf("bob received %d of %d paced audio frames", len(got), total)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp <= got[i-1].Timestamp {
			t.Errorf("frame %d went backwards: timestamp %d after %d",
				i, got[i].Timestamp, got[i-1].Timestamp)
		}
	}
}
