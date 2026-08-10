package app

import (
	"io"
	"log/slog"
	"testing"

	"t/internal/bridge"
	"t/internal/telemetry"
	"t/internal/update"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(log, telemetry.NewLogSink(slog.LevelInfo), "0.3.0")
}

// The bridge listens on loopback, which any local process can reach. "Open
// this URL" is therefore a capability handed to more than just our own
// frontend, so it is restricted to the links this app itself produced.
func TestOpenLinkRefusesUnknownURLs(t *testing.T) {
	a := newTestApp(t)
	var opened []string
	a.SetOpenURL(func(url string) error {
		opened = append(opened, url)
		return nil
	})

	for _, url := range []string{
		"https://example.test/malware",
		"file:///etc/passwd",
		"https://github.com/someone-else/tlmst/releases",
		// Close enough to look right, and still not ours.
		update.ReleasesURL + ".evil.test",
		"",
	} {
		if err := a.openLink(url); err == nil {
			t.Errorf("openLink(%q) = nil, want an error", url)
		}
	}
	if len(opened) != 0 {
		t.Errorf("opened %v, want nothing", opened)
	}
}

func TestOpenLinkAllowsTheReleasesPage(t *testing.T) {
	a := newTestApp(t)
	var opened []string
	a.SetOpenURL(func(url string) error {
		opened = append(opened, url)
		return nil
	})

	if err := a.openLink(update.ReleasesURL); err != nil {
		t.Fatalf("openLink(releases) = %v", err)
	}
	if len(opened) != 1 || opened[0] != update.ReleasesURL {
		t.Fatalf("opened %v, want the releases page", opened)
	}
}

// The specific release the check turned up is a link this app produced, so it
// opens too — otherwise the button would have to fall back to the release list
// and lose the version it was offering.
func TestOpenLinkAllowsTheOfferedRelease(t *testing.T) {
	a := newTestApp(t)
	var opened []string
	a.SetOpenURL(func(url string) error {
		opened = append(opened, url)
		return nil
	})

	const offered = "https://github.com/floatdrop/tlmst/releases/tag/v0.4.0"
	if err := a.openLink(offered); err == nil {
		t.Error("openLink accepted a release that was never offered")
	}

	a.offer = &bridge.Update{Version: "0.4.0", URL: offered}
	if err := a.openLink(offered); err != nil {
		t.Fatalf("openLink(offered) = %v", err)
	}
	if len(opened) != 1 || opened[0] != offered {
		t.Fatalf("opened %v, want the offered release", opened)
	}
}

// A platform with no way to open a link says so rather than reporting success.
func TestOpenLinkWithoutAnOpener(t *testing.T) {
	a := newTestApp(t)
	if err := a.openLink(update.ReleasesURL); err == nil {
		t.Fatal("openLink = nil with no opener set, want an error")
	}
}
