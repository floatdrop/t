<p align="center">
  <img src="icon.svg" alt="" width="88" height="88">
</p>

# tlmst

A desktop teleconference client that carries every participant's camera and
microphone over **Media over QUIC**, using
[moq-go](https://github.com/floatdrop/moq-go) (draft-19) as the transport.

There is no signalling server and no SFU: participants find each other through
MOQT namespace discovery on a relay, and each publishes its own media directly.

## How it works

The process is split in two halves that talk over a loopback WebSocket.

```
┌──────────────────────────── tlmst.app ────────────────────────────┐
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

### Naming and discovery

Each participant owns a namespace tuple and publishes three tracks under it:

```
("tlmst", <room>, <participant-id>)
    catalog   MSF catalog (draft-ietf-moq-msf-01) — declares the media tracks
    video     LOC-packaged H.264 (Annex B)
    audio     LOC-packaged Opus
```

Discovery falls out of that layout. A participant announces itself with
`PUBLISH_NAMESPACE` and watches the room with `SUBSCRIBE_NAMESPACE` on the
prefix `("tlmst", <room>)`; the relay then reports arrivals as `NAMESPACE` and
departures as `NAMESPACE_DONE`. Each peer's catalog is fetched with a Relative
Joining FETCH so a late joiner sees a catalog that was published before it
arrived.

The nickname travels in the catalog (a producer-defined root field), not in the
namespace, so it can be anything and can repeat.

### Stream mapping

**Video** uses one group per GOP: a keyframe opens a new group and the frames
after it are objects `1..n` in a single subgroup stream. A relay can therefore
drop a whole group under congestion and land the subscriber exactly on the next
keyframe. H.264 is encoded in Annex B, so SPS/PPS travel in-band with every
keyframe and no out-of-band config is needed.

**Audio** has no keyframes, so it uses a fixed cadence — a new group every 25
frames, which is 500 ms at the 20 ms framing WebCodecs produces. Opus's
`OpusHead` is carried in the catalog's `initDataList`, and also stamped on the
first object of each group, so a subscriber can configure a decoder from the
first object it receives.

Every audio object also carries LOC's **AudioLevel** property (§2.3.3.2) — the
RFC 6464 byte holding a voice-activity flag and a magnitude in -dBov. That is
what lights a remote participant's speaking border: a peer knows who is talking
without decoding their audio, and a relay could prioritise on it.

[draft-lcurley-moq-hang](https://datatracker.ietf.org/doc/draft-lcurley-moq-hang/)
was the idea source for this shape; it is not a spec this implements.

### Audio processing

**Echo cancellation is the platform's.** `getUserMedia`'s `echoCancellation`
constraint engages macOS's own AEC, which can see the render reference signal —
what the speakers are actually playing. Code in the page cannot, so a WASM echo
canceller here would be strictly worse. What the app does instead is ask for the
constraint and report what the browser *actually applied*, which is visible in
the Tracks & codecs panel. That matters: on this machine WebKit granted
`echoCancellation` but silently declined `noiseSuppression` and
`autoGainControl`.

**Noise suppression is local**, running after whatever the platform did.
[RNNoise](https://github.com/xiph/rnnoise) — a small recurrent network — removes
the stationary noise a general-purpose suppressor leaves behind, and returns a
voice-activity probability as a side effect, which is what drives the speaking
indicator. It runs on the main thread between the capture AudioWorklet and the
AudioEncoder: the PCM already makes that hop (no `MediaStreamTrackProcessor` in
WebKit), and an `AudioWorkletGlobalScope` has neither `fetch` nor `atob` to
instantiate a wasm module with. Cost is about 1% of a core.

The model is a ~5 MB lazily-imported chunk, so startup does not pay for it, and
a failure to load degrades to platform-only suppression plus an energy-based VAD
rather than breaking capture. Toggle it on the welcome screen.

### Voice activity

A participant's tile grows a green ring while they speak, drawn as an inset
`box-shadow` so it can never reflow the grid mid-call. The local ring comes from
RNNoise's probability with fast attack and a 300 ms release — a border that
flickers on every inter-word pause is worse than none. Remote rings come from
the AudioLevel property described above, latched with a short timeout so a gap
in delivery does not read as silence.

## Running

You need a relay. The simplest is moq-go's:

```sh
cd ../moq-go && go run ./cmd/relay      # ephemeral self-signed cert on :4433
```

Then, in this directory:

```sh
wails3 task build     # builds the frontend and the binary
wails3 task run       # bundles bin/tlmst.dev.app and launches it
```

Enter the relay address, a room, and a nickname on the welcome screen; the
camera preview doubles as the permission prompt. Two instances in the same room
see each other.

**It must run from the `.app` bundle**, which is what `wails3 task run` does.
macOS terminates a bare binary that touches the camera, because the usage
descriptions live in the bundle's `Info.plist`.

### Launch flags

The flags prefill (and optionally submit) the welcome form — handy for starting
two instances against a relay without clicking through twice:

```sh
bin/tlmst.dev.app/Contents/MacOS/tlmst \
    -relay localhost:4433 -room demo -nickname alice -join -debug
```

| Flag | Effect |
|---|---|
| `-relay` | relay address: `host:port`, `moqt://…`, or `https://…` for WebTransport |
| `-room` | room identifier |
| `-nickname` | display name |
| `-join` | join immediately instead of waiting for a click |
| `-debug` | open the debug drawer at start (`Cmd+D` toggles it) |
| `-debug-tab` | which tab to open: `transport`, `tracks`, or `logs` |

The welcome screen reads the same values from its own URL query, which is how
these flags reach it.

## Devices

The welcome screen picks camera, microphone, resolution and bitrates before
joining, and **Devices** in the call header changes them without leaving the
room. That control is not a convenience: joining by an invite link skips the
welcome screen entirely, so it is the only place those choices can be made on
that path.

A switch rebuilds just the local capture pipeline. The MOQ publications belong
to the backend and stay open, so the new frames flow into the same tracks; a
resolution change re-declares the video track, which republishes the catalog and
makes subscribers reconfigure under a fresh handle. The media clock and audio
sample counter are carried across the swap so timestamps stay monotonic — a
subscriber mid-decode must not see them jump backwards. `TestTrackReconfiguration`
in `internal/conf` covers that wire behaviour.

## Invite links

**Copy invite** in the call header puts a link like this on the clipboard:

```
tlmst://localhost:4433/standup
```

The relay is the authority and the room is the path, so the link reads as an
address. `tlmst` is registered as a URL scheme (`CFBundleURLTypes` in
`build/darwin/Info.plist`), which makes it clickable: macOS launches the app and
joins, or — if a call is already in progress — offers to switch rather than
yanking you out of the conversation you are in. Wails already installs the Apple
Event handler for custom schemes and republishes it as
`ApplicationLaunchedWithUrl`, so no native code was needed for this.

A relay that is more than a bare `host:port` — a `moqt://` or `https://` URL,
possibly with a path — cannot be expressed by an authority alone. Those links
keep the readable authority and carry the exact value in a `relay` query
parameter, which wins when present:

```
tlmst://relay.example.com/r1?relay=https%3A%2F%2Frelay.example.com%2Flive
```

The welcome screen also accepts a link **pasted** into its relay or room field,
for chat clients that will not linkify an unknown scheme. The format is parsed
in two places — `parseInviteURL` in `main.go` and `parseInviteLink` in
`frontend/src/lib/invite.ts` — and `invite_test.go` pins them to the same
dialect.

Scheme registration only happens for an app the OS knows about. For a dev build
that means registering the bundle once:

```sh
/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister \
    -f bin/tlmst.dev.app
```

Windows and Linux need their own registration — registry keys written by an
installer, and `MimeType=x-scheme-handler/tlmst` in the `.desktop` file — neither
of which is wired up yet.

## Debug panels

Click any tab to open the drawer, the × to close it, or `Cmd+D` to toggle.
Drag its top edge to resize. Three tabs:

- **Transport** — live plots over a one-minute window: round-trip time, packet
  loss, QUIC throughput, MOQ object throughput, congestion window, objects per
  second. RTT is plotted as smoothed against the interval's *peak*, not its
  minimum: the minimum is the path's propagation floor and near-constant, so as
  a line it says nothing, while the gap between smoothed and peak is queueing
  delay and the spike a smoothed average hides is what makes a call stutter.
  The floor is still worth knowing, so it stays as a number on the tile. Plus a table view of every plotted measure. The transport numbers come
  from the connection's own qlog event stream (`internal/telemetry/quictrace.go`);
  as of quic-go v0.61 that is the only way to read RTT and loss.
- **Tracks & codecs** — per-track bytes, objects and groups on the wire, next to
  the WebView's encoder and decoder counters (codec, decoded resolution, fps,
  queue depth, dropped frames, audio buffer depth). Reading them side by side is
  what localises a fault: a track carrying bytes while its decoder sits at 0 fps
  means the problem is in the WebView, not the network. Also reports which
  microphone processing the platform actually applied, and live voice activity.
- **Logs** — everything the backend logs, including moq-go's own output, plus
  the WebView's capture and decode events. The level selector changes the
  backend's `slog` level at runtime, so moq-go's per-message DEBUG output can be
  turned on mid-call.

## Icon

`icon.svg` is the mark: a lowercase "t" whose period is the same green as the
speaking indicator, so it reads as "a call is in progress" rather than just a
letter. It is drawn as strokes rather than `<text>` — an icon must not depend on
a font being installed wherever it is rendered — and sits on a tile in the app's
own background colour so it needs no light/dark variant.

It is also the app icon. `build/appicon.png` is a 1024px render of it, which
`wails3 task build` turns into `darwin/icons.icns` and `windows/icon.ico`:

```sh
rsvg-convert -w 1024 -h 1024 icon.svg -o build/appicon.png
```

`build/appicon.icon/` holds the glyph-only variant for macOS 26's Icon Composer
format, where the OS draws the tile and its lighting from a supplied layer.
Building its `Assets.car` needs Xcode's `actool`, which the Command Line Tools
alone do not provide, so `CFBundleIconName` is removed from both Info.plists and
macOS falls back to `CFBundleIconFile` → `icons.icns`. With full Xcode
installed, `wails3 task common:generate:icons` will produce `Assets.car` and the
key can go back.

## Continuous integration

Two workflows, both driving the same `wails3 task` targets used locally.

`.github/workflows/ci.yml` runs on every push and pull request: `gofmt`,
`go vet`, `svelte-check`, and `go test -race` once on Linux (nothing under test
touches a WebView), then a build on macOS, Linux and Windows to prove each
still compiles and links.

`.github/workflows/release.yml` runs on a `v*` tag, or by hand from the Actions
tab. It packages each platform, signs where credentials exist, writes a
`.sha256` beside every archive, and on a tag publishes them as a GitHub release:

| Platform | Artifact |
|---|---|
| macOS | `tlmst-<version>-macos-universal.zip` — a universal (arm64 + amd64) `.app` |
| Linux | `tlmst-<version>-linux-amd64.tar.gz` |
| Windows | `tlmst-<version>-windows-amd64.zip` |

Both workflows check the repository out into a `tlmst/` subdirectory and
`floatdrop/moq-go` beside it, because `go.mod` resolves moq-go through
`replace … => ../moq-go`. The ref defaults to `draft-19` and is overridable on
a manual release run.

### Signing

Signing is optional: with no secrets configured everything still builds, so a
fork works out of the box. What you get without them:

- **macOS** — the `.app` is ad-hoc signed by `wails3 task package`. It runs
  locally, but Gatekeeper refuses it once it has been downloaded.
- **Windows** — unsigned. It runs, but SmartScreen warns.
- **Linux** — unsigned either way; there is no code-signing convention. The
  `.sha256` is the integrity signal.

### Running an unsigned build

There is no free path to a signed, notarized macOS app: a Developer ID
certificate and notarization both require the Apple Developer Program
($99/year). Without it the release `.app` is ad-hoc signed, which is enough to
run on the machine that built it but not enough for one that downloaded it —
macOS quarantines the download and Gatekeeper refuses to open it.

Three ways round that, best first:

1. **Build it yourself.** A locally built binary is never quarantined, so
   `wails3 task build && wails3 task run` sidesteps the problem entirely. For a
   tool aimed at people who already have Go and Node installed, this is the
   primary route, not a fallback.
2. **Open it anyway.** Launch the downloaded app once and let macOS block it,
   then go to System Settings → Privacy & Security, where an **Open Anyway**
   button now appears for it. Confirm once and it launches from then on. (Older
   macOS put this behind Control-click → Open on the app itself.)
3. **Strip the quarantine flag**, if you would rather do it from a terminal —
   and having checked the `.sha256` first, since this bypasses the warning
   rather than answering it:

   ```sh
   xattr -dr com.apple.quarantine /Applications/tlmst.app
   ```

Windows is less strict: an unsigned `.exe` runs after clicking through
SmartScreen's "More info → Run anyway". Linux has nothing to bypass.

Configure these repository secrets to sign. macOS signing and notarization are
separate: with only the first group you get a signature but still a first-run
warning, since only notarization removes it.

| Secret | Purpose |
|---|---|
| `MACOS_CERTIFICATE` | Developer ID Application cert as a base64 `.p12` |
| `MACOS_CERTIFICATE_PASSWORD` | password for that `.p12` |
| `MACOS_SIGNING_IDENTITY` | e.g. `Developer ID Application: You (TEAMID)` |
| `MACOS_NOTARY_APPLE_ID` | Apple ID for notarization |
| `MACOS_NOTARY_PASSWORD` | app-specific password for that Apple ID |
| `MACOS_NOTARY_TEAM_ID` | Apple Developer team ID |
| `WINDOWS_CERTIFICATE` | Authenticode cert as a base64 `.pfx` |
| `WINDOWS_CERTIFICATE_PASSWORD` | password for that `.pfx` |

Base64-encode a certificate with `base64 -i cert.p12 | pbcopy`.

## Tests

`internal/conf` has integration tests that run a real moq-go relay in-process
over loopback QUIC and assert on the whole path — discovery, catalog exchange,
subscription, and exact object delivery:

```sh
go test . ./internal/... -race
```

(`./...` would pull in `build/ios`, a Wails template package that does not build
on the desktop.)

## Platform notes

Development happened on macOS, and two things there are worth knowing.

**getUserMedia needs a runtime patch.** WKWebView asks its `WKUIDelegate` for
permission before handing a page a camera, and denies capture outright when the
delegate does not implement the request method. Wails v3.0.0-beta.3 implements
that only on its Linux backend, so `mediapermission_darwin.m` installs the
missing method on Wails' delegate class at startup. Without it every
`getUserMedia` call fails before macOS is ever consulted. If a later Wails
release implements this itself, the shim detects that and stands down — the file
can then be deleted.

**WebKit has no `MediaStreamTrackProcessor`.** There is no Insertable Streams
path from a `MediaStreamTrack` to WebCodecs, so video frames are pulled off a
`<video>` element with `requestVideoFrameCallback` and audio PCM comes from an
`AudioWorklet`. See `frontend/src/lib/capture.ts`.

## Known limitations

- **TLS verification is off.** Development relays use self-signed certificates
  and there is no UI for trusting one. Fix before any deployment where the relay
  identity matters (`internal/app/app.go`, `conf.Config.Insecure`).
- **No end-to-end encryption.** LOC public properties — timestamps, codec
  configs — are visible to relays by design. The draft's SecureObjects mechanism
  is not implemented in moq-go yet.
- **`go.mod` pins moq-go through a `replace`** to `../moq-go`, since the module
  has no published versions. That means moq-go must be checked out beside this
  repository. Change it to a module version when one exists.
- **Departure detection does not rely on `NAMESPACE_DONE` alone.** moq-go's
  relay only sends it to subscribers that were already watching when the
  namespace was announced, so a peer who joined later would never hear about a
  departure. The app treats the end of a peer's catalog subscription as a
  departure too, which also covers a peer that crashes.
- **Regenerating build assets overwrites the Info.plist.** `wails3 task
  common:update:build-assets` rewrites `build/darwin/Info.plist` from
  `build/config.yml`. Two hand edits have to be reapplied afterwards: the
  `NSCameraUsageDescription` / `NSMicrophoneUsageDescription` keys, which
  `config.yml` has no field for, and the removal of `CFBundleIconName` (see
  "Icon").
- One video and one audio track per participant; no simulcast, no layer
  switching, no bandwidth-driven quality adaptation.
- **Playback assumes groups arrive in publication order.** Each group is its
  own subgroup stream, read on its own goroutine, so two groups in flight at
  once may be delivered in either order — and the audio player is a ring buffer
  fed in arrival order, with no reordering. Live capture never produces that
  case (audio groups are 500 ms apart and video groups a keyframe interval
  apart, so they are never simultaneously in flight), but a burst of publishes
  does. A real jitter buffer keyed on the LOC timestamp would remove the
  assumption.
- **Only the macOS build has been run.** The Linux and Windows targets are
  verified to compile and link, and the window grants camera and microphone
  through Wails' cross-platform `Permissions` option (which those two backends
  honour and macOS ignores), but neither has been exercised in a real call.
- A subscriber discards inbound video until the first keyframe, so joining
  mid-GOP costs up to the keyframe interval (2 s) before the first frame paints.
  Those frames show up as `dropped` in the decoders table and are expected.
