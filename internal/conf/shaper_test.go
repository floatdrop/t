package conf

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shaper is a UDP forwarder that puts a bottleneck in front of the relay, so a
// test can watch what this client does on a link that cannot carry what is
// being sent to it.
//
// Everything else in this package runs over loopback, where there is no queue
// to fill and no packet to lose — which is exactly the condition under which
// the inbound signals this exercises never fire. Shaping the host's own
// interface would need root and would leave firewall state behind, so the
// bottleneck is modelled in userspace instead: one socket faces the client,
// another faces the relay, and the relay-to-client direction is paced through
// a finite drop-tail queue.
//
// Only that direction is shaped. The publisher in these tests reaches the
// relay directly; it is the subscriber's downlink that is squeezed, which is
// the asymmetry a real call has — one stream up, everyone else's coming down.
type shaper struct {
	t *testing.T

	ln *net.UDPConn // faces the client
	up *net.UDPConn // faces the relay

	queue chan []byte

	// rate is the bottleneck in bytes per second.
	rate int

	mu     sync.Mutex
	client *net.UDPAddr
	// next is when the pacer may send again — the token bucket, kept as a
	// deadline rather than a balance.
	next time.Time

	dropped atomic.Uint64
	passed  atomic.Uint64

	closeOnce sync.Once
}

// startShaper puts a bottleneck of bytesPerSec in front of relayAddr, holding
// at most queueDepth packets before it starts dropping. It returns once the
// forwarder is listening.
func startShaper(t *testing.T, relayAddr string, bytesPerSec, queueDepth int) *shaper {
	t.Helper()

	upstream, err := net.ResolveUDPAddr("udp", relayAddr)
	if err != nil {
		t.Fatalf("resolve relay %q: %v", relayAddr, err)
	}
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("shaper listen: %v", err)
	}
	up, err := net.DialUDP("udp", nil, upstream)
	if err != nil {
		t.Fatalf("shaper dial relay: %v", err)
	}

	s := &shaper{
		t:     t,
		ln:    ln,
		up:    up,
		queue: make(chan []byte, queueDepth),
		rate:  bytesPerSec,
	}
	go s.readFromClient()
	go s.readFromRelay()
	go s.drain()
	t.Cleanup(s.Close)
	return s
}

// Addr is what a client should dial instead of the relay.
func (s *shaper) Addr() string { return s.ln.LocalAddr().String() }

// Stats reports how many packets reached the client and how many the
// bottleneck's queue had to drop.
func (s *shaper) Stats() (passed, dropped uint64) {
	return s.passed.Load(), s.dropped.Load()
}

// readFromClient forwards the uplink untouched.
func (s *shaper) readFromClient() {
	buf := make([]byte, 65536)
	for {
		n, addr, err := s.ln.ReadFromUDP(buf)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.client = addr
		s.mu.Unlock()
		if _, err := s.up.Write(buf[:n]); err != nil {
			return
		}
	}
}

// readFromRelay puts the downlink into the bottleneck's queue, dropping from
// the tail when it is full — which is what a full queue at a real bottleneck
// does, and what makes this produce loss as well as delay.
func (s *shaper) readFromRelay() {
	buf := make([]byte, 65536)
	for {
		n, err := s.up.Read(buf)
		if err != nil {
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		select {
		case s.queue <- pkt:
		default:
			s.dropped.Add(1)
		}
	}
}

// drain paces the queue out at the bottleneck rate.
func (s *shaper) drain() {
	for pkt := range s.queue {
		s.mu.Lock()
		client := s.client
		now := time.Now()
		if s.next.Before(now) {
			s.next = now
		}
		s.next = s.next.Add(time.Duration(
			float64(len(pkt)) / float64(s.rate) * float64(time.Second)))
		wait := time.Until(s.next)
		s.mu.Unlock()

		if wait > 0 {
			time.Sleep(wait)
		}
		if client == nil {
			continue
		}
		if _, err := s.ln.WriteToUDP(pkt, client); err != nil {
			return
		}
		s.passed.Add(1)
	}
}

func (s *shaper) Close() {
	s.closeOnce.Do(func() {
		_ = s.ln.Close()
		_ = s.up.Close()
	})
}
