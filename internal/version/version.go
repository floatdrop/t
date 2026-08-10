// Package version reports which build of t is running.
//
// The version lives in build/config.yml, which is what the packaging reads
// when it stamps the macOS bundle and names the release archives. This package
// reads the same field rather than keeping a second copy of it: a constant here
// would be one more thing to remember on a release, and the failure mode of
// forgetting is a build that lies about itself to every participant in the room
// — quietly, and for as long as nobody checks.
//
// The file cannot be embedded here, since go:embed cannot reach outside its own
// package directory. main.go embeds it and hands the bytes to Parse.
package version

import (
	"regexp"
	"strconv"
	"strings"
)

// Dev is what Parse returns when the version cannot be read, and what an
// unpackaged build effectively is. Deliberately not a number: it must not
// compare as newer or older than a release, and anything that looks like a
// version invites exactly that.
const Dev = "dev"

// versionLine matches an indented, uncommented `version:` key with a value
// that starts with a digit. The indent is what distinguishes the field we want
// from the Taskfile schema's own `version: '3'` at column zero, and the digit
// keeps a quoted schema version from qualifying either.
var versionLine = regexp.MustCompile(`(?m)^[ \t]+version:[ \t]*["']?([0-9][^"'#\s]*)`)

// Parse pulls the application version out of build/config.yml.
//
// Scanned rather than YAML-decoded, to avoid taking a dependency for one
// scalar. The scan is anchored on the `info:` block instead of simply taking
// the first match, because config.yml also carries a commented-out `ios:`
// section with a version of its own — one uncommenting away from silently
// becoming the number this returns.
func Parse(configYAML []byte) string {
	info := infoBlock(string(configYAML))
	m := versionLine.FindStringSubmatch(info)
	if m == nil {
		return Dev
	}
	return m[1]
}

// infoBlock returns the body of the top-level `info:` mapping, or the whole
// document if there is no such key.
func infoBlock(doc string) string {
	lines := strings.Split(doc, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "info:") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return doc
	}
	for i := start; i < len(lines); i++ {
		line := lines[i]
		// The next line at column zero that is not blank and not a comment
		// ends the block.
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") ||
			strings.HasPrefix(line, "#") {
			continue
		}
		return strings.Join(lines[start:i], "\n")
	}
	return strings.Join(lines[start:], "\n")
}

// IsRelease reports whether v names a real release, as opposed to a build that
// has no business being compared against one.
func IsRelease(v string) bool {
	return v != "" && v != Dev
}

// Newer reports whether latest is a strictly later release than current.
//
// Only the numeric fields are compared, and a version carrying anything else —
// a `-rc1`, a `+build` — is never considered newer. An update prompt is a
// nudge to leave the app, so the bar for showing one is that we are certain,
// and the cost of staying quiet about a release candidate is nothing.
func Newer(current, latest string) bool {
	if !IsRelease(current) || !IsRelease(latest) {
		return false
	}
	c, ok := fields(current)
	if !ok {
		return false
	}
	l, ok := fields(latest)
	if !ok {
		return false
	}
	for i := range 3 {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// fields splits a plain "1.2.3" — with an optional leading v, since that is how
// the tags are written — into its three numbers. Anything else fails.
func fields(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
