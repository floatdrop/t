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

The welcome screen reads the same values from the URL query, so a room is also
shareable as a link.

## Debug panels

Click any tab to open the drawer, the × to close it, or `Cmd+D` to toggle.
Drag its top edge to resize. Three tabs:

- **Transport** — live plots over a one-minute window: round-trip time, packet
  loss, QUIC throughput, MOQ object throughput, congestion window, objects per
  second. Plus a table view of every plotted measure. The transport numbers come
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

## Tests

`internal/conf` has integration tests that run a real moq-go relay in-process
over loopback QUIC and assert on the whole path — discovery, catalog exchange,
subscription, and exact object delivery:

```sh
go test ./internal/... -race
```

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
- **`go.mod` pins moq-go through a `replace`** to `/Users/floatdrop/moq-go`,
  since it has no published versions. Change that to a module version when one
  exists.
- **Departure detection does not rely on `NAMESPACE_DONE` alone.** moq-go's
  relay only sends it to subscribers that were already watching when the
  namespace was announced, so a peer who joined later would never hear about a
  departure. The app treats the end of a peer's catalog subscription as a
  departure too, which also covers a peer that crashes.
- **Regenerating build assets overwrites the Info.plist.** `wails3 task
  common:update:build-assets` rewrites `build/darwin/Info.plist` from
  `build/config.yml`, which has no field for usage descriptions —
  `NSCameraUsageDescription` and `NSMicrophoneUsageDescription` must be
  re-added by hand afterwards.
- One video and one audio track per participant; no simulcast, no layer
  switching, no bandwidth-driven quality adaptation.
- A subscriber discards inbound video until the first keyframe, so joining
  mid-GOP costs up to the keyframe interval (2 s) before the first frame paints.
  Those frames show up as `dropped` in the decoders table and are expected.
