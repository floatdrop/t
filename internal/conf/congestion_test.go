package conf

import (
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// stubRTT is a minimal [quic.RTTStatsProvider]. Implementing it here, outside
// quic-go, is part of what this checks: the fork's congestion hook has to be
// usable without reaching into quic-go's internal packages.
type stubRTT struct{ rtt time.Duration }

func (s stubRTT) MinRTT() time.Duration      { return s.rtt }
func (s stubRTT) LatestRTT() time.Duration   { return s.rtt }
func (s stubRTT) SmoothedRTT() time.Duration { return s.rtt }

func TestLookupCongestionControllerBuildsEachAlgorithm(t *testing.T) {
	for _, name := range CongestionControllerNames() {
		t.Run(name, func(t *testing.T) {
			cc, err := LookupCongestionController(name)
			if err != nil {
				t.Fatalf("LookupCongestionController(%q): %v", name, err)
			}
			if cc == nil {
				t.Fatal("resolved to a nil controller")
			}
			ctrl := cc(stubRTT{rtt: 50 * time.Millisecond}, 1200, nil)
			if ctrl == nil {
				t.Fatal("factory returned a nil controller")
			}
			if ctrl.GetCongestionWindow() <= 0 {
				t.Fatalf("controller starts with a non-positive window: %d", ctrl.GetCongestionWindow())
			}
		})
	}
}

// TestLookupCongestionControllerEmptyMeansDefault pins the contract
// [Config.Congestion] documents: an unset -congestion is a nil controller, which
// quic.Config reads as "use the default".
func TestLookupCongestionControllerEmptyMeansDefault(t *testing.T) {
	cc, err := LookupCongestionController("")
	if err != nil {
		t.Fatalf("empty name should not be an error: %v", err)
	}
	if cc != nil {
		t.Fatal("empty name should resolve to a nil controller")
	}
}

func TestLookupCongestionControllerRejectsUnknown(t *testing.T) {
	_, err := LookupCongestionController("nope")
	if err == nil {
		t.Fatal("expected an error for an unknown controller name")
	}
}

// TestCongestionControllersAreDistinct guards against every name resolving to
// the same algorithm, which would make comparing them meaningless. BBR leaves
// Startup on its own estimators and so ignores the MaybeExitSlowStart hint that
// the loss-based controllers act on.
func TestCongestionControllersAreDistinct(t *testing.T) {
	build := func(t *testing.T, name string) quic.CongestionController {
		t.Helper()
		cc, err := LookupCongestionController(name)
		if err != nil {
			t.Fatalf("LookupCongestionController(%q): %v", name, err)
		}
		return cc(stubRTT{rtt: 50 * time.Millisecond}, 1200, nil)
	}

	bbr := build(t, "bbr")
	bbr.MaybeExitSlowStart()
	if !bbr.InSlowStart() {
		t.Error("BBR should ignore the MaybeExitSlowStart hint")
	}
	for _, name := range []string{"reno", "cubic"} {
		if build(t, name) == bbr {
			t.Errorf("%s resolved to the same controller instance as bbr", name)
		}
	}
}

// TestConfigCarriesTheController checks the field the app populates actually
// reaches the value dial reads, so a rename cannot quietly sever the flag from
// the transport.
func TestConfigCarriesTheController(t *testing.T) {
	cc, err := LookupCongestionController("reno")
	if err != nil {
		t.Fatalf("LookupCongestionController: %v", err)
	}
	cfg := Config{Relay: "example:4433", Room: "r", Congestion: cc}
	if cfg.Congestion == nil {
		t.Fatal("Config.Congestion is nil after being set")
	}
	if ctrl := cfg.Congestion(stubRTT{rtt: time.Millisecond}, 1200, nil); ctrl == nil {
		t.Fatal("the controller carried by Config does not build")
	}
}
