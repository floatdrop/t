package conf

import "testing"

// TestWithDefaultPort covers the shape of address the welcome screen now ships
// as its own default. webtransport-go rejects an authority with no port, so an
// https relay written the way anyone writes one — no port, standard port
// implied — has to gain the §3.1.1 default before it is dialled.
func TestWithDefaultPort(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{{
		// The reported failure: the default relay, exactly as it is written.
		name: "https with no port gains 443",
		raw:  "https://moq.tel.yandex.net/",
		want: "https://moq.tel.yandex.net:443/",
	}, {
		name: "no trailing slash",
		raw:  "https://relay.example",
		want: "https://relay.example:443",
	}, {
		name: "path and query are preserved",
		raw:  "https://relay.example/moq?x=1",
		want: "https://relay.example:443/moq?x=1",
	}, {
		// Returned untouched rather than reassembled, so nothing else about
		// the URL can shift on the way through.
		name: "explicit port is left alone",
		raw:  "https://relay.example:4433/moq",
		want: "https://relay.example:4433/moq",
	}, {
		name: "explicit 443 is not duplicated",
		raw:  "https://relay.example:443/",
		want: "https://relay.example:443/",
	}, {
		name: "IPv6 literal keeps its brackets",
		raw:  "https://[::1]/moq",
		want: "https://[::1]:443/moq",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := withDefaultPort(tc.raw)
			if err != nil {
				t.Fatalf("withDefaultPort(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("withDefaultPort(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// An address with no host at all cannot be dialled, and saying so beats
// handing webtransport-go a ":443" to fail on later.
func TestWithDefaultPortNoHost(t *testing.T) {
	for _, raw := range []string{"https://", "/moq", ""} {
		if got, err := withDefaultPort(raw); err == nil {
			t.Errorf("withDefaultPort(%q) = %q, want an error", raw, got)
		}
	}
}

// TestWithDefaultAuthorityPort covers the address form that never reaches a
// URL parser. quic.DialAddr rejects an authority with no port, so a relay
// written as a plain hostname — what an invite link that lost its `relay`
// parameter hands over — failed on "missing port in address" before it was
// dialled.
func TestWithDefaultAuthorityPort(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{{
		// The reported failure: the public relay reduced to its authority.
		name: "bare host gains 443",
		raw:  "moq.tel.yandex.net",
		want: "moq.tel.yandex.net:443",
	}, {
		// A development relay, which is what the bare form was built for.
		name: "explicit port is left alone",
		raw:  "localhost:4433",
		want: "localhost:4433",
	}, {
		name: "explicit 443 is not duplicated",
		raw:  "relay.example:443",
		want: "relay.example:443",
	}, {
		// Splits without error but cannot be dialled, so the empty port is
		// treated as the absence it is.
		name: "empty port is filled",
		raw:  "relay.example:",
		want: "relay.example:443",
	}, {
		name: "IPv6 literal keeps its brackets",
		raw:  "[::1]",
		want: "[::1]:443",
	}, {
		name: "IPv6 literal with a port is left alone",
		raw:  "[::1]:4433",
		want: "[::1]:4433",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := withDefaultAuthorityPort(tc.raw)
			if err != nil {
				t.Fatalf("withDefaultAuthorityPort(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("withDefaultAuthorityPort(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// An empty address cannot be dialled, and saying so beats handing quic-go a
// ":443" to fail on later.
func TestWithDefaultAuthorityPortEmpty(t *testing.T) {
	if got, err := withDefaultAuthorityPort(""); err == nil {
		t.Errorf("withDefaultAuthorityPort(\"\") = %q, want an error", got)
	}
}
