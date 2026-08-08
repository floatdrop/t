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

	"github.com/coder/websocket"
)

// maxFrameBytes bounds a single inbound bridge frame. A 4 MB ceiling is
// far above any single encoded video frame while still bounding what a
// runaway frontend can make the backend buffer.
const maxFrameBytes = 4 << 20

// sendQueueDepth bounds the per-connection outbound queue. Media is
// live: when the queue is full the oldest frames are worthless, so
// Send drops rather than blocking the MOQ read path (see Server.Send).
const sendQueueDepth = 256

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
	ws     *websocket.Conn
	out    chan outbound
	ctx    context.Context
	cancel context.CancelFunc

	dropped atomic.Uint64
}

// outbound is one queued frame plus the WebSocket opcode to send it
// under, so the write loop never has to guess from the payload bytes.
type outbound struct {
	typ  websocket.MessageType
	data []byte
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
	c := &conn{ws: ws, out: make(chan outbound, sendQueueDepth), ctx: ctx, cancel: cancel}

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
	s.log.Info("bridge: frontend disconnected", "droppedFrames", c.dropped.Load())
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
	for {
		select {
		case <-c.ctx.Done():
			return
		case msg := <-c.out:
			if err := c.ws.Write(c.ctx, msg.typ, msg.data); err != nil {
				if c.ctx.Err() == nil && !isNormalClose(err) {
					log.Warn("bridge: write failed", "err", err)
				}
				c.cancel()
				return
			}
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
	s.enqueue(outbound{websocket.MessageText, b})
}

// SendError is shorthand for a MsgError control message.
func (s *Server) SendError(detail string) {
	s.SendControl(&ServerMessage{Type: MsgError, Error: detail})
}

// SendMedia queues an encoded frame for the frontend. It never blocks: if
// the frontend is not draining fast enough the frame is dropped, because
// a late frame in a live conference is worth less than a stalled reader.
func (s *Server) SendMedia(f *MediaFrame) {
	buf := make([]byte, 0, FrameHeaderLen+len(f.Config)+len(f.Payload))
	s.enqueue(outbound{websocket.MessageBinary, AppendFrame(buf, f)})
}

func (s *Server) enqueue(msg outbound) {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return
	}
	select {
	case c.out <- msg:
	default:
		c.dropped.Add(1)
	}
}

// Connected reports whether a frontend is currently attached.
func (s *Server) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// DroppedFrames returns how many outbound frames the current connection
// has dropped for backpressure. Sampled into every metrics message, so a
// rising count is visible in the debug panel while it is rising: it means
// the WebView cannot keep up with its decoders, and the loss is ours rather
// than the network's.
func (s *Server) DroppedFrames() uint64 {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return 0
	}
	return c.dropped.Load()
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
