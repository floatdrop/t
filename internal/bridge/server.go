package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// maxFrameBytes bounds a single inbound bridge frame. A 4 MB ceiling is
// far above any single encoded video frame while still bounding what a
// runaway frontend can make the backend buffer.
const maxFrameBytes = 4 << 20

// The outbound media queues, one per medium. Media is live: when a queue is
// full the oldest frames are worthless, so enqueuing discards them rather than
// blocking the MOQ read path (see Server.SendMedia).
//
// One queue per medium, because they were sharing 256 slots and a shared queue
// is a coupling: video is 1.5 Mbps against audio's 32 kbps, so a video backlog
// both delayed audio behind it and evicted audio to make room for itself. Every
// time the WebView stopped reading — macOS resizing its window, a busy main
// thread — the sound came back as a burst, which the player then trimmed to
// bound its latency, and each trim is an audible chop.
//
// What bounds each queue is the **age** of what is in it, not how many slots it
// has. Slots were the first answer and they were the wrong unit, because a queue
// is per connection and carries every remote participant at once: 64 video slots
// is a couple of seconds of one stream but 0.7 s of three, so the depth that was
// supposed to guarantee a keyframe stopped doing so in exactly the calls where
// the WebView is most likely to stall. Sizing the slots for the worst case
// instead would make a two-party call carry a latency reservoir it never needs.
//
// Age says what was actually meant, and says it whatever the participant count:
// a frame this old is not worth sending, because the frontend paints the newest
// of what it is given and throws the rest away.
const (
	// Two keyframe intervals, so what survives always contains a keyframe and a
	// frontend coming back from a stall resumes on one instead of decoding
	// deltas against references it never received.
	videoMaxAge = 2 * time.Second
	// Under the player's own 250 ms ceiling, so the bridge gives up on stale
	// sound before the ring buffer has to trim it. Both are audible; this one
	// is shorter and it moves a counter.
	audioMaxAge = 200 * time.Millisecond
)

// Slot counts are a memory backstop rather than the policy, deep enough that age
// is normally what decides — several participants' worth at each cadence.
const (
	videoQueueDepth = 512
	audioQueueDepth = 128
)

// controlQueueDepth bounds the outbound control queue, which is separate
// because control messages are not interchangeable the way frames are. A
// dropped remoteTrack announcement is a participant who never gets a decoder;
// a dropped state is a session phase the frontend never learns about.
//
// Comfortably above telemetry.logRingSize, because HandleConnect replays the
// whole ring into this queue the moment a frontend attaches, before it has
// read anything. Sized under that and a reconnecting WebView would overflow
// its own backlog on arrival.
const controlQueueDepth = 4096

// writeTimeout bounds one WebSocket write. Without it a frontend that has
// stopped reading — which one does, for as long as macOS is resizing its
// window — blocks the write loop on a send that has nowhere to go, and the
// queues behind it fill with nothing draining them.
const writeTimeout = 10 * time.Second

// Handler consumes what arrives from the frontend. Both methods are
// called from the connection's read goroutine, one at a time.
type Handler interface {
	// HandleControl processes one JSON control message.
	HandleControl(ctx context.Context, msg *ClientMessage) error
	// HandleMedia processes one encoded media frame. The frame's Config
	// and Payload alias the read buffer and are invalid once
	// HandleMedia returns; implementations that retain them must copy.
	HandleMedia(ctx context.Context, f *MediaFrame) error
	// HandleConnect runs once the frontend's connection is established,
	// before any of its messages are read.
	HandleConnect()
	// HandleDisconnect runs when the frontend's connection ends.
	HandleDisconnect()
}

// Server is the loopback WebSocket endpoint the WebView connects to. It
// accepts a single frontend connection at a time — a later connection
// (a reload, say) displaces the earlier one.
type Server struct {
	log     *slog.Logger
	handler Handler

	listener net.Listener
	token    string

	mu   sync.Mutex
	conn *conn
}

type conn struct {
	ws *websocket.Conn
	// Three queues. Control is separate from media because the two fail
	// differently: a frame that cannot be sent should be abandoned, and a
	// control message never should. Video is separate from audio so that
	// neither medium's backlog can delay or evict the other.
	video   chan outbound
	audio   chan outbound
	control chan outbound
	ctx     context.Context
	cancel  context.CancelFunc

	droppedVideo atomic.Uint64
	droppedAudio atomic.Uint64
}

// outbound is one queued frame plus the WebSocket opcode to send it
// under, so the write loop never has to guess from the payload bytes.
type outbound struct {
	typ  websocket.MessageType
	data []byte
	// queued is when this went into a media queue, so the write loop can tell
	// a frame worth sending from one the frontend would only throw away. Zero
	// for control messages, which are never discarded for age or anything else.
	queued time.Time
}

// NewServer binds a loopback listener on an ephemeral port and returns a
// Server ready to Serve. The caller passes the bound endpoint to the
// frontend (see Server.Endpoint).
func NewServer(log *slog.Logger, h Handler) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bridge: listen: %w", err)
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("bridge: token: %w", err)
	}
	return &Server{
		log:      log,
		handler:  h,
		listener: ln,
		token:    hex.EncodeToString(raw[:]),
	}, nil
}

// Endpoint returns the WebSocket URL and the token a client must present.
// The token guards against other local processes talking to the bridge:
// a loopback listener is reachable by anything running as this user.
func (s *Server) Endpoint() Endpoint {
	return Endpoint{
		URL:   "ws://" + s.listener.Addr().String() + "/ws",
		Token: s.token,
	}
}

// Serve runs the HTTP server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)

	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	err := srv.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != s.token {
		s.log.Warn("bridge: rejected connection with bad token", "remote", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// InsecureSkipVerify disables coder/websocket's Origin check. The
	// WebView's origin is wails://localhost, which no Origin allowlist
	// can express; the token above is what actually authenticates.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.log.Warn("bridge: accept failed", "err", err)
		return
	}
	ws.SetReadLimit(maxFrameBytes)

	ctx, cancel := context.WithCancel(r.Context())
	c := &conn{
		ws:      ws,
		video:   make(chan outbound, videoQueueDepth),
		audio:   make(chan outbound, audioQueueDepth),
		control: make(chan outbound, controlQueueDepth),
		ctx:     ctx,
		cancel:  cancel,
	}

	s.mu.Lock()
	if old := s.conn; old != nil {
		old.cancel()
	}
	s.conn = c
	s.mu.Unlock()

	s.log.Info("bridge: frontend connected")
	go c.writeLoop(s.log)
	s.handler.HandleConnect()
	s.readLoop(ctx, c)

	cancel()
	s.mu.Lock()
	if s.conn == c {
		s.conn = nil
	}
	s.mu.Unlock()
	_ = ws.CloseNow()
	s.handler.HandleDisconnect()
	s.log.Info("bridge: frontend disconnected",
		"droppedVideo", c.droppedVideo.Load(),
		"droppedAudio", c.droppedAudio.Load())
}

func (s *Server) readLoop(ctx context.Context, c *conn) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			if ctx.Err() == nil && !isNormalClose(err) {
				s.log.Warn("bridge: read failed", "err", err)
			}
			return
		}
		switch typ {
		case websocket.MessageText:
			var msg ClientMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				s.log.Warn("bridge: bad control message", "err", err)
				continue
			}
			if err := s.handler.HandleControl(ctx, &msg); err != nil {
				s.log.Error("bridge: control failed", "type", msg.Type, "err", err)
				s.SendError(err.Error())
			}
		case websocket.MessageBinary:
			f, err := ParseFrame(data)
			if err != nil {
				s.log.Warn("bridge: bad media frame", "err", err)
				continue
			}
			if err := s.handler.HandleMedia(ctx, &f); err != nil {
				s.log.Debug("bridge: media dropped", "err", err)
			}
		}
	}
}

func (c *conn) writeLoop(log *slog.Logger) {
	write := func(msg outbound) bool {
		ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
		err := c.ws.Write(ctx, msg.typ, msg.data)
		cancel()
		if err != nil {
			if c.ctx.Err() == nil && !isNormalClose(err) {
				log.Warn("bridge: write failed", "err", err)
			}
			c.cancel()
			return false
		}
		return true
	}

	for {
		if msg, ok := c.nextReady(time.Now()); ok {
			if !write(msg) {
				return
			}
			continue
		}
		// Nothing queued: block until something is, on any of the three. The
		// order does not matter here, because only one can be ready.
		select {
		case <-c.ctx.Done():
			return
		case msg := <-c.control:
			if !write(msg) {
				return
			}
		case msg := <-c.audio:
			if !write(msg) {
				return
			}
		case msg := <-c.video:
			if !write(msg) {
				return
			}
		}
	}
}

// nextReady takes the highest-priority message already queued, reporting false
// when all three queues are empty.
//
// Control first: a state change is what tells the frontend how to interpret the
// frames around it, so it should not wait behind a queue of them.
//
// Then audio ahead of video. Separate queues stop a video backlog evicting
// sound; this is what stops it delaying sound, which a queue of stale frames
// ahead of it in one write loop would still do. It cannot starve video however
// busy the call gets: audio is 32 kbps against video's 1.5 Mbps, so there is
// never more than a packet every 20 ms of it to prefer.
func (c *conn) nextReady(now time.Time) (outbound, bool) {
	select {
	case msg := <-c.control:
		return msg, true
	default:
	}
	if msg, ok := takeFresh(c.audio, &c.droppedAudio, audioMaxAge, now); ok {
		return msg, true
	}
	if msg, ok := takeFresh(c.video, &c.droppedVideo, videoMaxAge, now); ok {
		return msg, true
	}
	return outbound{}, false
}

// takeFresh returns the oldest frame still worth sending, discarding and
// counting whatever sat too long. Discarding here rather than at enqueue is
// deliberate: how long a frame waited is only known once something is ready to
// send it, and a queue that was filling while the frontend was not reading is
// exactly the case this exists for.
func takeFresh(
	q chan outbound,
	dropped *atomic.Uint64,
	maxAge time.Duration,
	now time.Time,
) (outbound, bool) {
	for {
		select {
		case msg := <-q:
			if now.Sub(msg.queued) <= maxAge {
				return msg, true
			}
			dropped.Add(1)
		default:
			return outbound{}, false
		}
	}
}

// SendControl queues a JSON control message. It never blocks.
func (s *Server) SendControl(msg *ServerMessage) {
	b, err := json.Marshal(msg)
	if err != nil {
		s.log.Error("bridge: marshal control", "type", msg.Type, "err", err)
		return
	}
	s.enqueueControl(outbound{typ: websocket.MessageText, data: b})
}

// SendError is shorthand for a MsgError control message.
func (s *Server) SendError(detail string) {
	s.SendControl(&ServerMessage{Type: MsgError, Error: detail})
}

// SendMedia queues an encoded frame for the frontend. It never blocks: if
// the frontend is not draining fast enough a frame is dropped, because a late
// frame in a live conference is worth less than a stalled reader.
//
// The frame dropped is the oldest queued one, not this one. The distinction is
// the whole point and it used to go the other way, which is what a full queue
// exposed: a WebView that stops reading — and one does, for as long as macOS
// is resizing its window — left the queue holding several seconds of stale
// video while every frame produced from then on, keyframes included, was
// discarded on arrival. When the WebView came back it drained history nobody
// could use and had nothing current to show, so the picture stayed frozen
// until the publisher's next scheduled keyframe, up to a keyframe interval
// after the resize had finished.
//
// Keeping the newest instead means the queue always holds the live edge, and
// at the video depth that edge spans more than one keyframe interval — so there
// is always something decodable in it.
//
// Which queue is decided here, by kind, and it is the whole reason there are
// two: a frame is only ever interchangeable with another of its own medium.
func (s *Server) SendMedia(f *MediaFrame) {
	buf := make([]byte, 0, FrameHeaderLen+len(f.Config)+len(f.Payload))
	msg := outbound{
		typ:    websocket.MessageBinary,
		data:   AppendFrame(buf, f),
		queued: time.Now(),
	}

	c := s.current()
	if c == nil {
		return
	}
	if f.Kind == KindAudio {
		enqueueMedia(c, c.audio, &c.droppedAudio, msg)
		return
	}
	enqueueMedia(c, c.video, &c.droppedVideo, msg)
}

// enqueueControl queues a control message, which is never dropped for
// backpressure: unlike frames, no two of them are interchangeable.
//
// Never blocks either, which is the harder half. Every goroutine in the
// process reaches this: the log sink emits synchronously on whichever
// goroutine called slog, and slog is the default logger, so moq-go's and
// quic-go's own diagnostics arrive here too. Waiting for room would therefore
// stall the metrics sampler, the bridge read loop, whichever conf goroutine
// was mid-subscribe holding its lock, and the transport underneath — on
// nothing more than a frontend that stopped reading its socket.
//
// So a full queue is treated as what it is. A thousand messages have gone
// unread; the frontend is gone in all but name, and the connection is failed
// rather than propped up. The WebView reconnects on its own and gets a fresh
// state message on attach, which is exactly the recovery path this has.
//
// Deliberately silent: reporting it would call slog, which would come straight
// back here.
func (s *Server) enqueueControl(msg outbound) {
	c := s.current()
	if c == nil {
		return
	}
	select {
	case c.control <- msg:
	case <-c.ctx.Done():
	default:
		c.cancel()
	}
}

// enqueueMedia queues a frame on one medium's queue, making room by discarding
// that medium's oldest if it must. Never blocks: the caller is a MOQ read loop,
// and stalling it would hold up every other track on the session.
func enqueueMedia(c *conn, q chan outbound, dropped *atomic.Uint64, msg outbound) {
	for {
		select {
		case q <- msg:
			return
		case <-c.ctx.Done():
			return
		default:
		}
		// Full. Take the oldest off the front and count it, then try again —
		// another producer may have refilled the slot in between, which is why
		// this loops rather than assuming one drop is enough.
		select {
		case <-q:
			dropped.Add(1)
		default:
		}
	}
}

func (s *Server) current() *conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

// Connected reports whether a frontend is currently attached.
func (s *Server) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// DroppedFrames returns how many outbound video and audio frames the current
// connection has dropped for backpressure. Sampled into every metrics message,
// so a rising count is visible in the debug panel while it is rising: it means
// the WebView cannot keep up with its decoders, and the loss is ours rather
// than the network's.
//
// Reported per medium because they have separate queues, and the two say
// different things. Dropped video is a frontend that cannot decode and paint as
// fast as frames arrive, which the picture shows anyway. Dropped audio is a
// frontend that has stopped reading its socket altogether — a queue that small,
// carrying that little, does not fill for any other reason — and that is worth
// telling apart from the network.
func (s *Server) DroppedFrames() (video, audio uint64) {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return 0, 0
	}
	return c.droppedVideo.Load(), c.droppedAudio.Load()
}

// Close releases the listener.
func (s *Server) Close() error { return s.listener.Close() }

func isNormalClose(err error) bool {
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway, websocket.StatusNoStatusRcvd:
		return true
	}
	return errors.Is(err, net.ErrClosed)
}
