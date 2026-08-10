package conf

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlogwriter"
	"github.com/quic-go/webtransport-go"

	"github.com/floatdrop/moq-go/pkg/moqt/session"
	"github.com/floatdrop/moq-go/pkg/moqt/session/quicconn"
	"github.com/floatdrop/moq-go/pkg/moqt/session/wtconn"
	"github.com/floatdrop/moq-go/pkg/moqt/uri"

	"t/internal/telemetry"
)

// implementation is the MOQT_IMPLEMENTATION SETUP option value.
const implementation = "t/0.1"

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
		// A relay that vanishes without closing — a crash, a partition —
		// is only detectable by timeout, and that timeout is how long the
		// call sits dead before a reconnect can even begin. Ten seconds
		// with a 2 s keepalive means five missed probes before giving up:
		// fast enough not to strand a conversation, patient enough not to
		// tear down a session over a brief stall. Media traffic refreshes
		// the timer anyway, so this only bites when the path is genuinely
		// gone.
		MaxIdleTimeout:  10 * time.Second,
		KeepAlivePeriod: 2 * time.Second,
		// How long a dial may spend finding out that nobody is there.
		//
		// Separate from MaxIdleTimeout above, which governs a session that was
		// established and went quiet; this one governs never establishing at
		// all. Left unset it is quic-go's 5 s, and a reconnect loop pays that
		// in full for every attempt against a relay that is down — which is
		// most of the time a restarting relay is unreachable. Three seconds is
		// many round trips on any path this app is usable on, and nearly twice
		// as fast to give up and try again.
		HandshakeIdleTimeout:             3 * time.Second,
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
	target, err := withDefaultPort(target)
	if err != nil {
		return nil, err
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

// withDefaultPort gives an https URL the explicit port that dialling needs.
//
// webtransport-go splits the authority into host and port and refuses one that
// has no port, so "https://relay.example/" — which is how anyone would write a
// relay reachable on the standard port, and how the welcome screen's own
// default is written — failed before it was ever dialled. A moqt:// URI never
// reaches here without a port because uri.Parse applies the same §3.1.1
// default, which is the constant reused below rather than a second 443.
func withDefaultPort(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse relay address %q: %w", raw, err)
	}
	if u.Port() != "" {
		return raw, nil
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("relay address %q names no host", raw)
	}
	u.Host = net.JoinHostPort(u.Hostname(), uri.DefaultPort)
	return u.String(), nil
}

func isWebTransport(addr string) bool {
	return strings.HasPrefix(addr, "https://")
}
