package main

import "testing"

// TestParseInviteURL pins the link format against the builder in
// frontend/src/lib/invite.ts. The two live in different languages and are
// exercised by different halves of the app, so nothing but a test keeps them
// speaking the same dialect. Every input here is a literal string that
// buildInviteLink produces.
func TestParseInviteURL(t *testing.T) {
	tests := []struct {
		name  string
		url   string
		relay string
		room  string
		ok    bool
	}{{
		name:  "bare host:port is the authority",
		url:   "tlmst://localhost:4433/linktest",
		relay: "localhost:4433",
		room:  "linktest",
		ok:    true,
	}, {
		name:  "remote relay",
		url:   "tlmst://relay.example.com:443/standup",
		relay: "relay.example.com:443",
		room:  "standup",
		ok:    true,
	}, {
		// The query parameter carries relays an authority cannot express,
		// and takes precedence over the authority shown for readability.
		name:  "https relay overrides the authority",
		url:   "tlmst://relay.example.com/r1?relay=https%3A%2F%2Frelay.example.com%2Flive",
		relay: "https://relay.example.com/live",
		room:  "r1",
		ok:    true,
	}, {
		name:  "moqt relay overrides the authority",
		url:   "tlmst://relay.example.com:4433/r2?relay=moqt%3A%2F%2Frelay.example.com%3A4433%2Fapp",
		relay: "moqt://relay.example.com:4433/app",
		room:  "r2",
		ok:    true,
	}, {
		name:  "room is percent-decoded",
		url:   "tlmst://localhost:4433/room%20one",
		relay: "localhost:4433",
		room:  "room one",
		ok:    true,
	}, {
		name: "no room",
		url:  "tlmst://localhost:4433",
	}, {
		name: "no room, trailing slash only",
		url:  "tlmst://localhost:4433/",
	}, {
		name: "no relay",
		url:  "tlmst:///room",
	}, {
		name: "wrong scheme",
		url:  "https://localhost:4433/room",
	}, {
		name: "not a URL at all",
		url:  "localhost:4433",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			relay, room, ok := parseInviteURL(tt.url)
			if ok != tt.ok {
				t.Fatalf("parseInviteURL(%q) ok = %v, want %v", tt.url, ok, tt.ok)
			}
			if !tt.ok {
				return
			}
			if relay != tt.relay {
				t.Errorf("relay = %q, want %q", relay, tt.relay)
			}
			if room != tt.room {
				t.Errorf("room = %q, want %q", room, tt.room)
			}
		})
	}
}
