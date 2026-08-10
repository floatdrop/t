<p align="center">
  <img src="icon.svg" alt="" width="88" height="88">
</p>

# t

A desktop teleconference client that carries every participant's camera and
microphone over **Media over QUIC**, using
[moq-go](https://github.com/floatdrop/moq-go) (draft-19) as the transport.

There is no signalling server and no SFU: participants find each other through
MOQT namespace discovery on a relay, and each publishes its own media directly.

## How it works

The process is split in two halves that talk over a loopback WebSocket.

```
┌────────────────────────────── t.app ──────────────────────────────┐
│  WKWebView (wails://localhost)            Go                      │
│  ┌──────────────────────────────┐   ┌───────────────────────────┐ │
│  │ Svelte UI                    │   │ bridge (internal/bridge)  │ │
│  │  · welcome / device pickers  │◄─►│  JSON control +           │ │
│  │  · video grid                │ws │  binary media frames      │ │
│  │  · debug panels              │   ├───────────────────────────┤ │
│  │                              │   │ conf (internal/conf)      │ │
│  │ getUserMedia                 │   │  · namespace discovery    │ │
│  │ VideoEncoder / AudioEncoder  │   │  · MSF catalog            │ │
│  │ VideoDecoder / AudioDecoder  │   │  · publish / subscribe    │ │
│  │ canvas + AudioWorklet        │   ├───────────────────────────┤ │
│  └──────────────────────────────┘   │ moq-go session ───────────┼─┼──► relay
│                                     │ telemetry (qlog, logs)    │ │    (QUIC)
│                                     └───────────────────────────┘ │
└───────────────────────────────────────────────────────────────────┘
```

The **WebView owns the media**: it holds the devices, runs the WebCodecs
encoders and decoders, and paints the grid. The **Go side owns the MOQ
session**: it dials the relay, publishes the local tracks, discovers the room,
and subscribes to everyone in it.

A WebSocket rather than Wails bindings because media is binary and hot —
bindings would base64 every frame through JSON, while binary frames on loopback
measure ~400 MB/s up and ~1 GB/s down in this WebView.

Each participant publishes a catalog and its two media tracks under a namespace
of its own, and the relay's namespace discovery is what makes a room a room —
[Media and transport](docs/README.md#media-and-transport) has the details.

## Running

You need a relay. The simplest is moq-go's, which needs no checkout:

```sh
go run github.com/floatdrop/moq-go/cmd/relay@draft-19   # self-signed cert on :4433
```

Then, in this directory:

```sh
wails3 task build     # builds the frontend and the binary
wails3 task run       # bundles bin/t.dev.app and launches it
```

Enter the relay address, a room, and a nickname on the welcome screen; the
camera preview doubles as the permission prompt. Two instances in the same room
see each other — [launch flags](docs/README.md#launch-flags) start the second
one without clicking through the form again.

**It must run from the `.app` bundle**, which is what `wails3 task run` does.
macOS terminates a bare binary that touches the camera, because the usage
descriptions live in the bundle's `Info.plist`.

Prebuilt archives are on the [releases
page](https://github.com/floatdrop/tlmst/releases). They are not signed or
notarized, so macOS quarantines the download and Gatekeeper refuses it; [Running
an unsigned build](docs/README.md#running-an-unsigned-build) covers the ways
round that, of which building it yourself is the simplest.

## Tests

`internal/conf` has integration tests that run a real moq-go relay in-process
over loopback QUIC and assert on the whole path — discovery, catalog exchange,
subscription, and exact object delivery:

```sh
go test . ./internal/... -race
```

(`./...` would pull in `build/ios`, a Wails template package that does not build
on the desktop.)

## Documentation

[docs/README.md](docs/README.md) is the long-form version — what each design
decision cost before it was made:

| | |
|---|---|
| [Media and transport](docs/README.md#media-and-transport) | naming and discovery, stream mapping, presentation and mixing, audio processing, voice activity |
| [Launch flags](docs/README.md#launch-flags) | prefilling and submitting the welcome form from the command line |
| [Devices](docs/README.md#devices) | camera, microphone, the resolution ladder, and what Auto sizes against |
| [Version and updates](docs/README.md#version-and-updates) | where the version comes from and how a newer release is offered |
| [Losing the relay](docs/README.md#losing-the-relay) | reconnection, and migrating on GOAWAY |
| [Invite links](docs/README.md#invite-links) | the `t://` scheme |
| [Debug panels](docs/README.md#debug-panels) | transport plots, per-track counters, live logs |
| [Icon](docs/README.md#icon) | the mark, and why it is not an Icon Composer bundle |
| [Continuous integration](docs/README.md#continuous-integration) | the two workflows, signing, and running an unsigned build |
| [Platform notes](docs/README.md#platform-notes) | the WKWebView permission shim, and WebKit's missing pieces |
| [Known limitations](docs/README.md#known-limitations) | what this does not do yet |

Three worth knowing before you point it at anything real: **TLS verification is
off**, since development relays are self-signed and there is no UI for trusting
one; there is **no end-to-end encryption**, so timestamps and codec configs are
visible to relays by design; and **only the macOS build has been run** — Linux
and Windows are verified to compile and link, nothing more.
