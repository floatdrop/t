/**
 * Wire types and framing shared with the Go backend. Keep in step with
 * internal/bridge/protocol.go and internal/bridge/frame.go.
 */

export const FRAME_VERSION = 1;
export const FRAME_HEADER_LEN = 24;

export const KIND_VIDEO = 0;
export const KIND_AUDIO = 1;

export const FLAG_KEYFRAME = 1 << 0;
/** Marks the header's audio-level byte as a real measurement. */
export const FLAG_AUDIO_LEVEL = 1 << 1;

/**
 * The frame's temporal layer lives in bits 2-3 of the flags byte, so layers
 * 0-3. It rides in the flags rather than a field of its own because the header
 * is a fixed 24 bytes with a hand-written implementation at each end, and
 * widening it would be a wire break for a two-bit value.
 *
 * Layer 0 is the base — decodable alone, referenced by every layer above it —
 * and is also what an encoder producing no temporal layers reports, so an
 * unlayered stream needs no special case.
 */
export const TEMPORAL_LAYER_SHIFT = 2;
export const TEMPORAL_LAYER_MASK = 0x3 << TEMPORAL_LAYER_SHIFT;

/** Handles for the two tracks this frontend publishes. */
export const HANDLE_LOCAL_VIDEO = 0;
export const HANDLE_LOCAL_AUDIO = 1;
/**
 * The second, smaller encoding of the same camera. Its frames are KIND_VIDEO
 * like any other picture — only the handle says which encoding they belong to,
 * and only the publisher ever distinguishes them. A subscriber takes one layer
 * or the other and is told about it as plain video.
 */
export const HANDLE_LOCAL_VIDEO_LOW = 2;

export interface MediaFrame {
  kind: number;
  handle: number;
  /** Microseconds, straight from the encoder. */
  timestamp: number;
  keyFrame: boolean;
  /** Codec description bytes, when the encoder emitted a new config. */
  config?: Uint8Array;
  payload: Uint8Array;
  /**
   * RFC 6464 byte from LOC's AudioLevel property: bit 7 voice activity,
   * bits 0-6 magnitude in -dBov. Audio frames only.
   */
  audioLevel?: number;
  /**
   * Which temporal layer of an SVC encoding this frame belongs to. Decides the
   * subgroup the publisher writes it to, so the upper layers can be shed
   * without disturbing the base. Absent means the base layer.
   */
  temporalLayer?: number;
}

/** Encodes one media frame into the binary layout the backend parses. */
export function encodeFrame(f: MediaFrame): ArrayBuffer {
  const configLen = f.config ? f.config.byteLength : 0;
  const buf = new ArrayBuffer(FRAME_HEADER_LEN + configLen + f.payload.byteLength);
  const view = new DataView(buf);
  const bytes = new Uint8Array(buf);

  view.setUint8(0, FRAME_VERSION);
  view.setUint8(1, f.kind);
  let flags = f.keyFrame ? FLAG_KEYFRAME : 0;
  if (f.audioLevel !== undefined) {
    flags |= FLAG_AUDIO_LEVEL;
    view.setUint8(3, f.audioLevel & 0xff);
  }
  flags |= ((f.temporalLayer ?? 0) << TEMPORAL_LAYER_SHIFT) & TEMPORAL_LAYER_MASK;
  view.setUint8(2, flags);
  view.setUint32(4, f.handle);
  // Timestamps are microseconds in a u64. Number stays exact to 2^53 µs,
  // which is ~285 years — far beyond any session.
  view.setBigUint64(8, BigInt(Math.max(0, Math.round(f.timestamp))));
  view.setUint32(16, configLen);
  view.setUint32(20, f.payload.byteLength);

  if (f.config) {
    bytes.set(f.config, FRAME_HEADER_LEN);
  }
  bytes.set(f.payload, FRAME_HEADER_LEN + configLen);
  return buf;
}

/** Decodes one binary frame received from the backend. */
export function decodeFrame(buf: ArrayBuffer): MediaFrame | null {
  if (buf.byteLength < FRAME_HEADER_LEN) return null;
  const view = new DataView(buf);
  if (view.getUint8(0) !== FRAME_VERSION) return null;

  const configLen = view.getUint32(16);
  const payloadLen = view.getUint32(20);
  if (FRAME_HEADER_LEN + configLen + payloadLen > buf.byteLength) return null;

  const configEnd = FRAME_HEADER_LEN + configLen;
  const flags = view.getUint8(2);
  return {
    kind: view.getUint8(1),
    handle: view.getUint32(4),
    timestamp: Number(view.getBigUint64(8)),
    keyFrame: (flags & FLAG_KEYFRAME) !== 0,
    config: configLen > 0 ? new Uint8Array(buf, FRAME_HEADER_LEN, configLen) : undefined,
    payload: new Uint8Array(buf, configEnd, payloadLen),
    audioLevel: (flags & FLAG_AUDIO_LEVEL) !== 0 ? view.getUint8(3) : undefined,
    temporalLayer: (flags & TEMPORAL_LAYER_MASK) >> TEMPORAL_LAYER_SHIFT,
  };
}

// ---- control messages -------------------------------------------------

export interface JoinRequest {
  relay: string;
  room: string;
  nickname: string;
}

export interface TrackConfig {
  kind: 'video' | 'audio';
  codec: string;
  /** Codec extradata, base64. Empty for Annex B H.264. */
  description?: string;
  width?: number;
  height?: number;
  framerate?: number;
  bitrate?: number;
  sampleRate?: number;
  channels?: number;
  /**
   * How many temporal layers the encoder was configured to emit, and so how
   * many subgroups a group of this track is published on. One or absent both
   * mean a single subgroup. A subscriber needs it before the first frame —
   * see internal/conf/reorder.go.
   */
  temporalLayers?: number;
}

export interface ClientStats {
  encodeFps: number;
  encodeQueue: number;
  encodeKbps: number;
  audioEncodeFps: number;
  decoders?: Record<string, number>;
}

/**
 * Something the WebView wants in the backend's log — a capture failure, a
 * decoder error. Without this the two halves keep separate logs and the
 * debug panel only shows half the story.
 */
export interface ClientReport {
  level: 'DEBUG' | 'INFO' | 'WARN' | 'ERROR';
  msg: string;
  attrs?: Record<string, string>;
}

export type ClientMessage =
  | { type: 'join'; join: JoinRequest }
  | { type: 'leave' }
  | { type: 'track'; track: TrackConfig }
  | { type: 'stats'; stats: ClientStats }
  | { type: 'logLevel'; logLevel: string }
  | { type: 'untrack'; untrack: 'video' | 'audio' }
  // Handing a link to the OS is the backend's job: navigating this WebView to
  // a web page would replace the app with it, with no way back to the call.
  | { type: 'openUrl'; openUrl: string }
  // Which remote participants' video is worth receiving. Video only: audio is
  // never gated on being visible, since someone speaking off-screen has to be
  // heard, and hearing them is what makes anyone scroll to them.
  | { type: 'interest'; interest: { video: string[] } }
  | { type: 'report'; report: ClientReport };

/**
 * `reconnecting` means the room was joined and the relay session then ended;
 * the backend is re-dialling. Distinct from `connecting` so the call can stay
 * on screen instead of dropping the user back to the welcome form.
 */
export type Phase = 'idle' | 'connecting' | 'joined' | 'failed' | 'reconnecting';

export interface SessionState {
  phase: Phase;
  relay?: string;
  room?: string;
  id?: string;
  nickname?: string;
  detail?: string;
}

export interface Participant {
  id: string;
  nickname: string;
  /** The build they are running, absent from a peer old enough not to say. */
  version?: string;
  hasVideo: boolean;
  hasAudio: boolean;
  /**
   * How much of their video we are still taking: "full", "small" or "none".
   *
   * Distinct from hasVideo, which says what they publish. A peer the backend
   * gave up on and a peer with the camera off both show no picture, and only
   * this tells them apart.
   */
  videoLevel?: string;
}

/** A released version newer than the one running. */
export interface UpdateInfo {
  version: string;
  url: string;
}

export interface RemoteTrack {
  handle: number;
  participant: string;
  nickname: string;
  config: TrackConfig;
}

export interface RemoteTrackID {
  handle: number;
  participant: string;
}

export interface LogEntry {
  /** Unix milliseconds. */
  t: number;
  level: string;
  msg: string;
  attrs?: Record<string, string>;
}

export interface TrackMetrics {
  label: string;
  kbps: number;
  objects: number;
  groups: number;
  /**
   * How fast this track's arrivals are falling behind the clock that produced
   * them, in milliseconds per second. Positive means a queue on the inbound
   * path is filling — more is being sent to us than is getting through.
   *
   * Inbound audio only, and absent until there is enough history to fit a
   * trend. Absent and zero are different answers: zero means keeping up.
   */
  skewMillisPerSec?: number;
  /**
   * How much later objects are arriving now than when this subscription
   * started — the integral of the above. The slope says the call is falling
   * behind; this says it has fallen behind, and is what a resubscribe exists
   * to reset.
   */
  lagMillis?: number;
}

export interface Metrics {
  t: number;

  rttMs: number;
  minRttMs: number;
  latestRttMs: number;
  peakRttMs: number;
  cwnd: number;
  bytesInFlight: number;
  packetsInFlight: number;
  congestionState?: string;

  packetsSentPerSec: number;
  packetsLostPerSec: number;
  lossPercent: number;
  sendKbps: number;
  receiveKbps: number;

  packetsSent: number;
  packetsReceived: number;
  packetsLost: number;

  publishKbps: number;
  subscribeKbps: number;
  objectsOutPerSec: number;
  objectsInPerSec: number;
  groupsOutPerSec: number;

  /** Frames the bridge dropped on the way here, cumulative for the connection. */
  bridgeDropped: number;

  tracks?: TrackMetrics[];
}

/** A relay and room the OS handed us through an invite link. */
export interface InviteMessage {
  relay: string;
  room: string;
}

export type ServerMessage =
  | { type: 'state'; state: SessionState }
  | { type: 'invite'; invite: InviteMessage }
  | { type: 'requestKeyFrame' }
  | { type: 'participants'; participants: Participant[] }
  | { type: 'remoteTrack'; track: RemoteTrack }
  | { type: 'trackGone'; trackGone: RemoteTrackID }
  | { type: 'update'; update: UpdateInfo }
  | { type: 'log'; log: LogEntry }
  | { type: 'metrics'; metrics: Metrics }
  | { type: 'error'; error: string };

/** Endpoint descriptor served by the backend at /__bridge. */
export interface Endpoint {
  url: string;
  token: string;
  /** The build the backend — and so this frontend — was packaged as. */
  version?: string;
}

/**
 * Normalises whatever WebCodecs hands back as a codec description into a
 * Uint8Array. The spec types `description` as AllowSharedBufferSource, so
 * it can be an ArrayBuffer, a SharedArrayBuffer, or any view over either.
 */
export function toBytes(src: AllowSharedBufferSource): Uint8Array {
  if (src instanceof ArrayBuffer) return new Uint8Array(src);
  const view = src as ArrayBufferView;
  return new Uint8Array(view.buffer as ArrayBuffer, view.byteOffset, view.byteLength);
}

/** base64-encodes raw bytes for the JSON control channel. */
export function toBase64(bytes: Uint8Array): string {
  let s = '';
  for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
  return btoa(s);
}

/** Decodes a base64 codec description back to bytes. */
export function fromBase64(b64: string): Uint8Array {
  const s = atob(b64);
  const out = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
  return out;
}
