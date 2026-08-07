package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serve stands in for the GitHub API, so no test here reaches the network.
func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func json200(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func TestLatest(t *testing.T) {
	api := serve(t, func(w http.ResponseWriter, r *http.Request) {
		// The headers are part of the contract with the API, and getting them
		// wrong is the kind of thing that only shows up as a rate limit.
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("X-GitHub-Api-Version = %q", got)
		}
		json200(`{"tag_name":"v0.4.0","html_url":"https://example.test/r/0.4.0"}`)(w, r)
	})

	got, err := latestFrom(context.Background(), nil, api)
	if err != nil {
		t.Fatalf("latestFrom: %v", err)
	}
	if got.Version != "0.4.0" {
		t.Errorf("Version = %q, want 0.4.0 (the v belongs to the tag, not the version)", got.Version)
	}
	if got.URL != "https://example.test/r/0.4.0" {
		t.Errorf("URL = %q", got.URL)
	}
}

// A release with no page of its own still gets a link worth following.
func TestLatestFallsBackToReleasesPage(t *testing.T) {
	api := serve(t, json200(`{"tag_name":"0.4.0"}`))
	got, err := latestFrom(context.Background(), nil, api)
	if err != nil {
		t.Fatalf("latestFrom: %v", err)
	}
	if got.URL != ReleasesURL {
		t.Errorf("URL = %q, want %q", got.URL, ReleasesURL)
	}
}

// A repository with no releases answers 404, which is an answer rather than a
// fault: nothing to offer, and nothing to complain about in the log.
func TestLatestNoReleases(t *testing.T) {
	api := serve(t, status(http.StatusNotFound))
	got, err := latestFrom(context.Background(), nil, api)
	if err != nil {
		t.Fatalf("latestFrom: %v", err)
	}
	if got != (Release{}) {
		t.Errorf("latestFrom = %+v, want zero", got)
	}
}

func TestLatestSkipsUnfinishedReleases(t *testing.T) {
	for _, body := range []string{
		`{"tag_name":"v9.9.9","prerelease":true}`,
		`{"tag_name":"v9.9.9","draft":true}`,
		`{"tag_name":""}`,
	} {
		api := serve(t, json200(body))
		got, err := latestFrom(context.Background(), nil, api)
		if err != nil {
			t.Fatalf("latestFrom(%s): %v", body, err)
		}
		if got != (Release{}) {
			t.Errorf("latestFrom(%s) = %+v, want zero", body, got)
		}
	}
}

func TestLatestServerError(t *testing.T) {
	api := serve(t, status(http.StatusInternalServerError))
	if _, err := latestFrom(context.Background(), nil, api); err == nil {
		t.Fatal("latestFrom: want an error on HTTP 500")
	}
}

func TestCheck(t *testing.T) {
	api := serve(t, json200(`{"tag_name":"v0.4.0","html_url":"https://example.test/r"}`))

	got, err := checkFrom(context.Background(), nil, "0.3.0", api)
	if err != nil {
		t.Fatalf("checkFrom: %v", err)
	}
	if got.Version != "0.4.0" {
		t.Errorf("checkFrom = %+v, want 0.4.0 offered", got)
	}

	for _, current := range []string{"0.4.0", "0.5.0"} {
		got, err := checkFrom(context.Background(), nil, current, api)
		if err != nil {
			t.Fatalf("checkFrom(%s): %v", current, err)
		}
		if got != (Release{}) {
			t.Errorf("checkFrom(%s) = %+v, want nothing offered", current, got)
		}
	}
}

// A development build must not be told it is out of date — and must not reach
// the network to find that out.
func TestCheckSkipsDevBuild(t *testing.T) {
	client := &http.Client{Transport: refuse{t}}
	got, err := checkFrom(context.Background(), client, "dev", "http://example.invalid")
	if err != nil {
		t.Fatalf("checkFrom: %v", err)
	}
	if got != (Release{}) {
		t.Errorf("checkFrom = %+v, want nothing offered", got)
	}
}

type refuse struct{ t *testing.T }

func (r refuse) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Error("checkFrom reached the network for a build with no release version")
	return nil, http.ErrUseLastResponse
}
