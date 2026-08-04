package conf

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlogwriter"
	"github.com/quic-go/webtransport-go"

	"github.com/floatdrop/moq-go/pkg/moqt/session"
	"github.com/floatdrop/moq-go/pkg/moqt/session/quicconn"
	"github.com/floatdrop/moq-go/pkg/moqt/session/wtconn"
	"github.com/floatdrop/moq-go/pkg/moqt/uri"

	"tlmst/internal/telemetry"
)

// implementation is the MOQT_IMPLEMENTATION SETUP option value.
const implementation = "tlmst/0.1"

// alpnDraft19 is the draft-19 ALPN. draft-19 SETUP carries no version
// field, so the ALPN is the version signal (§3.1).
const alpnDraft19 = "moqt-19"

// dialResult carries the session plus the trace bound to its connection.
type dialResult struct {
	sess  *session.Session
	trace *telemetry.QUICTrace
}

// dial opens a MOQT session to addr and returns it alongside the qlog
// trace collecting that connection's transport metrics.
//
// addr may be:
//   - a bare "host:port" or a "moqt://" URI — raw QUIC (§3.1.1)
//   - an "https://" URI — WebTransport
//
// insecure skips TLS verification, which development relays with
// self-signed certificates require.
func dial(ctx context.Context, log *slog.Logger, addr string, insecure bool) (*dialResult, error) {
	trace := telemetry.NewQUICTrace()

	quicCfg := &quic.Config{
		MaxIdleTimeout:                   30 * time.Second,
		KeepAlivePeriod:                  5 * time.Second,
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true, // §11.4.3 RESET_STREAM_AT
		Tracer: func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
			return trace
		},
	}

	if isWebTransport(addr) {
		sess, err := dialWebTransport(ctx, log, addr, insecure, quicCfg)
		if err != nil {
			return nil, err
		}
		return &dialResult{sess: sess, trace: trace}, nil
	}

	hostPort := addr
	opts := []session.Option{session.WithImplementation(implementation)}
	if strings.Contains(addr, "://") {
		u, err := uri.Parse(addr)
		if err != nil {
			return nil, fmt.Errorf("parse relay address: %w", err)
		}
		hostPort = u.HostPort()
		// §10.3.1.1: the client MUST set AUTHORITY to the URI's
		// authority portion.
		opts = append(opts, session.WithAuthority(u.Authority))
		if pq := u.PathAndQuery(); pq != "" {
			opts = append(opts, session.WithPath(pq))
		}
	}

	tlsCfg := &tls.Config{
		//nolint:gosec // G402: opt-in, for dev relays with self-signed certs.
		InsecureSkipVerify: insecure,
		NextProtos:         []string{alpnDraft19},
	}

	log.Info("dialing relay over QUIC", "addr", hostPort, "alpn", alpnDraft19)
	qconn, err := quic.DialAddr(ctx, hostPort, tlsCfg, quicCfg)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", hostPort, err)
	}
	sess, err := session.Client(ctx, quicconn.New(qconn), opts...)
	if err != nil {
		return nil, fmt.Errorf("moqt handshake: %w", err)
	}
	return &dialResult{sess: sess, trace: trace}, nil
}

// dialWebTransport establishes the session over an HTTP/3 WebTransport
// tunnel, for relays that only speak WebTransport.
func dialWebTransport(
	ctx context.Context,
	log *slog.Logger,
	addr string,
	insecure bool,
	quicCfg *quic.Config,
) (*session.Session, error) {
	target := addr
	if strings.HasPrefix(addr, uri.Scheme+"://") {
		u, err := uri.Parse(addr)
		if err != nil {
			return nil, fmt.Errorf("parse relay address: %w", err)
		}
		target = u.HTTPSURL()
	}

	// webtransport.Transport fills in the h3 ALPN itself. The MOQT draft
	// version is not signalled here: over WebTransport it is the tunnel,
	// not the ALPN, that carries the session, and relays differ on whether
	// they implement the WebTransport application-protocol extension. Raw
	// QUIC (the branch above) is the well-tested path.
	d := &webtransport.Transport{
		TLSClientConfig: &tls.Config{
			//nolint:gosec // G402: opt-in, for dev relays with self-signed certs.
			InsecureSkipVerify: insecure,
		},
		QUICConfig: quicCfg,
	}
	log.Info("dialing relay over WebTransport", "url", target)
	resp, wtSess, err := d.Dial(ctx, target, http.Header{})
	if err != nil {
		return nil, fmt.Errorf("webtransport dial %s: %w", target, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("webtransport dial %s: status %s", target, resp.Status)
	}
	sess, err := session.Client(ctx, wtconn.New(wtSess),
		session.WithImplementation(implementation))
	if err != nil {
		return nil, fmt.Errorf("moqt handshake: %w", err)
	}
	return sess, nil
}

func isWebTransport(addr string) bool {
	return strings.HasPrefix(addr, "https://")
}
