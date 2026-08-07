package version

import (
	"os"
	"testing"
)

// TestParseRealConfig is the one that matters: it reads the file the packaging
// reads, so a release that renames or moves the field is caught here rather
// than by a user reading "dev" in the welcome screen of a tagged build.
func TestParseRealConfig(t *testing.T) {
	raw, err := os.ReadFile("../../build/config.yml")
	if err != nil {
		t.Fatalf("read build/config.yml: %v", err)
	}
	got := Parse(raw)
	if got == Dev {
		t.Fatalf("Parse(build/config.yml) = %q, want a version", got)
	}
	if _, ok := fields(got); !ok {
		t.Fatalf("Parse(build/config.yml) = %q, want three numeric fields", got)
	}
}

func TestParse(t *testing.T) {
	// The decoys are the point: `version: '3'` at column zero is the Taskfile
	// schema, and the ios block ships commented out with a version of its own.
	const doc = `version: '3'

info:
  productName: "tlmst"
  version: "1.4.2" # The application version

# ios:
#   version: "0.0.1"

other:
  - name: My Other Data
`
	if got := Parse([]byte(doc)); got != "1.4.2" {
		t.Fatalf("Parse = %q, want 1.4.2", got)
	}
}

// An uncommented ios block sits after info and must not win.
func TestParseIgnoresLaterBlocks(t *testing.T) {
	const doc = `version: '3'
info:
  version: "2.0.0"
ios:
  version: "9.9.9"
`
	if got := Parse([]byte(doc)); got != "2.0.0" {
		t.Fatalf("Parse = %q, want 2.0.0", got)
	}
}

func TestParseMissing(t *testing.T) {
	if got := Parse([]byte("version: '3'\nother: []\n")); got != Dev {
		t.Fatalf("Parse = %q, want %q", got, Dev)
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.3.0", "0.3.1", true},
		{"0.3.0", "0.4.0", true},
		{"0.3.0", "1.0.0", true},
		{"0.3.0", "v0.3.1", true}, // tags carry the v
		{"0.3.0", "0.3.0", false},
		{"0.3.1", "0.3.0", false},
		{"1.0.0", "0.9.9", false},
		{"0.10.0", "0.9.0", false}, // not string order
		{"0.9.0", "0.10.0", true},
		// A build that does not name a release compares against nothing.
		{Dev, "9.9.9", false},
		{"0.3.0", Dev, false},
		{"", "1.0.0", false},
		// Anything that is not three plain numbers is not offered.
		{"0.3.0", "0.4.0-rc1", false},
		{"0.3.0", "0.4", false},
		{"0.3.0", "not-a-version", false},
	}
	for _, c := range cases {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}
