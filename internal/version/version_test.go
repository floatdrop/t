package version

import (
	"encoding/json"
	"os"
	"regexp"
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
  productName: "t"
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

// TestBundleMetadataMatchesConfig pins the version files nothing else reads.
//
// build/config.yml is the source of truth, and Parse above is how the running
// binary learns its own version — but it is not the only place the number is
// written. macOS reads build/darwin/Info.plist, Windows reads the resource
// generated from build/windows/info.json, and both are maintained by hand.
// Nothing consults them at build time, so a release that forgets one produces
// an artifact that reports the right version in the app and the wrong one to
// the operating system. That is not hypothetical: 0.10.0 bumped config.yml
// alone and shipped a bundle still telling Finder it was 0.9.1, which nobody
// noticed until the next release read the file.
//
// This is the check that would have caught it, and it is deliberately a test
// rather than a generator: the two files carry hand edits — the bundle
// identifier, the URL scheme, the camera and microphone usage descriptions —
// that regenerating them from config.yml would destroy.
func TestBundleMetadataMatchesConfig(t *testing.T) {
	raw, err := os.ReadFile("../../build/config.yml")
	if err != nil {
		t.Fatalf("read build/config.yml: %v", err)
	}
	want := Parse(raw)
	if want == Dev {
		t.Fatalf("Parse(build/config.yml) = %q, want a version", want)
	}

	// Both plists: the shipping bundle and the one `wails3 task run` uses, so a
	// dev build reports the same version a release does.
	for _, name := range []string{"Info.plist", "Info.dev.plist"} {
		path := "../../build/darwin/" + name
		plist, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, key := range []string{"CFBundleVersion", "CFBundleShortVersionString"} {
			got, ok := plistString(string(plist), key)
			if !ok {
				t.Errorf("%s has no %s", name, key)
				continue
			}
			if got != want {
				t.Errorf("%s %s = %q, want %q (build/config.yml)", name, key, got, want)
			}
		}
	}

	// Windows splits the number in two. The string table carries it as written,
	// while VS_FIXEDFILEINFO is four numeric fields, so the file has to spell
	// out the fourth — winres parses "0.11.0" as far as it can and leaves the
	// rest zero, which happens to be right and would silently not be if the
	// convention ever changed.
	winPath := "../../build/windows/info.json"
	winRaw, err := os.ReadFile(winPath)
	if err != nil {
		t.Fatalf("read %s: %v", winPath, err)
	}
	var win struct {
		Fixed struct {
			FileVersion    string `json:"file_version"`
			ProductVersion string `json:"product_version"`
		} `json:"fixed"`
		Info map[string]map[string]string `json:"info"`
	}
	if err := json.Unmarshal(winRaw, &win); err != nil {
		t.Fatalf("parse %s: %v", winPath, err)
	}
	wantFixed := want + ".0"
	for key, got := range map[string]string{
		"fixed.file_version":    win.Fixed.FileVersion,
		"fixed.product_version": win.Fixed.ProductVersion,
	} {
		if got != wantFixed {
			t.Errorf("windows/info.json %s = %q, want %q", key, got, wantFixed)
		}
	}
	for lang, table := range win.Info {
		for _, key := range []string{"FileVersion", "ProductVersion"} {
			got, ok := table[key]
			if !ok {
				t.Errorf("windows/info.json info[%s] has no %s", lang, key)
				continue
			}
			if got != want {
				t.Errorf("windows/info.json info[%s].%s = %q, want %q", lang, key, got, want)
			}
		}
	}
}

// plistString reads the <string> value following a <key> in a plist. Enough for
// the flat top-level keys this checks, and less than a plist parser would be.
var plistKeyValue = regexp.MustCompile(`(?s)<key>([^<]+)</key>\s*<string>([^<]*)</string>`)

func plistString(doc, key string) (string, bool) {
	for _, m := range plistKeyValue.FindAllStringSubmatch(doc, -1) {
		if m[1] == key {
			return m[2], true
		}
	}
	return "", false
}
