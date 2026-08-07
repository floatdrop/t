// Package update asks GitHub whether a newer release exists.
//
// The check runs here rather than in the WebView for two reasons. The frontend
// is served from a custom scheme, so a fetch to api.github.com is a
// cross-origin request that the WebView is under no obligation to allow — and
// the answer is only interesting once, at startup, which is not worth teaching
// the render thread about. The result travels to the frontend as one control
// message, and the app opens the page through the OS rather than navigating
// the WebView, which has a call in it.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"tlmst/internal/version"
)

// Repo is where releases are published. Not configurable: an update prompt
// that could be pointed somewhere else is a way to get someone to install
// something they did not ask for.
const Repo = "floatdrop/tlmst"

// ReleasesURL is the page the update button opens. The user sees what they are
// downloading and from whom, which is the whole reason this offers a page
// rather than doing the update itself.
const ReleasesURL = "https://github.com/" + Repo + "/releases"

const latestAPI = "https://api.github.com/repos/" + Repo + "/releases/latest"

// Timeout bounds the whole check. Nothing waits on this — it is a button that
// may or may not appear — so a slow answer is the same as no answer.
const Timeout = 10 * time.Second

// Release is the newest published release, as far as GitHub is concerned.
type Release struct {
	// Version is the tag with any leading v stripped, so it compares against
	// the running version directly.
	Version string
	// URL is the release's own page, falling back to the releases list.
	URL string
}

type apiRelease struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Latest fetches the newest release. The zero Release and a nil error mean
// GitHub answered but has nothing worth offering — a draft, a prerelease, or
// no release at all.
func Latest(ctx context.Context, client *http.Client) (Release, error) {
	return latestFrom(ctx, client, latestAPI)
}

// latestFrom is Latest against a named endpoint. The address is a constant in
// everything but the tests: an update prompt that could be pointed elsewhere is
// a way to get someone to install something they did not ask for, so it is not
// a parameter callers get to choose.
func latestFrom(ctx context.Context, client *http.Client, api string) (Release, error) {
	if client == nil {
		client = &http.Client{Timeout: Timeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return Release{}, fmt.Errorf("update: build request: %w", err)
	}
	// Unauthenticated, so this is subject to GitHub's 60-per-hour-per-address
	// limit. One call per app start is nowhere near it.
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	res, err := client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("update: fetch latest release: %w", err)
	}
	defer res.Body.Close()

	// 404 is the honest answer for a repository with no releases yet, and is
	// not worth reporting as a failure.
	if res.StatusCode == http.StatusNotFound {
		return Release{}, nil
	}
	if res.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("update: latest release: HTTP %d", res.StatusCode)
	}

	var out apiRelease
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return Release{}, fmt.Errorf("update: decode latest release: %w", err)
	}
	if out.Draft || out.Prerelease || out.TagName == "" {
		return Release{}, nil
	}

	url := out.HTMLURL
	if url == "" {
		url = ReleasesURL
	}
	return Release{Version: trimV(out.TagName), URL: url}, nil
}

func trimV(tag string) string {
	if len(tag) > 1 && (tag[0] == 'v' || tag[0] == 'V') {
		return tag[1:]
	}
	return tag
}

// Check returns the release to offer, or the zero Release if there is nothing
// to say. A build with no release version of its own never offers anything:
// comparing a development build against a tag can only produce a prompt that
// is wrong in one direction or the other.
func Check(ctx context.Context, client *http.Client, current string) (Release, error) {
	return checkFrom(ctx, client, current, latestAPI)
}

func checkFrom(ctx context.Context, client *http.Client, current, api string) (Release, error) {
	if !version.IsRelease(current) {
		return Release{}, nil
	}
	latest, err := latestFrom(ctx, client, api)
	if err != nil {
		return Release{}, err
	}
	if !version.Newer(current, latest.Version) {
		return Release{}, nil
	}
	return latest, nil
}
