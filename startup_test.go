package main

import (
	"net/url"
	"strings"
	"testing"
)

// TestStartURL pins the query string against what Welcome.svelte reads. The
// two live in different languages, so nothing but a test keeps the parameter
// names in step — and one of them, `user`, exists solely to be distinguishable
// from `nickname`, which a rename would silently undo.
func TestStartURL(t *testing.T) {
	tests := []struct {
		name string
		in   launch
		want url.Values
	}{{
		// The ordinary interactive start: no flags, but the account name is
		// always known, so the welcome screen can offer it as a nickname.
		name: "only the system user",
		in:   launch{user: "floatdrop"},
		want: url.Values{"user": {"floatdrop"}},
	}, {
		// Both are present and separate. Collapsing them would let the account
		// name override a nickname the user had chosen before.
		name: "nickname and user are distinct parameters",
		in:   launch{nickname: "alice", user: "floatdrop"},
		want: url.Values{"nickname": {"alice"}, "user": {"floatdrop"}},
	}, {
		name: "every flag",
		in: launch{
			relay:    "localhost:4433",
			room:     "demo",
			nickname: "alice",
			user:     "floatdrop",
			join:     true,
			debug:    true,
			debugTab: "logs",
		},
		want: url.Values{
			"relay":    {"localhost:4433"},
			"room":     {"demo"},
			"nickname": {"alice"},
			"user":     {"floatdrop"},
			"join":     {"1"},
			"debug":    {"1"},
			"debugTab": {"logs"},
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := startURL(tc.in)
			raw, ok := strings.CutPrefix(got, "/?")
			if !ok {
				t.Fatalf("startURL(%+v) = %q, want a /? query string", tc.in, got)
			}
			values, err := url.ParseQuery(raw)
			if err != nil {
				t.Fatalf("startURL(%+v) = %q: %v", tc.in, got, err)
			}
			if len(values) != len(tc.want) {
				t.Errorf("startURL(%+v) = %q, want exactly %v", tc.in, got, tc.want)
			}
			for key, want := range tc.want {
				if values.Get(key) != want[0] {
					t.Errorf("%s = %q, want %q", key, values.Get(key), want[0])
				}
			}
		})
	}
}

// A start with nothing at all to say must not append an empty query string:
// "/?" would be served, and read, as a different path from "/".
func TestStartURLEmpty(t *testing.T) {
	if got := startURL(launch{}); got != "/" {
		t.Errorf("startURL(launch{}) = %q, want %q", got, "/")
	}
}

// TestSystemUserName only asserts what holds on any machine the tests run on:
// an account name exists and is a bare name. The value itself is whoever is
// running the tests.
func TestSystemUserName(t *testing.T) {
	got := systemUserName()
	if got == "" {
		t.Fatal("systemUserName() is empty; the welcome screen has no name to offer")
	}
	if strings.ContainsAny(got, `\ `) {
		t.Errorf("systemUserName() = %q, want no domain prefix and no spaces", got)
	}
}
