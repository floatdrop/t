package conf

import (
	"testing"
	"time"
)

// How long video stays off after being given up on has to move in both
// directions. Growing is what stops a link that cannot carry video from
// bouncing off the reduced state every thirty seconds; shrinking is what stops
// one bad minute from being held against a call for the rest of the hour.
//
// Only the growing half existed for a while. Nothing lowered the wait, so two
// congested minutes early on pinned it near the ceiling, and an hour of a
// perfect link later a single burst still cost a two-minute blank tile —
// exactly what videoRecoveryMax's reasoning says must not happen.
func TestRecoveryWaitGrowsAndShrinks(t *testing.T) {
	t.Run("starts at the short wait", func(t *testing.T) {
		if got := nextRecoveryWait(0, 0, false); got != videoRecovery {
			t.Errorf("first wait = %v, want %v", got, videoRecovery)
		}
	})

	t.Run("doubles when video did not last the wait it was given", func(t *testing.T) {
		got := nextRecoveryWait(videoRecovery, videoRecovery/2, true)
		if want := 2 * videoRecovery; got != want {
			t.Errorf("wait after a short-lived recovery = %v, want %v", got, want)
		}
	})

	t.Run("stops doubling at the ceiling", func(t *testing.T) {
		got := nextRecoveryWait(videoRecoveryMax, time.Second, true)
		if got != videoRecoveryMax {
			t.Errorf("wait = %v, want it capped at %v", got, videoRecoveryMax)
		}
	})

	t.Run("resets once video has held longer than the wait", func(t *testing.T) {
		// The link demonstrably recovered: video came back and outlasted the
		// wait that preceded it. Holding the old wait against it is what left
		// tiles black for minutes on a link that was fine.
		got := nextRecoveryWait(videoRecoveryMax, videoRecoveryMax+time.Second, true)
		if got != videoRecovery {
			t.Errorf("wait after a recovery that held = %v, want %v back at the floor",
				got, videoRecovery)
		}
	})

	t.Run("resets when there has been no recovery to judge", func(t *testing.T) {
		if got := nextRecoveryWait(2*videoRecovery, 0, false); got != videoRecovery {
			t.Errorf("wait = %v, want %v", got, videoRecovery)
		}
	})
}
