// Command shaper puts a bottleneck between one client and a relay, so the
// behaviour that only appears on a bad network can be watched on a good one.
//
// Everything this app is tested against is loopback or a relay across a
// healthy link, and the paths that matter most under congestion — the drift
// meter, the relay's overload verdict, the demotion ladder — are exactly the
// ones that never fire there. Shaping the host's own interface would need root
// and would leave firewall state behind, so the bottleneck is modelled in
// userspace: one socket faces the client, another faces the relay, and the
// relay-to-client direction is paced through a finite drop-tail queue. QUIC and
// WebTransport both ride UDP, so it does not care which is inside.
//
// Only the downlink is shaped, which is the asymmetry a call has: one stream
// going up and everyone else's coming down.
//
// Point one participant at it and leave the others on the relay directly:
//
//	go run ./cmd/shaper -relay 51.250.12.228:4433 -rate 55000
//	bin/t.dev.app/Contents/MacOS/t \
//	    -relay https://127.0.0.1:14433/moq -room demo -nickname bob -join -debug
//
// Rate is the whole experiment. Around half what the publisher offers is where
// the ladder is most visible; squeezing much harder makes the relay's verdict
// arrive *later*, not sooner, because it checks how long an object waited only
// when its writer manages to dequeue one, and a writer wedged on flow control
// rarely does.
package main

import (
	"flag"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:14433", "address the client dials")
	upstream := flag.String("relay", "", "relay host:port to forward to")
	rate := flag.Int("rate", 120_000, "downlink bottleneck, bytes per second")
	depth := flag.Int("queue", 64, "packets the bottleneck holds before dropping")
	flag.Parse()

	if *upstream == "" {
		log.Fatal("shaper: -relay is required")
	}
	remote, err := net.ResolveUDPAddr("udp", *upstream)
	if err != nil {
		log.Fatalf("shaper: resolve %s: %v", *upstream, err)
	}
	local, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		log.Fatalf("shaper: resolve %s: %v", *listen, err)
	}
	ln, err := net.ListenUDP("udp", local)
	if err != nil {
		log.Fatalf("shaper: listen: %v", err)
	}
	up, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		log.Fatalf("shaper: dial relay: %v", err)
	}

	s := &shaper{ln: ln, up: up, rate: *rate, queue: make(chan []byte, *depth)}
	log.Printf("shaper: %s -> %s at %d bytes/s, queue %d",
		ln.LocalAddr(), remote, *rate, *depth)

	go s.fromClient()
	go s.fromRelay()
	go s.drain()

	for range time.Tick(2 * time.Second) {
		log.Printf("shaper: passed=%d dropped=%d", s.passed.Load(), s.dropped.Load())
	}
}

type shaper struct {
	ln    *net.UDPConn
	up    *net.UDPConn
	queue chan []byte
	rate  int

	mu     sync.Mutex
	client *net.UDPAddr
	next   time.Time

	passed  atomic.Uint64
	dropped atomic.Uint64
}

// fromClient forwards the uplink untouched.
func (s *shaper) fromClient() {
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

// fromRelay queues the downlink, dropping from the tail when it is full —
// which is what a full queue at a real bottleneck does, and what makes this
// produce loss as well as delay.
func (s *shaper) fromRelay() {
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
		s.next = s.next.Add(time.Duration(float64(len(pkt)) / float64(s.rate) * float64(time.Second)))
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
