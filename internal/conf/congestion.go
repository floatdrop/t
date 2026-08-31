package conf

import (
	"fmt"
	"maps"
	"slices"

	"github.com/quic-go/quic-go"
)

// A CongestionController builds the QUIC congestion controller for a
// connection. It is [quic.CongestionControllerFactory] under a local name, so
// that callers above this package can carry one without importing quic-go.
type CongestionController = quic.CongestionControllerFactory

// congestionControllers are the algorithms a call can be run with.
//
// This machine is one end of a live media flow on whatever network the user
// happens to be on, which is the case the choice matters for. Reno treats every
// lost packet as congestion and halves its window; on a path that loses or
// reorders packets for reasons that are not congestion — wifi, cellular, a
// tunnel — that pins the window near its floor and the outgoing camera and
// microphone stall behind it. BBRv3 paces at a gain-scaled multiple of the
// delivery rate it measures and treats loss under 2% as noise, and it targets a
// shorter queue at the bottleneck, which is latency this app pays for directly.
//
// Selecting one needs quic.Config.Congestion, which is not a field upstream
// quic-go has; see the module's replace directive.
var congestionControllers = map[string]CongestionController{
	"bbr":   quic.NewBBRv3,
	"reno":  quic.NewReno,
	"cubic": quic.NewCubic,
}

// CongestionControllerNames lists the accepted controller names, sorted, for
// flag help and error messages.
func CongestionControllerNames() []string {
	return slices.Sorted(maps.Keys(congestionControllers))
}

// LookupCongestionController resolves a controller name. An empty name selects
// quic-go's default, reported as a nil controller, which [Config.Congestion]
// documents as "leave it to the transport".
func LookupCongestionController(name string) (CongestionController, error) {
	if name == "" {
		return nil, nil
	}
	cc, ok := congestionControllers[name]
	if !ok {
		return nil, fmt.Errorf("unknown congestion controller %q (want one of %v)",
			name, CongestionControllerNames())
	}
	return cc, nil
}
