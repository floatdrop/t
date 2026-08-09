# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Wails v3 desktop teleconference client. Media travels over Media over QUIC
(moq-go, draft-19) with no signalling server and no SFU: peers find each other
through MOQT namespace discovery on a relay and each publishes its own media.
`docs/README.md` is unusually complete — it explains the *why* behind most
design decisions here (presentation, clock tracking, reconnection, invite links,
icon). Read the relevant section before changing behaviour in those areas;
the top-level `README.md` is only the short orientation.

## Prerequisites and invariants

**`frontend/dist` must exist before any Go command works.** `main.go` does
`//go:embed all:frontend/dist`, and that directory is gitignored — so `go
build`, `go vet` and `go test` all fail on a fresh checkout until the frontend
has been built once:

```sh
cd frontend && npm ci && npm run build
```

**The macOS app must run from its `.app` bundle.** macOS kills a bare binary
that touches the camera, because the usage descriptions live in the bundle's
`Info.plist`. `wails3 task run` bundles and launches; running `bin/tlmst`
directly does not work.

## Commands

Build orchestration is Wails Taskfiles; `wails3 task` runs them (the CLI is
`go install github.com/wailsapp/wails/v3/cmd/wails3@<version from go.mod>`).

```sh
wails3 task build          # frontend + binary for the host platform
wails3 task run            # bundle bin/tlmst.dev.app and launch it
wails3 task package        # signed/packaged production build
wails3 task dev            # Wails dev mode with Vite HMR

go test . ./cmd/... ./internal/... -race   # NOT ./... — build/ios is a Wails
                                          # template package that does not
                                          # build on the desktop
go test ./internal/conf -run TestTwoParticipants -race -v   # single test
go vet . ./cmd/... ./internal/...
gofmt -l . | grep -v '^build/'        # build/ ships unformatted from the template

cd frontend && npx svelte-check --tsconfig ./tsconfig.json   # frontend typecheck
cd frontend && npm test                                      # vitest, policy modules only
```

A relay is needed to run anything end to end:
`go run github.com/floatdrop/moq-go/cmd/relay@draft-19` (self-signed cert on
`:4433`). Launch flags prefill the welcome form, which is
how two instances get started against it without clicking through twice:

```sh
bin/tlmst.dev.app/Contents/MacOS/tlmst -relay localhost:4433 -room demo \
    -nickname alice -join -debug
```

`cmd/shaper` puts a userspace bottleneck in front of one participant, which is
the only way to see the congestion paths work — the relay's TOO_FAR_BEHIND
verdict, the §8 shedding of the enhancement layer and the rebuild that answers
them never fire on a healthy link.
Point one instance at it and leave the rest on the relay directly; its doc
comment explains why the rate matters more than it looks.

The frontend's tests cover the plain-TypeScript policy modules — currently just
the grid arithmetic in `layout.ts`. Nothing renders a component: what touches a
camera, a codec or a socket is exercised by running the app.

`internal/conf`'s tests run a real moq-go relay in-process over loopback QUIC
and assert on the whole path — discovery, catalog exchange, subscription, exact
object delivery, session loss and GOAWAY. They are the fastest way to check a
wire-level change; nothing under test touches a WebView.

## Architecture

Two halves in one process, talking over a loopback WebSocket.

**The WebView owns the media.** `frontend/src/lib/capture.ts` holds the devices
and runs the WebCodecs encoders; `playback.ts` runs a decoder per announced
remote track and paints/plays it; `session.svelte.ts` is the single Svelte 5
runes store everything reads. WebKit has no `MediaStreamTrackProcessor`, so
video frames come off a `<video>` element via `requestVideoFrameCallback` and
audio PCM comes from an `AudioWorklet` (`worklets.ts`, processors as source
strings loaded through blob URLs).

**The Go half owns the MOQ session.** `internal/conf` is the conference: `Room`
(session lifecycle, `Lost()` / `Migrating()`), `publisher` (the local
participant's catalog + two LOC tracks), `remote` (one per peer), `router`
(inbound data-stream demux, one goroutine per stream), `catalog` (MSF JSON).
`internal/app` is the `bridge.Handler` and the supervisor that re-dials on loss
and replays declared track configs. `internal/telemetry` samples counters and
the QUIC qlog stream for the debug plots.

**The bridge is a WebSocket, not Wails bindings**, because bindings would
base64 every media frame through JSON. Control state is JSON text frames;
encoded media is binary frames with a fixed 24-byte header. The frontend
discovers the URL and per-run token from `/__bridge`, served by the asset
handler in `main.go`.

Naming and stream mapping (both packages document this at the top of
`internal/conf/conf.go`): namespace tuple `("tlmst", <room>, <participant-id>)`
with `catalog` / `video` / `audio` tracks under it; video is one group per GOP
opened by a keyframe, audio a fixed 25-object (500 ms) cadence. Nickname and
build version ride in the catalog as MSF §5.1 producer root fields.

## Things that must stay in step

- `internal/bridge/protocol.go` + `frame.go` ↔ `frontend/src/lib/protocol.ts` —
  the wire format has two hand-written implementations.
- `parseInviteURL` in `main.go` ↔ `parseInviteLink` in
  `frontend/src/lib/invite.ts` — `invite_test.go` pins them to one dialect.
- `build/config.yml`'s `version` is the single source of truth; the binary reads
  it back via the embed in `main.go` and `internal/version`.
  `TestParseRealConfig` fails if the field moves.
- `internal/conf/reorder.go` assumes subgroup 0 is the temporal base layer, and
  `internal/conf/publisher.go` is what makes that true by writing layer *L* to
  subgroup *L*. The base layer is emitted on arrival and never held or dropped;
  reversing that mapping would silently invert which layer is disposable.

## Gotchas

- `wails3 task common:update:build-assets` overwrites `build/darwin/Info.plist`.
  Two hand edits must be reapplied: the `NSCameraUsageDescription` /
  `NSMicrophoneUsageDescription` keys, and the removal of `CFBundleIconName`.
- `mediapermission_darwin.m` patches a missing `WKUIDelegate` method onto Wails'
  delegate class at startup; without it every `getUserMedia` fails before macOS
  is consulted. It stands down if a later Wails release implements this.
- TLS verification is deliberately off (`conf.Config.Insecure`, set in
  `internal/app/app.go`) for self-signed development relays.
- moq-go has no tagged releases, so `go.mod` holds a pseudo-version that
  `go get -u` will not move. Bump it deliberately:
  `go get github.com/floatdrop/moq-go@<branch-or-commit>`.
- The `tlmst://` scheme only resolves for a bundle the OS knows about; register
  a dev build once with `lsregister -f bin/tlmst.dev.app`.
