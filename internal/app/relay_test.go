package app

import "testing"

// TestRelayForAttempt pins where a reconnect dials, which GOAWAY makes
// non-obvious: the message may name a replacement relay, and §10.4 allows it
// to be absent. Absent means "come back to me" — so an empty preferred
// address must resolve to the relay already configured, not to nothing.
func TestRelayForAttempt(t *testing.T) {
	const configured = "relay.example.com:4433"

	tests := []struct {
		name      string
		preferred string
		attempt   int
		want      string
	}{{
		name:      "no URI in the GOAWAY dials the same relay",
		preferred: "",
		attempt:   1,
		want:      configured,
	}, {
		name:      "no URI still dials the same relay on later attempts",
		preferred: "",
		attempt:   7,
		want:      configured,
	}, {
		name:      "a named relay is tried first",
		preferred: "moved.example.com:4433",
		attempt:   1,
		want:      "moved.example.com:4433",
	}, {
		// A URI that does not come up must not strand the client, so every
		// attempt after the first goes back to what the user chose.
		name:      "a named relay that fails falls back to the configured one",
		preferred: "moved.example.com:4433",
		attempt:   2,
		want:      configured,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := relayForAttempt(configured, tt.preferred, tt.attempt); got != tt.want {
				t.Errorf("relayForAttempt(%q, %q, %d) = %q, want %q",
					configured, tt.preferred, tt.attempt, got, tt.want)
			}
		})
	}
}
