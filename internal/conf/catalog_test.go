package conf

import (
	"testing"

	"t/internal/bridge"
)

// The version has to survive the round trip it actually makes — built into a
// catalog, marshalled, parsed back — because that is the only path it takes to
// another participant, and a field that silently fails to appear looks exactly
// like a peer on an old build.
func TestCatalogCarriesTheVersion(t *testing.T) {
	video := &bridge.TrackConfig{Kind: "video", Codec: "avc1.42E01F", Width: 1280, Height: 720}

	cat, err := buildCatalog("alice", "0.3.0", video, nil)
	if err != nil {
		t.Fatalf("buildCatalog: %v", err)
	}
	payload, err := encodeCatalog(cat)
	if err != nil {
		t.Fatalf("encodeCatalog: %v", err)
	}
	got, err := parseCatalog(payload)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if got.Version != "0.3.0" {
		t.Errorf("Version = %q, want 0.3.0", got.Version)
	}
	if got.Nickname != "alice" {
		t.Errorf("Nickname = %q, want alice — the version must not displace it", got.Nickname)
	}
}

// A peer old enough not to publish a version must still parse, and must not be
// given one it never claimed.
func TestCatalogWithoutVersion(t *testing.T) {
	audio := &bridge.TrackConfig{Kind: "audio", Codec: "opus", SampleRate: 48000, Channels: 1}

	cat, err := buildCatalog("bob", "", nil, audio)
	if err != nil {
		t.Fatalf("buildCatalog: %v", err)
	}
	if _, ok := cat.Extras[catalogVersionKey]; ok {
		t.Errorf("Extras[%s] present for a build with no version", catalogVersionKey)
	}

	payload, err := encodeCatalog(cat)
	if err != nil {
		t.Fatalf("encodeCatalog: %v", err)
	}
	got, err := parseCatalog(payload)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if got.Version != "" {
		t.Errorf("Version = %q, want empty", got.Version)
	}
	if got.Nickname != "bob" {
		t.Errorf("Nickname = %q, want bob", got.Nickname)
	}
}

// The operating system takes the same round trip as the version, and for the
// same reason: it is a field the roster shows about everyone, so one that
// silently fails to appear is indistinguishable from a peer too old to publish
// it — and the icon for that is the neutral one, which is a quiet lie rather
// than a visible fault.
func TestCatalogCarriesTheOperatingSystem(t *testing.T) {
	cat, err := buildCatalog("alice", "0.5.3", nil, nil)
	if err != nil {
		t.Fatalf("buildCatalog: %v", err)
	}
	payload, err := encodeCatalog(cat)
	if err != nil {
		t.Fatalf("encodeCatalog: %v", err)
	}
	got, err := parseCatalog(payload)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if got.OS != PlatformName() {
		t.Errorf("OS = %q, want %q", got.OS, PlatformName())
	}
	if got.Version != "0.5.3" || got.Nickname != "alice" {
		t.Errorf("the OS displaced its neighbours: version %q, nickname %q",
			got.Version, got.Nickname)
	}
}

// A peer that publishes no OS parses as empty rather than as a guess. The
// roster shows the neutral icon and the word "unknown" for that, which is the
// honest answer for a build from before the field existed.
func TestCatalogWithoutAnOperatingSystem(t *testing.T) {
	cat, err := buildCatalog("alice", "0.5.3", nil, nil)
	if err != nil {
		t.Fatalf("buildCatalog: %v", err)
	}
	delete(cat.Extras, catalogOSKey)
	payload, err := encodeCatalog(cat)
	if err != nil {
		t.Fatalf("encodeCatalog: %v", err)
	}
	got, err := parseCatalog(payload)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if got.OS != "" {
		t.Errorf("OS = %q from a catalog that carried none, want empty", got.OS)
	}
}

// PlatformName is what both halves of the roster read — the catalog for remote
// participants and the bridge descriptor for our own row — so it has to name
// something, whatever it is built for.
func TestPlatformNameIsNeverEmpty(t *testing.T) {
	if PlatformName() == "" {
		t.Error("PlatformName is empty; our own row would show the unknown icon")
	}
}
