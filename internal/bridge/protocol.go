// Package bridge carries control messages and media frames between the
// WebView frontend and the Go backend over a loopback WebSocket.
//
// The WebView owns capture and playback: it holds the camera and
// microphone, runs the WebCodecs encoders and decoders, and paints the
// video grid. The Go side owns the MOQ session. Everything in between
// travels over one WebSocket connection: control state as JSON text
// frames, encoded media as binary frames with the compact header defined
// below.
//
// A WebSocket rather than Wails bindings because media is binary and hot:
// bindings marshal through JSON (so every frame would pay base64), while
// binary WebSocket frames on loopback measure ~400 MB/s up and ~1 GB/s
// down in this WebView — three orders of magnitude more than a conference
// needs.
package bridge

import "encoding/json"

// Media frame header. Fixed 24 bytes, big-endian, followed by an optional
// codec-config blob and then the encoded frame payload:
//
//	 0      u8      Version (always FrameVersion)
//	 1      u8      Kind (KindVideo / KindAudio)
//	 2      u8      Flags (FlagKeyFrame, FlagAudioLevel, TemporalLayer)
//	 3      u8      AudioLevel — valid only with FlagAudioLevel
//	 4..8   u32     Handle — identifies the track (see below)
//	 8..16  u64     Timestamp in microseconds, from the encoder
//	16..20  u32     ConfigLen — length of the config blob that follows
//	20..24  u32     PayloadLen — length of the payload after the config
//
// Handle namespaces differ by direction. Frontend to backend, it names
// one of the two local publications (HandleLocalVideo / HandleLocalAudio).
// Backend to frontend, it is a backend-assigned per-remote-track handle,
// announced ahead of any frame by a "track" control message.
const (
	FrameVersion   = 1
	FrameHeaderLen = 24

	KindVideo = 0
	KindAudio = 1

	FlagKeyFrame = 1 << 0
	// FlagAudioLevel marks the AudioLevel byte as carrying a real
	// measurement. Needed because level 0 means "loudest", not "absent".
	FlagAudioLevel = 1 << 1

	// TemporalLayerShift and TemporalLayerMask carve the frame's temporal
	// layer out of the flags byte: bits 2-3, so layers 0-3.
	//
	// In the flags byte rather than a field of its own because the header is
	// a fixed 24 bytes with two hand-written implementations, and widening it
	// would be a wire break for a value that needs two bits. Byte 3 was the
	// other candidate and is worse: it belongs to audio, and overloading it
	// per-kind is how the two implementations drift.
	//
	// Zero is the base layer, which is also what a frame from an encoder
	// producing no temporal layers reports — so an unlayered stream is layer
	// 0 throughout and needs no special case anywhere.
	TemporalLayerShift = 2
	TemporalLayerMask  = 0x3 << TemporalLayerShift

	// MaxTemporalLayer is the highest layer the two bits above can carry.
	MaxTemporalLayer = 3
)

// Handles for the two tracks the frontend publishes. Remote handles are
// assigned from HandleRemoteBase upward so they can never collide.
const (
	HandleLocalVideo = 0
	HandleLocalAudio = 1
	// HandleLocalVideoLow is the second, smaller encoding of the same camera —
	// see KindVideoLow. Its frames are KindVideo like any other picture; only
	// the handle says which encoding they belong to.
	HandleLocalVideoLow = 2
	HandleRemoteBase    = 16
)

// MediaFrame is one encoded chunk in flight over the bridge.
type MediaFrame struct {
	Kind      uint8
	Handle    uint32
	Timestamp uint64
	KeyFrame  bool
	// Config is the codec description (WebCodecs
	// VideoDecoderConfig.description / AudioDecoderConfig.description).
	// Present only on frames where the encoder emitted a new config —
	// which for Opus is the first frame, and for H.264 in Annex B is
	// never, since SPS/PPS travel in-band.
	Config  []byte
	Payload []byte

	// AudioLevel is the RFC 6464 byte LOC's AudioLevel property carries
	// (§2.3.3.2): bit 7 is voice activity, bits 0-6 the magnitude in
	// -dBov, where 0 is loudest and 127 is silence. Valid only when
	// HasAudioLevel is set. It drives the speaking indicator, so it
	// travels with every audio frame in both directions.
	AudioLevel    uint8
	HasAudioLevel bool

	// TemporalLayer is which temporal layer of an SVC encoding this frame
	// belongs to: 0 is the base, which every higher layer references and which
	// decodes on its own. It decides the subgroup the publisher writes the
	// frame to, so a subscriber or relay can shed the upper layers without
	// touching the base. Always 0 for audio, and for video from an encoder
	// configured without temporal layers.
	TemporalLayer uint8
}

// ---- control messages: frontend to backend ----------------------------

// ClientMessage is a JSON control frame from the WebView.
type ClientMessage struct {
	Type string `json:"type"`

	Join     *JoinRequest  `json:"join,omitempty"`
	Track    *TrackConfig  `json:"track,omitempty"`
	Stats    *ClientStats  `json:"stats,omitempty"`
	LogLevel string        `json:"logLevel,omitempty"`
	Report   *ClientReport `json:"report,omitempty"`
	// Untrack names a kind ("video"/"audio") the frontend has stopped
	// publishing, so the catalog stops advertising it.
	Untrack string `json:"untrack,omitempty"`
	// OpenURL is a link to hand to the OS browser. See MsgOpenURL.
	OpenURL string `json:"openUrl,omitempty"`
	// Interest is which remote video is worth receiving. See MsgInterest.
	Interest *Interest `json:"interest,omitempty"`
}

// Interest is the frontend's account of which remote participants' video it
// can currently make use of: the tiles on screen, plus whatever a scroll is
// about to bring on.
//
// Only video. Audio is never gated on being visible — someone speaking
// off-screen has to be heard, and the speaking indicator is what makes anyone
// scroll to them in the first place. It is also 32 kbps against video's
// 1.5 Mbps, so there is nothing to win by dropping it.
type Interest struct {
	// Video lists participant IDs whose video is wanted. An empty list means
	// none, which is a real answer — everyone can be scrolled off screen.
	Video []string `json:"video"`
	// Low is the subset of Video whose tile is small enough that the
	// publisher's smaller encoding will do. Anything here that is not also in
	// Video is not subscribed at all: this says which encoding, never whether.
	Low []string `json:"low,omitempty"`
}

// Client message types.
const (
	MsgJoin     = "join"     // dial the relay and start publishing
	MsgLeave    = "leave"    // tear the session down
	MsgTrack    = "track"    // declare the codec config for a local track
	MsgStats    = "stats"    // frontend-side encode/decode counters
	MsgLogLevel = "logLevel" // change the backend slog level
	MsgReport   = "report"   // a frontend-side event worth logging
	// MsgUntrack withdraws a local track. Turning the camera off stops the
	// frames, but a catalog that still declares video leaves every
	// subscriber holding a decoder and showing its last frame forever — so
	// stopping has to be said, not merely done.
	MsgUntrack = "untrack"
	// MsgOpenURL hands a link to the OS. The WebView must not navigate to it
	// itself — that would replace the app with a web page and there would be
	// no way back — and a target=_blank anchor generally does nothing here.
	MsgOpenURL = "openUrl"

	// MsgInterest carries which remote participants' video is worth
	// receiving — the tiles on screen, plus a margin for scrolling.
	MsgInterest = "interest"
)

// ClientReport is something the frontend wants in the shared log: a
// capture failure, a decoder error, a lifecycle event. Without this the
// two halves keep separate logs and the debug panel only shows half the
// story.
type ClientReport struct {
	Level   string            `json:"level"`
	Message string            `json:"msg"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// JoinRequest asks the backend to dial a relay and join a room.
type JoinRequest struct {
	Relay    string `json:"relay"`
	Room     string `json:"room"`
	Nickname string `json:"nickname"`
}

// TrackConfig describes one local publication well enough for a remote
// subscriber to configure a matching decoder. It is what the backend
// writes into its MSF catalog, and what the frontend sends once its
// encoder has produced a decoder config.
type TrackConfig struct {
	Kind string `json:"kind"` // "video" | "audio"

	Codec string `json:"codec"` // WebCodecs codec string

	// Description is the codec extradata, base64-encoded — WebCodecs
	// VideoDecoderConfig/AudioDecoderConfig `description`. Empty for
	// Annex B H.264.
	Description string `json:"description,omitempty"`

	Width     uint32  `json:"width,omitempty"`
	Height    uint32  `json:"height,omitempty"`
	Framerate float64 `json:"framerate,omitempty"`
	Bitrate   uint64  `json:"bitrate,omitempty"`

	SampleRate uint32 `json:"sampleRate,omitempty"`
	Channels   uint32 `json:"channels,omitempty"`

	// TemporalLayers is how many temporal layers the encoder was configured
	// to emit, and so how many subgroups a group of this track is published
	// on. One or zero both mean a single subgroup, which is what every track
	// was before layers and what an encoder that ignores scalabilityMode
	// produces anyway.
	//
	// A subscriber needs this before the first frame, not after: a group's
	// subgroup streams do not arrive together, and reassembly cannot tell a
	// layer that is gone from one the relay has not drained yet unless it
	// knows how many to look for. See internal/conf/reorder.go.
	TemporalLayers uint8 `json:"temporalLayers,omitempty"`
}

// ClientStats are the frontend's own counters, forwarded so the debug
// panels can show capture and playback health next to transport health.
type ClientStats struct {
	EncodeFPS      float64            `json:"encodeFps"`
	EncodeQueue    int                `json:"encodeQueue"`
	EncodeKbps     float64            `json:"encodeKbps"`
	AudioEncodeFPS float64            `json:"audioEncodeFps"`
	Decoders       map[string]float64 `json:"decoders,omitempty"`
}

// ---- control messages: backend to frontend ----------------------------

// ServerMessage is a JSON control frame to the WebView.
type ServerMessage struct {
	Type string `json:"type"`

	State        *SessionState  `json:"state,omitempty"`
	Participants []Participant  `json:"participants,omitempty"`
	Track        *RemoteTrack   `json:"track,omitempty"`
	TrackGone    *RemoteTrackID `json:"trackGone,omitempty"`
	Log          *LogEntry      `json:"log,omitempty"`
	Metrics      *Metrics       `json:"metrics,omitempty"`
	Invite       *Invite        `json:"invite,omitempty"`
	Update       *Update        `json:"update,omitempty"`
	Error        string         `json:"error,omitempty"`
}

// Update is a released version newer than the one running. Sent at most once
// per run, and only when there is something to offer — the frontend shows a
// button when it arrives and nothing at all when it does not.
type Update struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// Invite is a relay and room the app was asked to join from outside — an
// invite link the OS handed us. The frontend fills its welcome form from it
// and joins.
type Invite struct {
	Relay string `json:"relay"`
	Room  string `json:"room"`
}

// Server message types.
const (
	MsgState        = "state"
	MsgParticipants = "participants"
	MsgRemoteTrack  = "remoteTrack"
	MsgTrackGone    = "trackGone"
	MsgLog          = "log"
	MsgMetrics      = "metrics"
	MsgInvite       = "invite"
	MsgUpdate       = "update"
	MsgError        = "error"
	// MsgRequestKeyFrame asks the frontend's video encoder for an immediate
	// keyframe. A fresh session has no open group, and writeVideo will not
	// start one on a delta frame, so without this a reconnect stays blank
	// until the next scheduled keyframe — up to the keyframe interval.
	MsgRequestKeyFrame = "requestKeyFrame"
)

// Session lifecycle states reported in SessionState.Phase.
const (
	PhaseIdle       = "idle"
	PhaseConnecting = "connecting"
	PhaseJoined     = "joined"
	PhaseFailed     = "failed"
	// PhaseReconnecting means the room was joined and the relay session
	// then ended; the backend is re-dialling. Distinct from connecting so
	// the frontend can keep the call on screen and say what is happening
	// rather than dumping the user back to the welcome form.
	PhaseReconnecting = "reconnecting"
)

// SessionState is the backend's view of the MOQ session.
type SessionState struct {
	Phase string `json:"phase"`
	Relay string `json:"relay,omitempty"`
	Room  string `json:"room,omitempty"`
	// ID is the local participant's generated identifier — the last
	// field of its namespace tuple.
	ID       string `json:"id,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Participant is one remote peer discovered in the room.
type Participant struct {
	ID       string `json:"id"`
	Nickname string `json:"nickname"`
	// Version is the build they are running, or empty from a peer old
	// enough not to publish one.
	Version  string `json:"version,omitempty"`
	HasVideo bool   `json:"hasVideo"`
	HasAudio bool   `json:"hasAudio"`
	// VideoLevel is how much of their video this client is still taking:
	// "full", "small", or "none". Distinct from HasVideo, which says what they
	// publish — a peer we have given up on and a peer with the camera off both
	// show no picture, and only this tells them apart.
	VideoLevel string `json:"videoLevel,omitempty"`
}

// RemoteTrack announces an inbound track and the handle its frames will
// carry. The frontend configures a decoder from Config and then renders
// every binary frame bearing Handle.
type RemoteTrack struct {
	Handle      uint32      `json:"handle"`
	Participant string      `json:"participant"`
	Nickname    string      `json:"nickname"`
	Config      TrackConfig `json:"config"`
}

// RemoteTrackID identifies a track that has ended.
type RemoteTrackID struct {
	Handle      uint32 `json:"handle"`
	Participant string `json:"participant"`
}

// LogEntry is one backend log record, forwarded to the debug log panel.
type LogEntry struct {
	// TimeMillis is a Unix millisecond timestamp.
	TimeMillis int64             `json:"t"`
	Level      string            `json:"level"`
	Message    string            `json:"msg"`
	Attrs      map[string]string `json:"attrs,omitempty"`
}

// Metrics is one sample of transport and per-track health, emitted on a
// fixed interval so the frontend can plot it directly.
type Metrics struct {
	TimeMillis int64 `json:"t"`

	// QUIC transport, from the connection's qlog event stream.
	SmoothedRTTMillis float64 `json:"rttMs"`
	MinRTTMillis      float64 `json:"minRttMs"`
	LatestRTTMillis   float64 `json:"latestRttMs"`
	// PeakRTTMillis is the worst RTT measured during the sample interval —
	// the spike a smoothed average hides.
	PeakRTTMillis    float64 `json:"peakRttMs"`
	CongestionWindow int     `json:"cwnd"`
	BytesInFlight    int     `json:"bytesInFlight"`
	PacketsInFlight  int     `json:"packetsInFlight"`
	CongestionState  string  `json:"congestionState,omitempty"`

	// Rates over the sample interval.
	PacketsSentPerSec float64 `json:"packetsSentPerSec"`
	PacketsLostPerSec float64 `json:"packetsLostPerSec"`
	LossPercent       float64 `json:"lossPercent"`
	SendKbps          float64 `json:"sendKbps"`
	ReceiveKbps       float64 `json:"receiveKbps"`

	// Cumulative counters.
	PacketsSent     uint64 `json:"packetsSent"`
	PacketsReceived uint64 `json:"packetsReceived"`
	PacketsLost     uint64 `json:"packetsLost"`

	// MOQ-level rates, split by direction.
	PublishKbps   float64 `json:"publishKbps"`
	SubscribeKbps float64 `json:"subscribeKbps"`
	// Objects and groups published/received over the interval.
	ObjectsOutPerSec float64 `json:"objectsOutPerSec"`
	ObjectsInPerSec  float64 `json:"objectsInPerSec"`
	GroupsOutPerSec  float64 `json:"groupsOutPerSec"`

	// BridgeDropped is how many frames this bridge has dropped on its way to
	// the WebView, cumulative for the connection. Loss here is not the
	// network's and does not show up anywhere else: the send queue is bounded
	// and a full one discards rather than blocks, so a WebView that cannot
	// drain fast enough loses audio and video with nothing else to say so.
	BridgeDropped uint64 `json:"bridgeDropped"`

	Tracks []TrackMetrics `json:"tracks,omitempty"`
}

// TrackMetrics is per-track throughput, keyed by a label the frontend can
// show verbatim (e.g. "out/video" or "in/ab12cd/audio").
type TrackMetrics struct {
	Label   string  `json:"label"`
	Kbps    float64 `json:"kbps"`
	Objects uint64  `json:"objects"`
	Groups  uint64  `json:"groups"`
	// SkewMillisPerSec is how fast this track's arrivals are falling behind
	// the clock that produced them, in milliseconds per second. Positive means
	// a queue on the inbound path is filling — the path is carrying less than
	// is being sent. Inbound audio only, and nil until there is enough history
	// to fit a trend, which is why it is a pointer: zero means "keeping up",
	// not "no reading".
	SkewMillisPerSec *float64 `json:"skewMillisPerSec,omitempty"`
	// LagMillis is how much later objects are arriving now than when this
	// subscription started — the integral of the above. It says the call has
	// fallen behind, where the slope says it is falling behind.
	LagMillis *float64 `json:"lagMillis,omitempty"`
}

// Endpoint is what the asset handler serves at /__bridge so the frontend
// can find and authenticate to the WebSocket.
type Endpoint struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	// Version is the build the backend is running, which is also the build
	// the frontend is part of — they ship in one binary. Carried here rather
	// than in a control message because the welcome screen wants to show it
	// before any session exists, and this descriptor is already the first
	// thing the frontend fetches.
	Version string `json:"version,omitempty"`
}

func (e Endpoint) JSON() []byte {
	b, _ := json.Marshal(e)
	return b
}
