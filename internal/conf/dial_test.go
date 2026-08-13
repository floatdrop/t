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
