package conf

import (
	"testing"

	"tlmst/internal/bridge"
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
