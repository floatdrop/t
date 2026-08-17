/**
 * Camera and microphone capture, encoded with WebCodecs and pushed to the
 * backend as bridge frames.
 *
 * Two WebKit constraints shape this file. There is no
 * MediaStreamTrackProcessor, so video frames are pulled off a <video>
 * element with requestVideoFrameCallback and audio PCM comes from an
 * AudioWorklet (see worklets.ts). And H.264 is configured in Annex B, so
 * SPS/PPS travel in-band with every keyframe and no out-of-band
 * description has to be carried in the catalog.
 */

import { bridge } from './bridge';
import type { ClientMessage } from './protocol';
import {
  HANDLE_LOCAL_AUDIO,
  HANDLE_LOCAL_VIDEO,
  KIND_AUDIO,
  KIND_VIDEO,
  toBase64,
  toBytes,
} from './protocol';
import { DENOISE_FRAME, Denoiser, type VoiceState } from './denoise';
import { addTapModule, watchAudioContext, type TapBlock } from './worklets';

/**
 * Seconds between forced keyframes — also the video group length.
 *
 * It is what a subscriber waits before its first picture, because a decoder
 * cannot start on a delta frame. That used to be covered by a FETCH replaying
 * the group in progress; the FETCH is gone, so this is the wait, and five
 * seconds is a tile that appears rather than one that is missing.
 *
 * It is also how much a lost group costs, which is the other half of the
 * argument: a group is all-or-nothing to a decoder, so a longer group
 * raises the cost of losing one.
 *
 * The price is keyframes being a smaller share of the stream — at 30 fps and
 * roughly four delta frames to a keyframe, about one per cent of the bitrate
 * moves from deltas to keyframes. That is the cheaper side of this trade for
 * a call, where a blank tile is noticed and a slightly softer picture is not.
 *
 * Five seconds rather than one: the shorter interval was chosen when the FETCH
 * still set the join latency, and the interval was the only thing that did.
 * The grace window in the reassembler now covers the inter-layer skew that
 * was the original reason for the short interval, so the longer group is safe
 * and the bitrate savings and reduced keyframe overhead are worth the longer
 * join wait.
 */
const KEYFRAME_INTERVAL_SEC = 5;

/**
 * How many samples the reported video bitrate is averaged over.
 *
 * One second at the panel's 250 ms cadence. With a five-second GOP the window
 * no longer covers a whole keyframe, but keyframes are one per cent of the
 * bitrate at this interval, so the alternation between the rate with a
 * keyframe in it and the rate without is below the noise floor anyway. Kept
 * to a second so it still follows a real change — an adaptive step shows up
 * within a second of being taken.
 */
const VIDEO_KBPS_WINDOW = 4;

/**
 * The SVC mode the primary video encoding is configured with.
 *
 * One spatial layer, two temporal ones: frames alternate between a base layer
 * that stands on its own and an enhancement layer nothing else references. The
 * backend maps the layer onto the subgroup it publishes the frame in, so the
 * enhancement layer is separately declinable and separately sheddable.
 *
 * The small layer stays flat. It already runs at a divided framerate, so
 * shedding half of what is left is not a degraded picture but a broken one —
 * and a subscriber small enough to be given that layer has already taken the
 * step down this would be offering.
 *
 * Kept as a constant because two things have to agree on how many layers there
 * are — this and the two bits the bridge header spends on the layer id.
 */
const VIDEO_SCALABILITY_MODE = 'L1T2';

/**
 * Which temporal layer an encoded chunk belongs to.
 *
 * WebCodecs reports it as `svc.temporalLayerId` on the chunk's metadata. An
 * encoder that ignored `scalabilityMode` reports nothing, and that is the case
 * this defaults for: every frame then reads as the base layer, the backend puts
 * them all in one subgroup, and the result is exactly the flat single-subgroup
 * stream that existed before layers — degraded, but not broken. Which matters,
 * because whether WebKit honours scalabilityMode for H.264 is not something the
 * API will tell us in advance; it accepts the config either way.
 */
function temporalLayerOf(meta?: EncodedVideoChunkMetadata): number {
  // `svc` is the WebCodecs SVC extension, which the bundled DOM types do not
  // describe. Narrowed here rather than declared globally: a global
  // augmentation would assert the field exists on every platform, and whether
  // this one populates it is exactly what the runtime check below is for.
  const svc = (meta as { svc?: { temporalLayerId?: number } } | undefined)?.svc;
  return typeof svc?.temporalLayerId === 'number' ? svc.temporalLayerId : 0;
}

/**
 * How many chunks to watch before saying whether the encoder is really
 * producing temporal layers.
 *
 * One GOP at the keyframe interval, which is every layer's cadence several
 * times over: L1T2 alternates, so a single missing layer id shows up within two
 * frames and this is well past needing the benefit of the doubt.
 */
const SVC_SAMPLE_CHUNKS = 30;

/**
 * Most frames a second this will publish, whatever the camera offers.
 *
 * A ceiling rather than a target: a camera that can only manage 24 is left at
 * 24. Above 30 the extra frames cost bitrate in proportion and buy nothing a
 * conversation makes use of, and the budget is far better spent on the picture
 * being sharp than on it being smooth.
 */
const MAX_FRAMERATE = 30;




/**
 * How close to a full frame interval two presentations may be and still count
 * as two frames.
 *
 * requestVideoFrameCallback fires per *presentation*, and a WebView is free to
 * present the same camera frame twice — on a 60 Hz display showing a 30 fps
 * camera, every second callback carries a picture already encoded. Duplicates
 * arrive half an interval apart and real frames a whole one, so anything
 * between the two separates them.
 *
 * Three quarters, not nine tenths. Measured: at 0.9 the threshold sat 3 ms
 * under a 33 ms frame interval, ordinary capture jitter pushed real frames the
 * wrong side of it, and each rejection cost a whole frame period — a 30 fps
 * camera published at 18. Three quarters leaves a quarter of an interval of
 * slack either way.
 */
const FRAME_GAP_TOLERANCE = 0.75;

/**
 * Where the video comes from.
 *
 * One video track per participant is all the catalog carries, so a screen is
 * not an addition to the camera but a replacement for it — which makes this a
 * property of the video settings rather than a track of its own.
 */
export type VideoSource = 'camera' | 'screen';

export interface VideoSettings {
  source: VideoSource;
  /** Which camera. Meaningless for a screen, which is not a device. */
  deviceId?: string;
  width: number;
  height: number;
  framerate: number;
  bitrate: number;
  /**
   * Let the link choose the rate, with `bitrate` as its ceiling.
   *
   * WebCodecs defaults `bitrateMode` to `"variable"`, and this app never set it
   * — so the number here was a target the encoder was free to exceed, and did:
   * 50 kbps on a still face against 3500 kbps on movement, for a 1500 kbps
   * setting. The mode is pinned to `"constant"` now, which stops the overshoot
   * and leaves the harder question, which is what the number should be.
   *
   * Nothing answered that either. The encoder was configured once and changed
   * only when a setting did, so a link that could not carry the picture was sent
   * it anyway, and what answered *that* was the relay — shedding the enhancement
   * layer, timing a subgroup out, giving up on the subscription. Right last
   * resorts, wrong first one.
   *
   * So the rate follows the uplink instead, between a floor and this ceiling.
   * See bitrate.ts for how it is chosen; the ceiling is what this source would
   * have sent anyway, so adaptation only ever spends less than the setting, and
   * a link that was carrying it keeps carrying it.
   */
  adaptive: boolean;
}

export interface AudioSettings {
  deviceId?: string;
  bitrate: number;
  /** Run the local RNNoise denoiser on top of the platform's own. */
  denoise: boolean;
}

export const defaultVideoSettings: VideoSettings = {
  source: 'camera',
  width: 1280,
  height: 720,
  framerate: 30,
  bitrate: 1_500_000,
  adaptive: true,
};

export const defaultAudioSettings: AudioSettings = {
  bitrate: 32_000,
  denoise: true,
};

/**
 * What a screen share asks for, independent of the camera's resolution.
 *
 * A screen is not framed like a face. Text has to stay legible, so it gets
 * 1080p whatever the camera is set to, and a rate to carry it: the camera's
 * 720p budget would leave anything that moves on a desktop as mush. Still
 * modest for the content — a desktop is largely static between keyframes, which
 * is exactly what H.264 is cheapest at.
 */
export const screenVideoSettings = {
  width: 1920,
  height: 1080,
  /**
   * Slower than the camera, deliberately.
   *
   * A desktop is not a face: it holds still for seconds at a time and then
   * changes all at once, so frames a second buy far less here than pixels do,
   * and 1080p is the whole point of sharing a screen — text that cannot be read
   * is a share that failed. At half the camera's rate the whole 3 Mbps goes on
   * keeping that text sharp, which is the right trade for a window of it; 15 is
   * enough for a cursor to track and a page to scroll without tearing, and it
   * is the smoothness rather than the legibility that a viewer forgives.
   */
  framerate: 15,
  bitrate: 3_000_000,
} as const;

/** One camera size the picker offers. */
export interface VideoRung {
  width: number;
  height: number;
  /** How the size is written in the picker. */
  label: string;
}

/**
 * The sizes on offer, smallest first.
 *
 * A list to pick from, and nothing more. There used to be an "Auto" setting
 * above it that worked out how big our own tile was on everyone else's screen —
 * from the grid's column count, the window width and the device pixel ratio —
 * and re-encoded to the rung that fit, on the reasoning that sending 1080p into
 * a 400 px tile spends bitrate nobody sees.
 *
 * The reasoning was sound and the behaviour was not. Changing resolution
 * mid-call is not free: it rebuilds the local pipeline and costs every
 * subscriber a decoder reconfigure and a wait for the next keyframe. Auto spent
 * that on things that are not about the picture at all — someone joining,
 * someone leaving, a window being dragged wider — and each one showed up on the
 * far side as a stall. Dragging a window across a call was measured driving a
 * re-encode and a resubscribe, and an audio trim behind it. A setting that
 * degrades the call while adapting to it is worse than one number chosen once,
 * so what is published is now exactly what was asked for.
 */
export const VIDEO_LADDER: readonly VideoRung[] = [
  { width: 640, height: 360, label: '640 × 360' },
  { width: 854, height: 480, label: '854 × 480' },
  { width: 1280, height: 720, label: '1280 × 720' },
  { width: 1920, height: 1080, label: '1920 × 1080' },
];

/** How a rung is written as a resolution setting. */
export function rungValue(rung: VideoRung): string {
  return `${rung.width}x${rung.height}`;
}

/** The size a call starts on, matching defaultVideoSettings. */
export const DEFAULT_RESOLUTION = rungValue(VIDEO_LADDER[2]);

/**
 * Whether a pipeline built for `have` already satisfies `want`.
 *
 * Any difference rebuilds that kind, including a bitrate the encoder could in
 * principle just be reconfigured for. Bitrate is not something a call changes
 * often enough to be worth a second path, and an exact match is the honest
 * test of "nothing to do".
 */
function sameVideoSettings(want: VideoSettings, have: VideoSettings | null): boolean {
  return (
    have !== null &&
    // Compared first and never omitted: camera and screen at the same size and
    // bitrate differ in nothing else, and treating them as equal would leave a
    // request to start sharing doing nothing at all.
    have.source === want.source &&
    have.deviceId === want.deviceId &&
    have.width === want.width &&
    have.height === want.height &&
    have.framerate === want.framerate &&
    have.bitrate === want.bitrate &&
    // Compared even though the ceiling above already differs whenever the pick
    // does: switching off adaptation onto a fixed rate equal to the ceiling
    // changes neither number, and the encoder may be several rungs below it by
    // then. Without this that switch would do nothing at all.
    have.adaptive === want.adaptive
  );
}

function sameAudioSettings(want: AudioSettings, have: AudioSettings | null): boolean {
  return (
    have !== null &&
    have.deviceId === want.deviceId &&
    have.bitrate === want.bitrate &&
    have.denoise === want.denoise
  );
}

/** Live counters for the debug panel. */
export interface CaptureStats {
  videoFps: number;
  /**
   * What came out of the encoder, averaged over VIDEO_KBPS_WINDOW samples.
   *
   * Averaged because a keyframe is several times the size of a delta frame and
   * a group opens on one every second, so a rate taken over a single 250 ms
   * sample reports the GOP structure rather than the bitrate: one window in
   * four holds the keyframe and reads high, the other three read low. Measured
   * swinging between 400 and 1500 kbps, four times a second, on an encoder that
   * had not changed its target once.
   */
  videoKbps: number;
  /**
   * What the encoder is currently asked for, against videoKbps, which is what
   * came out. Reported because an adaptive rate is otherwise invisible: a
   * picture that has quietly stepped down to the floor and one that never
   * needed to look identical in every other number here.
   */
  videoBitrateTarget: number;
  encodeQueue: number;
  audioFps: number;
  audioKbps: number;
  /** Packets waiting inside the audio encoder — see MAX_AUDIO_ENCODE_QUEUE. */
  audioEncodeQueue: number;
  keyFrames: number;
  dropped: number;
  /** What the browser actually applied to the microphone track. */
  echoCancellation: boolean;
  noiseSuppression: boolean;
  autoGainControl: boolean;
  /** True while the local RNNoise model is running. */
  denoiseActive: boolean;
}

/**
 * The H.264 levels this will ask for, lowest first, with the limits §A.3.1
 * sets on each: macroblocks per frame, and macroblocks per second.
 *
 * A level is a ceiling on what a decoder must be prepared for, so naming one
 * too low is naming a stream we do not send. 3.1 stops at 3600 macroblocks —
 * exactly 1280x720, and not one row more — while 1080p needs 8160. Both the
 * top rung of VIDEO_LADDER and every screen share ask for 1080p, so a fixed
 * 3.1 described neither: WebKit is free to refuse the configuration outright,
 * and a decoder that believes the string is entitled to size its buffers for
 * 720p and meet a frame it has no room for.
 *
 * 5.1 is not a size anything here asks for. It is the catch to a source that
 * outran the constraints put on it, so an unusual display cannot end the call
 * by being large.
 */
const H264_LEVELS = [
  { idc: 0x1f, maxFrameMBs: 3600, maxMBsPerSec: 108_000 }, // 3.1 — through 720p30
  { idc: 0x28, maxFrameMBs: 8192, maxMBsPerSec: 245_760 }, // 4.0 — through 1080p30
  { idc: 0x33, maxFrameMBs: 36864, maxMBsPerSec: 983_040 }, // 5.1 — the catch-all
] as const;

/**
 * The codec string for one stream: H.264 baseline at the lowest level that can
 * carry it.
 *
 * Baseline keeps the bitstream to what every decoder handles, and the probe
 * confirmed both encode and decode support with Annex B framing in this
 * WebView. The level is chosen per stream rather than fixed, so the string in
 * the catalog describes the stream a subscriber is actually about to receive —
 * the same reason the frame rate is capped before the encoder is configured
 * rather than at the pump.
 */
function videoCodec(width: number, height: number, framerate: number): string {
  // A macroblock is 16x16, and a partial one still costs a whole macroblock.
  const frameMBs = Math.ceil(width / 16) * Math.ceil(height / 16);
  const level =
    H264_LEVELS.find(
      (l) => frameMBs <= l.maxFrameMBs && frameMBs * framerate <= l.maxMBsPerSec,
    ) ?? H264_LEVELS[H264_LEVELS.length - 1];
  return `avc1.42E0${level.idc.toString(16).toUpperCase().padStart(2, '0')}`;
}

const AUDIO_CODEC = 'opus';

/** Microphone sample rate. Opus is defined at 48 kHz. */
const SAMPLE_RATE = 48000;

/** How much audio one tap block carries, in microseconds. */
const AUDIO_BLOCK_US = (DENOISE_FRAME / SAMPLE_RATE) * 1e6;

/**
 * How much audio goes into one Opus packet, in samples and microseconds.
 *
 * Two denoiser frames, because RNNoise is defined at 480 samples and Opus is
 * cheapest at 20 ms. Feeding the encoder one denoiser frame at a time, as this
 * used to, is a 10 ms packet: WebKit emits one packet per AudioData handed to
 * it, so the call published a hundred packets a second per participant. Each
 * one carries its own Opus header, its own LOC object and its own frame across
 * the bridge, and the measured cost was 66 kbps for a stream configured at 32 —
 * the overhead was larger than the audio. It also halved the group length the
 * backend sizes its audio groups by (see audioGroupObjects in conf.go, which
 * says 20 ms outright).
 */
const OPUS_FRAME_SAMPLES = DENOISE_FRAME * 2;
const OPUS_FRAME_US = (OPUS_FRAME_SAMPLES / SAMPLE_RATE) * 1e6;

/**
 * How long a captured block may wait before it is dropped rather than encoded.
 *
 * Loose enough that ordinary main-thread jitter — a garbage collection, a
 * repaint — costs nothing, tight enough that the delay stays well under what
 * anyone would notice in a conversation.
 */
const MAX_AUDIO_LATE_US = 80_000;

/**
 * How many packets may be waiting inside the audio encoder before a new one is
 * dropped rather than handed to it.
 *
 * The same budget as MAX_AUDIO_LATE_US, applied on the far side of the encoder.
 * That threshold bounds how long a block may wait *before* being encoded, which
 * is only half the queue: an encoder that has fallen behind — a machine that is
 * throttling, contention from several decoders and the denoiser — holds the
 * rest, and holds it in a place nothing was looking at. The consequence is the
 * one this file is built around: encoding a backlog sends the audio anyway and
 * the queue drains at exactly 1x, so every listener stays that far behind for
 * the remainder of the call.
 */
const MAX_AUDIO_ENCODE_QUEUE = Math.max(1, Math.round(MAX_AUDIO_LATE_US / OPUS_FRAME_US));

/**
 * A message the backend must not miss: what this participant publishes, and
 * what it has stopped publishing.
 */
type Declaration = Extract<ClientMessage, { type: 'track' } | { type: 'untrack' }>;

/**
 * Which track a declaration is about, for superseding an earlier one.
 *
 * The two video layers are separate kinds here on purpose: a declaration for
 * the small one must not supersede the primary's, or bringing up the layer
 * would withdraw the picture it is meant to accompany.
 */
type DeclarationKind = 'video' | 'audio';

function declarationKind(msg: Declaration): DeclarationKind {
  return msg.type === 'track' ? msg.track.kind : msg.untrack;
}

/** How long to wait for the capture element to start playing before going on. */
const PLAY_TIMEOUT_MS = 3000;

/**
 * How long to wait for a worklet module to load before treating it as failed.
 *
 * Generous, because it fetches and compiles, and a machine under load doing
 * both while encoding video can be slow. Bounded at all because everything
 * behind it in the serial queue waits on it.
 */
const MODULE_TIMEOUT_MS = 10000;

/**
 * How long the frame pump may go without a callback before it is taken to have
 * stopped rather than to be running slowly.
 *
 * A second is thirty frames at the camera's rate and fifteen at a screen
 * share's, so a source that is merely struggling is never mistaken for a chain
 * that has ended. See #startCaptureWatchdog.
 */
const CAPTURE_STALL_MS = 1000;

/** How often to check that the frame pump is still being called. */
const CAPTURE_WATCHDOG_MS = 500;

/**
 * How many times the pump may be re-armed before the track is withdrawn.
 *
 * Re-arming answers a dropped callback. If several in a row change nothing,
 * the fault is elsewhere and the honest thing is to stop advertising video
 * nobody is receiving.
 */
const MAX_PUMP_RESTARTS = 5;

/**
 * How long to wait before offering the camera again, and the ceiling.
 *
 * Withdrawing video used to be the end of it: a camera that went quiet for six
 * seconds was gone for the rest of the call, however long that was, and the
 * only way back was to leave and rejoin. Which is the failure this codebase
 * keeps writing down — detected, reported, and then never retried.
 *
 * The wait doubles so a device that is genuinely gone is not hammered, and
 * stops at a minute so one that comes back is picked up while anyone still
 * cares.
 */
const VIDEO_RECOVERY_MS = 2000;
const VIDEO_RECOVERY_MAX_MS = 60_000;

/**
 * Resolves when work does, or after ms — whichever comes first, saying which.
 *
 * For the promises the platform hands back from a media element, where "it
 * never settles" is a real outcome and the caller has something better to do
 * than wait for it forever.
 */
async function withTimeout<T>(
  work: Promise<T>,
  ms: number,
  what: string,
  fatal = false,
): Promise<T | null> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const expired = new Promise<null>((resolve, reject) => {
    timer = setTimeout(() => {
      // Two shapes of expiry, and the caller knows which it has. Some work can
      // be carried on without — a video element that has not started playing
      // yet will deliver frames when it does. Some cannot: there is no version
      // of "carry on" that produces audio without the tap module, so that one
      // has to fail rather than leave a pipeline half-built and believed.
      if (fatal) {
        reject(new Error(`capture: ${what} did not settle within ${ms}ms`));
        return;
      }
      bridge.report('WARN', 'timed out waiting on the platform; carrying on', {
        what,
        afterMs: String(ms),
      });
      resolve(null);
    }, ms);
  });
  try {
    return await Promise.race([work, expired]);
  } finally {
    clearTimeout(timer);
  }
}

/**
 * How long the capture clock may be wrong before the epoch is corrected.
 *
 * The estimate is the smallest skew seen across a window, so the window has to
 * be long enough to contain at least one block that was handled without delay
 * — trivially true at 100 blocks a second — and short enough that a stall is
 * corrected before anyone notices the lips.
 */
const SKEW_WINDOW_US = 1_000_000;

/**
 * Most the epoch may be walked back per window.
 *
 * A downward correction means the capture clock is running slightly fast, which
 * is a rate difference and not an event, so it is eased out rather than jumped.
 * Small enough that timestamps cannot un-order: one window carries far more
 * capture time than this.
 */
const SKEW_SLEW_US = 1_000;

export class Capture {
  stream: MediaStream | null = null;

  #video: HTMLVideoElement | null = null;
  #videoEncoder: VideoEncoder | null = null;
  /**
   * What the live encoder was configured with, so setVideoBitrate can hand back
   * the same thing with one number changed. Held rather than rebuilt from the
   * settings because the settings say what was asked for and this says what was
   * granted — the size especially, which the camera gets a vote on.
   */
  #videoConfig: VideoEncoderConfig | null = null;
  /** Latched once a reconfigure has failed, so it is tried once and reported once. */
  #bitrateFixed = false;
  #audioEncoder: AudioEncoder | null = null;
  #audioCtx: AudioContext | null = null;
  #tap: AudioWorkletNode | null = null;

  /**
   * Liveness and settings per kind, rather than one flag for the pair.
   *
   * Split because muting must not disturb the picture: open() and start()
   * compare these against what is asked for and rebuild only the side that
   * changed. Each records the request its pipeline was built from, with the
   * device replaced by the one actually granted — see #acquire for why that
   * distinction matters.
   */
  #videoRunning = false;
  #audioRunning = false;
  /**
   * Declarations the socket would not take, newest per kind, retried until it
   * does.
   *
   * A declaration is not a message to lose. The socket refuses silently when
   * it is not open, and the audio one cannot be rebuilt afterwards: WebCodecs
   * emits a decoder description on the encoder's first output and never again,
   * so the OpusHead that message carries exists once. Dropped, it takes the
   * catalog's audio track with it for the life of that pipeline — nobody hears
   * you, and the flag saying it was sent is already set.
   *
   * The same holds for untrack in the other direction: one that does not
   * arrive leaves the catalog advertising a track nothing is feeding.
   */
  #undelivered: Declaration[] = [];

  /** The live frame pump, so the watchdog can re-arm the chain it drives. */
  #pump: (() => void) | null = null;
  #lastPumpMs = 0;
  #pumpRestarts = 0;
  #lastTapMs = 0;
  #audioRestarts = 0;
  #captureWatchdog: ReturnType<typeof setInterval> | null = null;
  #videoFor: VideoSettings | null = null;
  /** Pending attempt to bring a withdrawn camera back, and how long it waits. */
  #videoRecovery: ReturnType<typeof setTimeout> | null = null;
  #videoRecoveryWait = 0;
  /** Detaches the camera track's own state listeners. */
  #detachTrackWatch: (() => void) | null = null;
  /** Latched so a muted camera is reported once, not twice a second. */
  #reportedMute = false;
  #audioFor: AudioSettings | null = null;

  #frameIndex = 0;
  /** When the last frame was encoded, for holding the rate to the configured one. */
  #lastEncodeMs = 0;
  /** Set by forceKeyFrame; consumed by the next captured frame. */
  #forceKeyFrame = false;
  /** Shared epoch so audio and video timestamps sit on one clock. */
  #epochUs = 0;
  /**
   * Offset from the capture AudioContext's clock to the shared media clock, as
   * maintained by #trackAudioClock.
   *
   * Audio timestamps are the tap's capture time plus this. Two earlier
   * formulations were measured and both were wrong by a large, invisible,
   * constant amount. Counting encoded samples describes only the audio that got
   * through, so the 730 ms the encoder spends configuring while the microphone
   * is already live shifts every later timestamp into the past for good. Taking
   * the offset once and trusting the capture clock fails too, for the reason
   * #trackAudioClock explains. Between them they had the picture leading the
   * sound by two thirds of a second, steadily, on every call — and nothing
   * compared the two timelines, so nothing said so.
   */
  #audioEpochUs = 0;
  #haveAudioEpoch = false;
  /** Smallest skew seen in the window that is being measured. */
  #skewFloorUs = 0;
  #skewWindowEndUs = 0;

  #videoFrames = 0;
  #videoBytes = 0;
  /** The last VIDEO_KBPS_WINDOW samples of encoder output, oldest first. */
  #videoRate: Array<{ bytes: number; seconds: number }> = [];
  #audioFrames = 0;
  #audioBytes = 0;
  #keyFrames = 0;
  #dropped = 0;
  /** Chunks watched, chunks that carried an svc layer id, and the split. */
  #svcSampled = 0;
  #svcReported = 0;
  #svcLayers: Record<number, number> = {};
  #lastSample = 0;
  #stats: CaptureStats = {
    videoFps: 0, videoKbps: 0, videoBitrateTarget: 0, encodeQueue: 0,
    audioFps: 0, audioKbps: 0, audioEncodeQueue: 0, keyFrames: 0, dropped: 0,
    echoCancellation: false, noiseSuppression: false, autoGainControl: false,
    denoiseActive: false,
  };

  /**
   * Runs one pipeline change at a time.
   *
   * Every entry point here decides what to do by comparing the request against
   * what is already running — and every one of them then awaits getUserMedia,
   * an AudioWorklet module, or a <video> starting to play before it records
   * what it built. Two callers overlapping in that window both see nothing
   * running and both build it.
   *
   * Which is not hypothetical: join() starts the pipeline the moment the
   * backend reports the room joined, and the settings that follow the grid are
   * applied by an effect on that same transition. They raced, and the call went
   * out with two microphone taps feeding one encoder — a hundred packets a
   * second each carrying 20 ms of audio, so every listener received two seconds
   * of sound for every second that passed and their buffers sat permanently
   * full. Two seconds of delay, from two lines of code that both looked right.
   *
   * A queue rather than a flag per kind: the flags are what was already being
   * raced, and there is nothing here worth doing concurrently.
   */
  #queue: Promise<unknown> = Promise.resolve();

  #serial<T>(work: () => Promise<T>): Promise<T> {
    // Settled either way, so one failed change does not wedge every change
    // after it.
    const next = this.#queue.then(work, work);
    this.#queue = next.catch(() => {});
    return next;
  }

  /**
   * Opens the requested devices, keeping any already open on the settings
   * asked for. Call before start(). Returns the stream so the caller can show
   * a local preview.
   *
   * The stream keeps its identity across calls — tracks are added to and
   * removed from it in place — so a kind that was left alone is not even
   * momentarily detached from whatever is rendering it. The cost is that a
   * swap has nothing to announce it from the outside, which is why the local
   * tile keys itself on the video track's id instead of the stream's.
   */
  open(video: VideoSettings | null, audio: AudioSettings | null): Promise<MediaStream> {
    return this.#serial(() => this.#open(video, audio));
  }

  async #open(video: VideoSettings | null, audio: AudioSettings | null): Promise<MediaStream> {
    // Checked before anything is released, so a call that asks for nothing
    // leaves what is running alone.
    if (!video && !audio) {
      throw new Error('capture: neither camera nor microphone selected');
    }

    const freshVideo = !!video && !sameVideoSettings(video, this.#videoFor);
    const freshAudio = !!audio && !sameAudioSettings(audio, this.#audioFor);

    // Release only what is going away or being replaced.
    if (!video || freshVideo) this.#stopVideo();
    if (!audio || freshAudio) this.#stopAudio();

    if (freshVideo || freshAudio) {
      await this.#acquire(freshVideo ? video : null, freshAudio ? audio : null);
    }
    return this.stream!;
  }

  /**
   * Acquires the named kinds and merges them into the live stream.
   *
   * One getUserMedia call covers both when both are wanted, which is the
   * ordinary first open and keeps it to a single permission check. A later
   * single-kind switch asks for that kind alone, leaving the other track — and
   * the device light that goes with it — untouched.
   */
  async #acquire(video: VideoSettings | null, audio: AudioSettings | null): Promise<void> {
    const fresh: MediaStreamTrack[] = [];

    // A screen cannot be asked for in the same breath as a microphone —
    // getDisplayMedia takes only display constraints — so a screen share is two
    // calls. It goes first: getDisplayMedia needs the transient activation from
    // the click that asked for it, and spending it on a microphone prompt first
    // is how that gets lost.
    if (video?.source === 'screen') {
      const display = await navigator.mediaDevices.getDisplayMedia({
        video: {
          // Maxima, not ideals: a desktop larger than this should come down to
          // it, so the rate in screenVideoSettings is the rate it is carrying,
          // but a smaller one has no business being stretched up to meet a
          // target. Measured honouring these — a 2560-wide screen asked for
          // 1080p arrives as 1920×1080.
          width: { max: video.width },
          height: { max: video.height },
          frameRate: { max: video.framerate },
        },
      });
      fresh.push(...display.getTracks());
    }

    const constraints: MediaStreamConstraints = {};
    if (video && video.source === 'camera') {
      constraints.video = {
        width: { ideal: video.width },
        height: { ideal: video.height },
        frameRate: { ideal: video.framerate },
        ...(video.deviceId ? { deviceId: { exact: video.deviceId } } : {}),
      };
    }
    if (audio) {
      constraints.audio = {
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
        ...(audio.deviceId ? { deviceId: { exact: audio.deviceId } } : {}),
      };
    }
    if (constraints.video || constraints.audio) {
      fresh.push(...(await navigator.mediaDevices.getUserMedia(constraints)).getTracks());
    }

    if (!this.stream) this.stream = new MediaStream();
    fresh.forEach((track) => this.stream!.addTrack(track));
    bridge.report('INFO', 'capture devices opened', {
      tracks: fresh.map((t) => `${t.kind}:${t.label}`).join(', '),
      ...(video?.source === 'screen' ? { videoSource: 'screen' } : {}),
    });

    // What each kind is running on, for the next comparison to work against.
    // See #rememberVideo for why the video side is not simply what was asked.
    const cam = fresh.find((t) => t.kind === 'video');
    if (video && cam) this.#rememberVideo(video, cam);

    const mic = fresh.find((t) => t.kind === 'audio');
    if (audio && mic) {
      this.#audioFor = { ...audio, deviceId: mic.getSettings().deviceId ?? audio.deviceId };

      // Report the processing the browser actually applied, not what we asked
      // for. Echo cancellation in particular can only be done by the platform,
      // so knowing whether it engaged is the difference between a usable call
      // and an unexplained howl.
      const applied = mic.getSettings();
      this.#stats.echoCancellation = applied.echoCancellation ?? false;
      this.#stats.noiseSuppression = applied.noiseSuppression ?? false;
      this.#stats.autoGainControl = applied.autoGainControl ?? false;
      bridge.report('INFO', 'microphone processing applied by the platform', {
        echoCancellation: String(this.#stats.echoCancellation),
        noiseSuppression: String(this.#stats.noiseSuppression),
        autoGainControl: String(this.#stats.autoGainControl),
      });
    }
  }

  /**
   * Starts encoding and publishing whatever open() acquired, for the kinds not
   * already running. A kind that is running is left as it is — that is what
   * keeps a mute from costing the picture anything.
   */
  start(video: VideoSettings | null, audio: AudioSettings | null): Promise<void> {
    return this.#serial(() => this.#start(video, audio));
  }

  async #start(video: VideoSettings | null, audio: AudioSettings | null): Promise<void> {
    if (!this.stream) throw new Error('capture: open() must run before start()');
    // Set once per session, not per start: a device switch rebuilds a
    // pipeline, and re-basing the clock would send timestamps backwards
    // mid-stream for every subscriber already decoding us.
    if (this.#epochUs === 0) {
      this.#epochUs = performance.now() * 1000;
    }

    if (video && !this.#videoRunning && this.stream.getVideoTracks().length > 0) {
      await this.#startVideo(video);
    }
    if (audio && !this.#audioRunning && this.stream.getAudioTracks().length > 0) {
      await this.#startAudio(audio);
    }
  }

  async #startVideo(settings: VideoSettings): Promise<void> {
    const track = this.stream!.getVideoTracks()[0];

    // A screen share can be ended from outside the app — macOS puts its own
    // stop control in the menu bar, and the window being shared can simply be
    // closed. Nothing else would notice: the track goes silent, the pump stops
    // getting frames, and the catalog would keep advertising video while every
    // subscriber sat on the last thing it saw. stop() does not fire this, so
    // the only way here is a genuine end from the source.
    if (settings.source === 'screen') {
      track.addEventListener('ended', () => {
        bridge.report('INFO', 'screen share ended from outside the app');
        this.onVideoSourceLost?.();
      });
    }

    const actual = track.getSettings();
    const grantedWidth = actual.width ?? settings.width;
    const grantedHeight = actual.height ?? settings.height;
    // Capped here rather than at the pump, so that the encoder's rate control,
    // the keyframe cadence and what the catalog advertises all describe the
    // same stream as the one actually being sent.
    //
    // What was asked for is a ceiling as much as the global one is: a screen
    // asks for 15 and a source that hands back 30 anyway should not be
    // published at 30 — the rate was chosen to pay for the resolution.
    const framerate = Math.min(
      actual.frameRate ?? settings.framerate,
      settings.framerate,
      MAX_FRAMERATE,
    );

    // H.264 is 4:2:0, so a chroma sample covers a 2x2 block of luma and both
    // dimensions have to be even. A camera offers standard sizes that already
    // are; a *window* is whatever size it happens to be, and sharing one of
    // 1279x859 — the size of this app's own window — had the encoder refuse the
    // configuration outright ("H264 only supports even sized frames") and close
    // before a single frame went through.
    const width = grantedWidth - (grantedWidth % 2);
    const height = grantedHeight - (grantedHeight % 2);
    // Rounding down means the frames no longer match what the encoder was
    // configured for, so they get cropped by the odd row and column. Only when
    // something was actually rounded: the camera path is left exactly as it was.
    const crop = width !== grantedWidth || height !== grantedHeight;

    // Chosen from the size and rate settled on just above, so the level names
    // the stream being sent rather than the one that was asked for.
    const codec = videoCodec(width, height, framerate);

    // A <video> element is the only source WebKit offers for constructing
    // VideoFrames from a live track.
    const el = document.createElement('video');
    el.srcObject = new MediaStream([track]);
    el.muted = true;
    el.playsInline = true;
    // Not awaited without a bound. play() on a MediaStream element is a
    // promise the platform is under no obligation to settle promptly, and the
    // whole of start() is behind it: a join awaits start, so a play that never
    // settles is a call that reaches "joined", shows a conference, publishes
    // nothing and says nothing — devices open, catalog empty, no error
    // anywhere. Seen exactly once and not reproduced since, which is reason to
    // bound it rather than to wait for a better explanation.
    //
    // Proceeding early is safe: the pump is driven by requestVideoFrameCallback
    // on this element, so if it starts playing later the frames simply begin
    // then, and if it never does the watchdog below has something to report.
    await withTimeout(el.play(), PLAY_TIMEOUT_MS, 'video element play()');
    this.#video = el;

    // The whole selection goes to the one encoding. There used to be a second,
    // smaller one taking a share of it; a subscriber that cannot carry the
    // picture is sent it with the enhancement layer shed instead, which costs
    // the publisher nothing and needs no second encoder.
    const primaryBitrate = settings.bitrate;

    const encoder = new VideoEncoder({
      output: (chunk, meta) => this.#onVideoChunk(chunk, meta),
      // Carries what it was configured for: a failure here closes the encoder
      // for good, and the configuration is the first thing worth suspecting —
      // isConfigSupported is willing to approve sizes the platform then refuses
      // to encode.
      error: (err) =>
        this.#failVideo(encoder, 'video encoder failed', {
          err: String(err),
          source: settings.source,
          size: `${width}x${height}`,
          framerate: String(Math.round(framerate)),
          bitrate: String(settings.bitrate),
        }),
    });
    const videoConfig: VideoEncoderConfig = {
      codec,
      width,
      height,
      bitrate: primaryBitrate,
      framerate,
      // Said explicitly, because the default is not what a call wants: leaving
      // it out means "variable", and the rate then becomes a target the encoder
      // may exceed several times over on movement — which is the opposite of
      // what an adaptive rate is for, since it would overshoot the very number
      // the controller just chose. See VideoSettings.
      bitrateMode: 'constant',
      latencyMode: 'realtime',
      // Two temporal layers, so the picture has a step down that costs neither
      // a keyframe nor a re-subscribe. Nothing references the top layer, so
      // dropping it costs exactly the frames it carried — half of them — where
      // dropping frames from a flat encoding corrupts everything up to the next
      // keyframe. The backend puts each layer in its own subgroup, which is
      // what lets a subscriber decline one or a relay shed it.
      //
      // L1T2 rather than L1T3: shedding takes 30 fps to 15, which reads as a
      // slightly less fluid picture. L1T3's bottom rung is 7.5 fps, which reads
      // as broken, and the middle rung is a second decision to get right for a
      // saving the first rung already mostly banked.
      scalabilityMode: VIDEO_SCALABILITY_MODE,
      // Annex B puts SPS/PPS in the bitstream ahead of every keyframe, so
      // a subscriber can start decoding from any group without an
      // out-of-band description.
      avc: { format: 'annexb' },
    };
    encoder.configure(videoConfig);
    this.#videoEncoder = encoder;
    this.#videoConfig = videoConfig;
    this.#bitrateFixed = false;
    // A fresh encoder is a fresh answer: scalabilityMode is configured here,
    // and whether this one honours it is not something the last one settled.
    this.#svcSampled = 0;
    this.#svcReported = 0;
    this.#svcLayers = {};

    // Reported after the call, not before it: this line used to be written
    // first and so claimed a configuration that had not happened yet, which is
    // exactly the wrong thing to find in a log when configuring is what failed.
    bridge.report('INFO', 'video encoder configured', {
      codec,
      source: settings.source,
      size: `${width}x${height}`,
      asked: `${settings.width}x${settings.height}`,
      granted: `${grantedWidth}x${grantedHeight}`,
      framerate: String(Math.round(framerate)),
      bitrate: String(primaryBitrate),
      adaptive: String(settings.adaptive),
      selected: String(settings.bitrate),
      state: encoder.state,
    });

    // Declare the track now rather than on first output: the backend needs
    // it in its catalog before remote participants can subscribe, and
    // waiting for a chunk would delay that by a frame interval.
    this.#declare({
      type: 'track',
      track: {
        kind: 'video',
        codec,
        width,
        height,
        framerate,
        bitrate: primaryBitrate,
      },
    });


    const keyEvery = Math.max(1, Math.round(framerate * KEYFRAME_INTERVAL_SEC));

    /** Least time between encoded frames — see FRAME_GAP_TOLERANCE. */
    const minFrameGapMs = (1000 / framerate) * FRAME_GAP_TOLERANCE;

    const pump = () => {
      if (!this.#videoRunning || !this.#videoEncoder || !this.#video) return;
      // Proof the chain is alive, for the watchdog below. Recorded on every
      // callback rather than only on an encode, so a camera that has gone
      // quiet is not mistaken for a chain that has ended.
      this.#lastPumpMs = performance.now();
      this.#pumpRestarts = 0;
      this.#flushUndelivered();

      // An encoder that is no longer configured has been closed by its own
      // error callback, and closing is terminal — it cannot be reconfigured, so
      // every frame from here would throw exactly the same way. Encoding into
      // it anyway turned one failure into a warning per frame, thirty times a
      // second, burying the line that said what had actually gone wrong.
      if (this.#videoEncoder.state !== 'configured') {
        this.#failVideo(this.#videoEncoder, 'video encoder is not configured; stopping capture', {
          state: this.#videoEncoder.state,
          source: settings.source,
          size: `${width}x${height}`,
        });
        return;
      }

      // A frame the camera has not produced yet. Not counted as dropped —
      // nothing was lost, the display simply painted the same picture again.
      const nowMs = performance.now();
      if (nowMs - this.#lastEncodeMs < minFrameGapMs) {
        this.#video.requestVideoFrameCallback(pump);
        return;
      }

      // Dropping frames while the encoder is backed up keeps latency
      // bounded; queueing them would only push the backlog further out.
      if (this.#videoEncoder.encodeQueueSize > 2) {
        this.#dropped++;
      } else {
        this.#lastEncodeMs = nowMs;
        let frame: VideoFrame | null = null;
        try {
          frame = new VideoFrame(this.#video, {
            timestamp: Math.round(performance.now() * 1000 - this.#epochUs),
            ...(crop ? { visibleRect: { x: 0, y: 0, width, height } } : {}),
          });
          const forced = this.#forceKeyFrame;
          this.#forceKeyFrame = false;
          if (forced) {
            // Restart the cadence from here, so a forced keyframe does not
            // leave the next scheduled one moments behind it.
            this.#frameIndex = 0;
          }
          const key = forced || this.#frameIndex % keyEvery === 0;
          this.#videoEncoder.encode(frame, { keyFrame: key });
          // The same frame, the same keyframe decision. Aligned deliberately:
          // a subscriber that switches layers has to land on a keyframe in the
          // one it moves to, and there is no way to ask for one — so the only
          // guarantee available is that both layers always have one at the
          // same moment.
          this.#frameIndex++;
        } catch (err) {
          bridge.report('WARN', 'video frame capture failed', { err: String(err) });
        } finally {
          frame?.close();
        }
      }
      this.#video.requestVideoFrameCallback(pump);
    };

    this.#rememberVideo(settings);
    const camera = this.#videoTrack();
    if (camera) this.#watchTrack(camera);
    this.#videoRunning = true;
    this.#pump = pump;
    this.#lastPumpMs = performance.now();
    this.#pumpRestarts = 0;
    el.requestVideoFrameCallback(pump);
    this.#startCaptureWatchdog();
  }

  /**
   * Restarts the capture pump if it stops being called.
   *
   * The same shape as the presentation loop in playback.ts, and the same
   * failure: requestVideoFrameCallback re-arms only from inside itself, so one
   * undelivered callback ends the chain with nothing left to notice. Every
   * other signal still reads healthy — the encoder is configured, the track is
   * declared, encodeQueueSize is zero so no drop is counted, and #failVideo is
   * never reached.
   *
   * It is worse here than in playback, because this is the half nobody can
   * see. The local tile is drawn from the capture stream rather than the
   * encode path, so it keeps moving; the person whose picture has stopped is
   * the only one in the call still watching themselves move. Everyone else
   * sits on a last frame that looks exactly like a peer still connecting.
   *
   * A timer, deliberately, because it is a different clock and survives what
   * stopped the thing it is watching. Re-arming is cheap and harmless if the
   * chain was merely slow: the pump's own gap check discards a frame that
   * arrives too soon. Only while the page is visible — a hidden page is
   * supposed to stop presenting frames, and re-arming into one is a request
   * per tick that buys nothing.
   */
  #startCaptureWatchdog(): void {
    if (this.#captureWatchdog !== null) return;
    this.#captureWatchdog = setInterval(() => {
      this.#checkTap();
      if (!this.#videoRunning || !this.#video || !this.#pump) return;
      if (document.visibilityState !== 'visible') return;
      const sinceMs = performance.now() - this.#lastPumpMs;
      if (sinceMs < CAPTURE_STALL_MS) return;

      // Which fault is this? Re-arming answers exactly one of the three, and
      // counting attempts without asking spent the whole budget on the two it
      // cannot touch — then withdrew the camera for good.
      const track = this.#videoTrack();
      if (track && track.readyState === 'ended') {
        // The device is gone. No number of re-arms reaches a track that has
        // ended; only opening it again does.
        this.#failVideo(this.#videoEncoder, 'the camera was disconnected', {
          stalledMs: String(Math.round(sinceMs)),
        });
        return;
      }
      if (track?.muted) {
        // The source is temporarily dry — another application has the camera,
        // or the system suspended it. There are no frames to be had and no
        // callback was dropped, so this must not spend a restart: it ends when
        // the track unmutes, which it says itself.
        if (!this.#reportedMute) {
          this.#reportedMute = true;
          bridge.report('WARN', 'the camera has gone quiet; waiting for it to come back', {
            stalledMs: String(Math.round(sinceMs)),
          });
        }
        return;
      }

      this.#pumpRestarts++;
      if (this.#pumpRestarts > MAX_PUMP_RESTARTS && this.#videoEncoder) {
        // Re-arming has not taken, repeatedly. Whatever is wrong is not a
        // dropped callback, and a track that advertises video nobody is
        // sending is worse than one that admits it stopped.
        this.#failVideo(this.#videoEncoder, 'capture pump will not restart; withdrawing video', {
          restarts: String(this.#pumpRestarts),
          stalledMs: String(Math.round(sinceMs)),
        });
        return;
      }

      bridge.report('WARN', 'capture pump stalled; restarting it', {
        stalledMs: String(Math.round(sinceMs)),
        attempt: String(this.#pumpRestarts),
      });
      // Played again as well as re-armed: a paused element delivers no frame
      // callbacks at all, and play() on one already playing is a no-op.
      void this.#video.play().catch(() => {});
      this.#lastPumpMs = performance.now();
      this.#video.requestVideoFrameCallback(this.#pump);
    }, CAPTURE_WATCHDOG_MS);
  }

  /**
   * Rebuilds the audio pipeline if the tap stops delivering blocks.
   *
   * The microphone side has the same shape as the frame pump and needs the
   * same suspicion. A worklet posting to a port can go quiet — its context
   * interrupted, the processor throwing, the node disconnected by something
   * outside this code — and nothing downstream would know: audioRunning stays
   * true, the encoder stays configured, the catalog still declares audio, and
   * no drop is counted because nothing arrived to drop. The only visible trace
   * is audioFps reaching zero in a panel nobody has open.
   *
   * Unlike the pump there is nothing to re-arm — a port is not a callback that
   * can be requested again — so recovery means building the pipeline afresh,
   * through the same queue every other pipeline change goes through. The
   * settings are the ones it was already running.
   */
  #checkTap(): void {
    if (!this.#audioRunning || !this.#audioFor) return;
    if (document.visibilityState !== 'visible') return;
    if (performance.now() - this.#lastTapMs < CAPTURE_STALL_MS) return;

    const settings = this.#audioFor;
    this.#audioRestarts++;
    if (this.#audioRestarts > MAX_PUMP_RESTARTS && this.#audioEncoder) {
      this.#failAudio(this.#audioEncoder, 'capture tap will not restart; withdrawing audio', {
        restarts: String(this.#audioRestarts),
      });
      return;
    }

    bridge.report('WARN', 'capture tap went quiet; rebuilding the audio pipeline', {
      quietMs: String(Math.round(performance.now() - this.#lastTapMs)),
      attempt: String(this.#audioRestarts),
    });
    // Pushed out to the next tick of this watchdog rather than awaited: the
    // interval must not be held open by the rebuild it asked for.
    this.#lastTapMs = performance.now();
    void this.#serial(async () => {
      if (!this.#audioRunning) return;
      this.#stopAudio();
      await this.#open(null, settings);
      await this.#start(null, settings);
      this.#lastTapMs = performance.now();
    }).catch((err: unknown) => {
      bridge.report('ERROR', 'could not rebuild the audio pipeline', { err: String(err) });
    });
  }

  /**
   * Offers the camera again after it has been withdrawn, and keeps offering.
   *
   * Rebuilt the same way the audio pipeline is: the device is opened again as
   * well as restarted, so this covers a track that ended and not only one that
   * stalled. Each failure lengthens the wait, so a camera that is genuinely
   * gone costs one attempt a minute rather than a spin.
   */
  #scheduleVideoRecovery(): void {
    if (this.#videoRecovery !== null || !this.#videoFor) return;
    this.#videoRecoveryWait = this.#videoRecoveryWait
      ? Math.min(this.#videoRecoveryWait * 2, VIDEO_RECOVERY_MAX_MS)
      : VIDEO_RECOVERY_MS;
    const wait = this.#videoRecoveryWait;
    this.#videoRecovery = setTimeout(() => {
      this.#videoRecovery = null;
      this.#recoverVideoNow();
    }, wait);
  }

  /** Tries once, now, and schedules the next attempt if it did not take. */
  #recoverVideoNow(): void {
    const settings = this.#videoFor;
    if (!settings || this.#videoRunning) return;

    if (this.#videoRecovery !== null) {
      clearTimeout(this.#videoRecovery);
      this.#videoRecovery = null;
    }
    bridge.report('INFO', 'trying the camera again', {
      afterMs: String(this.#videoRecoveryWait),
    });

    void this.#serial(async () => {
      if (this.#videoRunning) return;
      this.#stopVideo();
      await this.#open(settings, null);
      await this.#start(settings, null);
    })
      .then(() => {
        if (this.#videoRunning) {
          this.#videoRecoveryWait = 0;
          this.#reportedMute = false;
          bridge.report('INFO', 'the camera is publishing again');
          this.onRecovered?.();
          return;
        }
        this.#scheduleVideoRecovery();
      })
      .catch((err: unknown) => {
        bridge.report('WARN', 'the camera did not come back', { err: String(err) });
        this.#scheduleVideoRecovery();
      });
  }

  /** The camera track behind the capture element, if there is one. */
  #videoTrack(): MediaStreamTrack | null {
    return this.stream?.getVideoTracks()[0] ?? null;
  }

  /**
   * Watches the camera track's own account of itself.
   *
   * Polling can see that frames stopped; only the track can say why, and it
   * says so exactly: mute when the source has nothing to give, unmute when it
   * does again, ended when the device is gone. Before this, a camera that came
   * back was indistinguishable from one that never left — nothing was
   * listening, so nothing acted on it.
   */
  #watchTrack(track: MediaStreamTrack): void {
    this.#detachTrackWatch?.();

    const onMute = () => {
      bridge.report('WARN', 'the camera stopped giving frames', { device: track.label });
    };
    const onUnmute = () => {
      bridge.report('INFO', 'the camera is giving frames again', { device: track.label });
      this.#reportedMute = false;
      this.#pumpRestarts = 0;
      if (this.#videoRunning && this.#video && this.#pump) {
        // Nothing was rebuilt, so the pump only has to be pointed at the
        // element again — it stopped being called when the frames stopped.
        this.#lastPumpMs = performance.now();
        this.#video.requestVideoFrameCallback(this.#pump);
        return;
      }
      // It was withdrawn while the camera was away, so bring it back now
      // rather than waiting out a backoff that was sized for a device that
      // might never return.
      this.#recoverVideoNow();
    };
    const onEnded = () => {
      bridge.report('WARN', 'the camera was disconnected', { device: track.label });
      this.#failVideo(this.#videoEncoder, 'the camera was disconnected', {
        device: track.label,
      });
    };

    track.addEventListener('mute', onMute);
    track.addEventListener('unmute', onUnmute);
    track.addEventListener('ended', onEnded);
    this.#detachTrackWatch = () => {
      track.removeEventListener('mute', onMute);
      track.removeEventListener('unmute', onUnmute);
      track.removeEventListener('ended', onEnded);
      this.#detachTrackWatch = null;
    };
  }

  #stopCaptureWatchdogIfIdle(): void {
    this.#pump = this.#videoRunning ? this.#pump : null;
    if (this.#captureWatchdog === null) return;
    if (this.#videoRunning || this.#audioRunning) return;
    clearInterval(this.#captureWatchdog);
    this.#captureWatchdog = null;
  }

  /**
   * Gives up on the video pipeline once its encoder has failed.
   *
   * A closed encoder is terminal, but the catalog outlives it, and that is the
   * part that matters to everyone else: a declared video track with nothing
   * behind it leaves every subscriber holding a decoder and waiting on a frame
   * that is never coming, on a tile indistinguishable from a peer still
   * connecting. The declaration is sent as soon as the encoder is configured —
   * deliberately, so a subscriber can be ready before the first frame — which
   * means every failure after that point has something to withdraw.
   *
   * Withdrawing a kind that was never declared is a no-op on the backend, so
   * this needs no memory of what has already been sent.
   *
   * Guarded on the encoder still being the current one. A failure reported by
   * an encoder that has since been replaced belongs to a pipeline that is
   * already gone, and untracking on its behalf would withdraw the live one.
   */
  #failVideo(
    // Nullable, because the faults the watchdog now names — a track that
    // ended, a device disconnected — are about the camera rather than the
    // encoder, and can be true when there is no encoder left to blame. The
    // identity check still holds: null matches null, which is exactly "this is
    // still the pipeline that failed".
    encoder: VideoEncoder | null,
    reason: string,
    attrs: Record<string, string>,
  ): void {
    if (this.#videoEncoder !== encoder) return;
    this.#videoRunning = false;
    bridge.report('ERROR', reason, attrs);
    this.#declare({ type: 'untrack', untrack: 'video' });
    // Withdrawing says what is true now, not what will be true for the rest of
    // the call. A camera taken by another application, suspended by the system
    // or unplugged and plugged back in should cost the seconds it was away.
    this.#scheduleVideoRecovery();
    this.onFailure?.(
      'Your camera stopped publishing. Others cannot see you — trying to bring it back.',
    );
  }

  /**
   * Gives up on the audio pipeline once its encoder has failed.
   *
   * The twin of #failVideo, and it was missing while that existed: an encoder
   * error logged a line and changed nothing, so emitAudioFrame's state check
   * turned every block away for the rest of the call. The catalog kept
   * declaring audio, so every listener held a decoder that would never be fed
   * again, and nobody was told.
   *
   * It hides better than the video case. The early return happens before the
   * denoiser runs, so the local speaking ring freezes wherever it last was —
   * and if that was silent, which it usually is, the speaker's own screen
   * looks exactly like a working microphone nobody is talking into.
   */
  #failAudio(encoder: AudioEncoder, reason: string, attrs: Record<string, string>): void {
    if (this.#audioEncoder !== encoder) return;
    this.#audioRunning = false;
    bridge.report('ERROR', reason, attrs);
    this.#declare({ type: 'untrack', untrack: 'audio' });
    // Cleared so the ring does not stay latched on a participant who has
    // stopped publishing anything to be speaking with.
    if (this.#voice.speaking) {
      this.#voice = { speaking: false, level: 0, rfc6464: 127 };
      this.onVoice?.(this.#voice);
    }
    this.onFailure?.('Your microphone stopped publishing — the encoder failed. Others cannot hear you.');
  }

  /**
   * Sends a message the backend must not miss, keeping it if the socket will
   * not take it now.
   *
   * Keyed by kind and type so a retry queue cannot grow: a later declaration
   * for the same track supersedes an earlier one, and an untrack supersedes
   * the declaration it withdraws.
   */
  #declare(msg: Declaration): void {
    // At most one held message per kind: a later declaration supersedes an
    // earlier one, and an untrack supersedes the declaration it withdraws, so
    // the queue cannot grow however long the socket stays shut.
    this.#undelivered = this.#undelivered.filter((m) => declarationKind(m) !== declarationKind(msg));
    if (!bridge.send(msg)) {
      this.#undelivered.push(msg);
    }
  }

  /**
   * Retries whatever the socket would not take. Called from the frame and
   * audio paths, which run often and cost nothing when there is nothing held.
   */
  #flushUndelivered(): void {
    if (this.#undelivered.length === 0) return;
    const held = this.#undelivered.filter((msg) => !bridge.send(msg));
    if (held.length !== this.#undelivered.length) {
      bridge.report('INFO', 'delivered a declaration the socket had refused', {
        remaining: String(held.length),
      });
    }
    this.#undelivered = held;
  }







  #onVideoChunk(chunk: EncodedVideoChunk, meta?: EncodedVideoChunkMetadata): void {
    const payload = new Uint8Array(chunk.byteLength);
    chunk.copyTo(payload);

    // Annex B normally yields no description; carry one if WebKit ever
    // does emit it, so a subscriber is never left without a config.
    let config: Uint8Array | undefined;
    const description = meta?.decoderConfig?.description;
    if (description) {
      config = toBytes(description);
    }

    const isKey = chunk.type === 'key';
    if (isKey) this.#keyFrames++;
    this.#videoFrames++;
    this.#videoBytes += payload.byteLength;

    const layer = temporalLayerOf(meta);
    this.#sampleSVC(meta, layer);

    if (!bridge.sendFrame({
      kind: KIND_VIDEO,
      handle: HANDLE_LOCAL_VIDEO,
      timestamp: chunk.timestamp,
      keyFrame: isKey,
      config,
      payload,
      temporalLayer: layer,
    })) {
      this.#dropped++;
    }
  }

  /**
   * Says once per encoder whether `scalabilityMode` was actually honoured.
   *
   * WebCodecs accepts the configuration either way and reports nothing back, so
   * an encoder that ignores it is indistinguishable from one that obeys it
   * until you look at what comes out. The difference matters well beyond the
   * frame rate: the backend puts each layer in its own subgroup, and the
   * enhancement subgroup is what a relay sheds under pressure and what carries
   * the §8 disposable timeout. A flat stream publishes one subgroup, so there
   * is nothing to shed and a link that cannot keep up loses the picture rather
   * than half the frame rate — silently, and looking exactly like a working
   * call until the link gets tight.
   *
   * Reported at WARN when it is flat, because that is a real degradation of
   * what the call can survive, and at INFO with the split when it is not.
   */
  #sampleSVC(meta: EncodedVideoChunkMetadata | undefined, layer: number): void {
    if (this.#svcSampled >= SVC_SAMPLE_CHUNKS) return;
    const svc = (meta as { svc?: { temporalLayerId?: number } } | undefined)?.svc;
    if (typeof svc?.temporalLayerId === 'number') this.#svcReported++;
    this.#svcLayers[layer] = (this.#svcLayers[layer] ?? 0) + 1;

    if (++this.#svcSampled < SVC_SAMPLE_CHUNKS) return;

    const layers = Object.keys(this.#svcLayers).length;
    const split = Object.entries(this.#svcLayers)
      .map(([id, n]) => `${id}:${n}`)
      .join(' ');
    if (layers > 1) {
      bridge.report('INFO', 'temporal layers are being produced', {
        mode: VIDEO_SCALABILITY_MODE,
        layers: String(layers),
        chunksPerLayer: split,
      });
      return;
    }
    bridge.report('WARN', 'the encoder ignored scalabilityMode; publishing flat video', {
      mode: VIDEO_SCALABILITY_MODE,
      // Told apart because they are different faults: no svc field at all is an
      // encoder without the extension, while a field that only ever says 0 is
      // one that has it and is not layering.
      reportedLayerId: this.#svcReported > 0 ? 'yes' : 'no',
      sampled: String(this.#svcSampled),
      consequence: 'a relay has no enhancement layer to shed under pressure',
    });
  }

  async #startAudio(settings: AudioSettings): Promise<void> {
    const track = this.stream!.getAudioTracks()[0];
    const ctx = new AudioContext({ sampleRate: SAMPLE_RATE, latencyHint: 'interactive' });
    watchAudioContext(ctx, 'capture');
    try {
      // Bounded like play(): addModule fetches and compiles, and the whole of
      // start() — and so the join that awaits it — is behind this one await,
      // inside a queue that serialises every later pipeline change. One that
      // never settles is a call with no microphone, no device switch, no mute
      // and no Auto step for the rest of its life, with the device menu stuck
      // showing itself busy.
      await withTimeout(addTapModule(ctx), MODULE_TIMEOUT_MS, 'pcm-tap addModule', true);
    } catch (err) {
      // No tap means no PCM at all: WebKit has no MediaStreamTrackProcessor,
      // so this worklet is the only path from the microphone to the encoder.
      // There is nothing to degrade to, unlike the denoiser — so the call goes
      // on without a microphone rather than failing a join that has already
      // been reported as successful and left the user looking at the call.
      //
      // It has to say so, though. A microphone that was never published looks
      // exactly like one nobody is speaking into, and this is the one failure
      // the person it happened to cannot see: their own tile is drawn from the
      // capture stream, which is fine, and their speaking ring is driven from
      // the denoiser, which never runs.
      void ctx.close();
      bridge.report('ERROR', 'microphone capture unavailable: the tap worklet would not load', {
        err: String(err),
      });
      this.#declare({ type: 'untrack', untrack: 'audio' });
      this.onFailure?.('Your microphone could not start. Others cannot hear you.');
      return;
    }
    this.#audioCtx = ctx;

    const encoder = new AudioEncoder({
      output: (chunk, meta) => this.#onAudioChunk(chunk, meta),
      error: (err) =>
        this.#failAudio(encoder, 'audio encoder failed', {
          err: String(err),
          bitrate: String(settings.bitrate),
        }),
    });
    bridge.report('INFO', 'audio encoder configured', {
      codec: AUDIO_CODEC,
      sampleRate: String(ctx.sampleRate),
      bitrate: String(settings.bitrate),
    });
    encoder.configure({
      codec: AUDIO_CODEC,
      sampleRate: ctx.sampleRate,
      numberOfChannels: 1,
      bitrate: settings.bitrate,
      // Stated rather than left to the default, so the packet length is not
      // something the platform gets to decide: it is what the backend's group
      // sizing and the whole audio budget are built on.
      opus: { frameDuration: OPUS_FRAME_US },
    });
    this.#audioEncoder = encoder;

    const source = ctx.createMediaStreamSource(new MediaStream([track]));
    const tap = new AudioWorkletNode(ctx, 'pcm-tap');
    source.connect(tap);
    // WebKit only pulls a graph that reaches the destination, so route the
    // tap there through a silent gain rather than leaving it dangling —
    // otherwise process() is never called and no PCM arrives.
    const mute = ctx.createGain();
    mute.gain.value = 0;
    tap.connect(mute);
    mute.connect(ctx.destination);
    this.#tap = tap;

    // The audio track is deliberately NOT declared here. Opus needs its
    // OpusHead description to configure a decoder, and the encoder only
    // reveals that on its first output. Declaring early would publish a
    // catalog without it, and every subscriber would then have to tear
    // down its decoder and resubscribe when the real config landed a
    // moment later. #onAudioChunk declares it once, correctly.
    if (settings.denoise) {
      // Loading is deliberately not awaited: capture starts immediately on
      // the platform's own suppression and the model joins in once ready.
      void this.#denoiser.load();
    }

    // A fresh context means a fresh capture clock, starting again from zero.
    this.#haveAudioEpoch = false;
    this.#skewWindowEndUs = 0;

    // What the pipeline was built with, for the same reason as #startVideo.
    this.#audioFor = { ...settings, deviceId: this.#audioFor?.deviceId ?? settings.deviceId };
    this.#audioRunning = true;
    tap.port.onmessage = (ev: MessageEvent<TapBlock>) => {
      if (!this.#audioRunning) return;
      this.#onTapBlock(ev.data);
    };
    this.#audioBitrate = settings.bitrate;
    this.#lastTapMs = performance.now();
    this.#audioRestarts = 0;
    this.#startCaptureWatchdog();
  }

  /**
   * Encodes one captured block, unless it has been waiting too long.
   *
   * The tap produces audio in real time whatever else is happening, so blocks
   * queue on the port whenever the main thread stalls — loading the denoiser's
   * WASM, configuring decoders, a peer's first keyframe. Encoding a backlog is
   * the wrong thing to do: the audio still goes out, and the queue still drains
   * at exactly 1×, so the delay never comes back. On a real two-party call this
   * left the sound 600 ms behind the picture indefinitely.
   *
   * So audio that is already too late to be worth hearing is dropped instead,
   * which is the one action that actually shortens the queue. A gap is audible
   * once; permanent latency is audible for the whole call. Dropping also
   * degrades gracefully on a machine that simply cannot keep up: it sheds
   * exactly as much as it must to stay current, rather than falling further
   * behind without bound.
   */
  #onTapBlock(block: TapBlock): void {
    // Proof the tap is still delivering, for the watchdog. The audio side has
    // the same self-perpetuating shape as the frame pump — a worklet posting
    // to a port — and the same failure: it can simply stop, with the encoder
    // still configured and the track still declared.
    this.#lastTapMs = performance.now();

    // Both clocks are the AudioContext's, so this is a real waiting time and
    // not an artefact of two unrelated clocks.
    const lateUs = (this.#audioCtx?.currentTime ?? 0) * 1e6 - block.captureUs - AUDIO_BLOCK_US;
    if (lateUs > MAX_AUDIO_LATE_US) {
      this.#dropped++;
      return;
    }
    this.#trackAudioClock(block.captureUs);
    this.#emitAudioFrame(block.samples, block.captureUs);
  }

  /**
   * Keeps the capture clock's offset to the shared media clock up to date.
   *
   * The capture clock counts rendered audio, so it stops when rendering stops —
   * and rendering does stop. Measured on a real call, the audio device pauses
   * for about 500 ms during startup, while the denoiser's WASM compiles and the
   * first subscriptions are set up. Half a second of the world happened and the
   * capture clock has no record of it, so from then on it names every block
   * half a second early.
   *
   * Hence a tracked offset rather than one taken once. The estimate is the
   * *smallest* skew seen across a window, because the main thread can only ever
   * make a block look later than it was, never earlier: the minimum is the
   * observation least polluted by whatever else was running. A stall raises
   * every subsequent skew, floor included, so the offset follows it up.
   */
  #trackAudioClock(captureUs: number): void {
    const wallUs = performance.now() * 1000 - this.#epochUs;
    const skewUs = wallUs - captureUs;

    if (!this.#haveAudioEpoch) {
      this.#audioEpochUs = skewUs;
      this.#haveAudioEpoch = true;
      this.#skewFloorUs = skewUs;
      this.#skewWindowEndUs = wallUs + SKEW_WINDOW_US;
      return;
    }

    if (skewUs < this.#skewFloorUs) this.#skewFloorUs = skewUs;
    if (wallUs < this.#skewWindowEndUs) return;

    // Upward steps are taken whole — they are lost audio, and until the offset
    // follows, every timestamp claims to be earlier than it is.
    this.#audioEpochUs = this.#skewFloorUs > this.#audioEpochUs
      ? this.#skewFloorUs
      : Math.max(this.#skewFloorUs, this.#audioEpochUs - SKEW_SLEW_US);
    this.#skewFloorUs = skewUs;
    this.#skewWindowEndUs = wallUs + SKEW_WINDOW_US;
  }

  /**
   * Denoises one frame, tracks voice activity, and encodes it once a whole
   * Opus packet's worth has accumulated.
   *
   * Voice activity is still per denoiser frame — it drives an indicator, and
   * halving its rate to suit the encoder would only make the border slower to
   * light. Only the encoding waits.
   */
  #emitAudioFrame(frame: Float32Array, captureUs: number): void {
    if (!this.#audioEncoder || this.#audioEncoder.state !== 'configured') return;

    const wasSpeaking = this.#voice.speaking;
    this.#voice = this.#denoiser.process(frame);
    this.#stats.denoiseActive = this.#denoiser.active;
    if (this.#voice.speaking !== wasSpeaking) {
      this.onVoice?.(this.#voice);
    }

    // The packet is timed from its first sample, not its last: it is the
    // moment the audio was captured, and dating it from the end would put
    // every packet 20 ms into the future.
    if (this.#opusLength === 0) this.#opusStartUs = captureUs;
    this.#opusFrame.set(frame, this.#opusLength);
    this.#opusLength += frame.length;
    if (this.#opusLength < OPUS_FRAME_SAMPLES) return;
    this.#opusLength = 0;

    // Dropped rather than queued once the encoder is behind, for the same
    // reason the tap drops a block that waited too long: the packet goes out
    // either way, and queueing it keeps the delay for good.
    if (this.#audioEncoder.encodeQueueSize > MAX_AUDIO_ENCODE_QUEUE) {
      this.#dropped++;
      return;
    }

    const timestamp = Math.round(this.#audioEpochUs + this.#opusStartUs);
    try {
      const data = new AudioData({
        format: 'f32-planar',
        sampleRate: SAMPLE_RATE,
        numberOfFrames: this.#opusFrame.length,
        numberOfChannels: 1,
        timestamp,
        // AudioData copies on construction, so handing it the reused
        // accumulation buffer is safe. The DOM types widen this slot to
        // the SharedArrayBuffer-backed case we never hit.
        data: this.#opusFrame as unknown as BufferSource,
      });
      this.#audioEncoder.encode(data);
      data.close();
    } catch (err) {
      bridge.report('WARN', 'audio encode failed', { err: String(err) });
    }
  }

  /**
   * One Opus packet under construction, filled a denoiser frame at a time.
   *
   * Exactly two frames fit, so a block never straddles the boundary and there
   * is no partial remainder to carry.
   */
  #opusFrame = new Float32Array(OPUS_FRAME_SAMPLES);
  #opusLength = 0;
  #opusStartUs = 0;

  #onAudioChunk(chunk: EncodedAudioChunk, meta?: EncodedAudioChunkMetadata): void {
    this.#flushUndelivered();
    const payload = new Uint8Array(chunk.byteLength);
    chunk.copyTo(payload);

    let config: Uint8Array | undefined;
    const description = meta?.decoderConfig?.description;
    if (description) {
      config = toBytes(description);
      // First output: this is where the OpusHead becomes available, so it
      // is where the track gets declared. The backend puts the description
      // in the catalog's initDataList, where a late subscriber finds it.
      if (!this.#audioConfigSent) {
        this.#audioConfigSent = true;
        this.#declare({
          type: 'track',
          track: {
            kind: 'audio',
            codec: meta?.decoderConfig?.codec ?? AUDIO_CODEC,
            sampleRate: meta?.decoderConfig?.sampleRate ?? SAMPLE_RATE,
            channels: meta?.decoderConfig?.numberOfChannels ?? 1,
            bitrate: this.#audioBitrate,
            description: toBase64(config),
          },
        });
      }
    }

    this.#audioFrames++;
    this.#audioBytes += payload.byteLength;

    if (!bridge.sendFrame({
      kind: KIND_AUDIO,
      handle: HANDLE_LOCAL_AUDIO,
      timestamp: chunk.timestamp,
      keyFrame: false,
      config,
      payload,
      // Carried in LOC's AudioLevel property, which is how remote peers
      // light their speaking indicator without decoding our audio.
      audioLevel: this.#voice.rfc6464,
    })) {
      this.#dropped++;
    }
  }

  #audioConfigSent = false;
  #audioBitrate = 0;

  #denoiser = new Denoiser();
  /** Latest voice state, and the RFC 6464 byte published with each frame. */
  #voice: VoiceState = { speaking: false, level: 0, rfc6464: 127 };

  /** Notified whenever the local speaking state changes. */
  onVoice: ((state: VoiceState) => void) | null = null;

  /**
   * Notified when the video source stops of its own accord, which in practice
   * means a screen share ended somewhere other than in this app. The store
   * decides what to fall back to; capture only reports the fact.
   */
  onVideoSourceLost: (() => void) | null = null;

  /**
   * Notified when a kind has stopped publishing and will not resume on its
   * own — a dead encoder, a worklet that would not load.
   *
   * A sentence for a person rather than the attributes the log gets, because
   * this is the one class of failure its owner cannot see: the local tile is
   * drawn from the capture stream and keeps moving, and the speaking ring is
   * driven by a denoiser that is no longer being fed.
   */
  onFailure: ((detail: string) => void) | null = null;
  /**
   * Called when capture recovers from a failure it has already reported.
   *
   * Without it the banner outlives the fault: "others cannot see you" stays on
   * screen after they can, which is worse than never having said it.
   */
  onRecovered: (() => void) | null = null;

  /** Samples the counters into per-second rates for the debug panel. */
  /**
   * Changes what the running encoder is asked for, without rebuilding anything.
   *
   * The ordinary path for a bitrate change is a settings change, and that
   * rebuilds the whole pipeline: a new encoder, a re-declared track, a
   * republished catalog and a decoder reconfigure for every subscriber. That is
   * the right cost for someone picking a different rate and far too much to pay
   * every time a link moves, which is what makes this a second path rather than
   * a reuse of the first.
   *
   * A reconfigure is not free either — expect a keyframe out of it — but a group
   * already opens on one every second, so an extra one costs a fraction of a
   * GOP. bitrate.ts is what keeps them rare.
   *
   * Tried once. If the platform will not reconfigure a live encoder the answer
   * will not change on the next sample, and retrying every quarter second would
   * bury the line that said so; the rate then stays where it is, which is the
   * behaviour this app had before there was a controller at all.
   */
  setVideoBitrate(bitrate: number): void {
    const encoder = this.#videoEncoder;
    const current = this.#videoConfig;
    if (!encoder || !current || this.#bitrateFixed) return;
    if (encoder.state !== 'configured' || current.bitrate === bitrate) return;

    const next: VideoEncoderConfig = { ...current, bitrate };
    try {
      encoder.configure(next);
    } catch (err) {
      this.#bitrateFixed = true;
      bridge.report('WARN', 'the encoder will not change bitrate; leaving it fixed', {
        from: String(current.bitrate),
        to: String(bitrate),
        err: String(err),
      });
      return;
    }
    this.#videoConfig = next;
    bridge.report('INFO', 'video bitrate changed', {
      from: String(current.bitrate),
      to: String(bitrate),
    });
  }

  sampleStats(): CaptureStats {
    const now = performance.now();
    const elapsed = this.#lastSample ? (now - this.#lastSample) / 1000 : 0;
    this.#lastSample = now;
    if (elapsed > 0) {
      this.#videoRate.push({ bytes: this.#videoBytes, seconds: elapsed });
      if (this.#videoRate.length > VIDEO_KBPS_WINDOW) this.#videoRate.shift();
      let windowBytes = 0;
      let windowSeconds = 0;
      for (const s of this.#videoRate) {
        windowBytes += s.bytes;
        windowSeconds += s.seconds;
      }
      this.#stats = {
        videoFps: this.#videoFrames / elapsed,
        videoKbps: (windowBytes * 8) / 1000 / windowSeconds,
        videoBitrateTarget: this.#videoConfig?.bitrate ?? 0,
        encodeQueue: this.#videoEncoder?.encodeQueueSize ?? 0,
        audioFps: this.#audioFrames / elapsed,
        audioKbps: (this.#audioBytes * 8) / 1000 / elapsed,
        audioEncodeQueue: this.#audioEncoder?.encodeQueueSize ?? 0,
        keyFrames: this.#stats.keyFrames + this.#keyFrames,
        dropped: this.#stats.dropped + this.#dropped,
        echoCancellation: this.#stats.echoCancellation,
        noiseSuppression: this.#stats.noiseSuppression,
        autoGainControl: this.#stats.autoGainControl,
        denoiseActive: this.#denoiser.active,
      };
    }
    this.#videoFrames = 0;
    this.#videoBytes = 0;
    this.#audioFrames = 0;
    this.#audioBytes = 0;
    this.#keyFrames = 0;
    this.#dropped = 0;
    return this.#stats;
  }

  /**
   * Brings the capture in line with a new selection on a live call.
   *
   * The MOQ publications belong to the backend and stay open, so this only
   * rebuilds the local pipeline — and only the half of it that changed. Muting
   * the microphone leaves the camera track, its encoder and its catalog entry
   * exactly as they were, which is what keeps a mute from costing every
   * subscriber a decoder reconfigure and a wait for the next keyframe. A
   * resolution change is still picked up by the fresh `track` declaration that
   * #startVideo sends, which republishes the catalog and makes subscribers
   * reconfigure.
   *
   * The media clock is deliberately carried across, so timestamps stay
   * monotonic through the swap. The audio epoch is not: a rebuilt audio side
   * brings a new AudioContext and so a new capture clock, and #startAudio takes
   * the offset again on the far side, against the same media clock.
   */
  switchDevices(
    video: VideoSettings | null,
    audio: AudioSettings | null,
  ): Promise<MediaStream | null> {
    // Taken once around the pair, not once each: opening and starting are one
    // change, and letting another change in between them would leave devices
    // open that nothing had started.
    return this.#serial(() => this.#switchDevices(video, audio));
  }

  async #switchDevices(
    video: VideoSettings | null,
    audio: AudioSettings | null,
  ): Promise<MediaStream | null> {
    try {
      if (video || audio) {
        await this.#open(video, audio);
        await this.#start(video, audio);
      } else {
        // Both off is a state the toggles reach in two clicks, not an error:
        // the call stays up with nothing published. The media clock is kept, so
        // turning a kind back on continues the same timeline rather than
        // sending timestamps backwards.
        this.#stopVideo();
        this.#stopAudio();
        this.stream = null;
      }
    } finally {
      // Withdraw every kind that is not publishing — switched off, or asked
      // for and failed to open. A catalog entry with nothing behind it leaves
      // each subscriber holding a decoder that will never be fed again, sitting
      // on the last frame it got. Withdrawing a kind that was never declared is
      // a no-op on the backend, so this needs no memory of what came before.
      if (!this.#videoRunning) {
        this.#declare({ type: 'untrack', untrack: 'video' });
      }
      if (!this.#audioRunning) this.#declare({ type: 'untrack', untrack: 'audio' });
    }

    bridge.report('INFO', 'capture devices switched', {
      video: this.#videoRunning ? 'on' : 'off',
      audio: this.#audioRunning ? 'on' : 'off',
    });
    return this.stream;
  }

  /** Releases both devices, the encoders and the audio graph. */
  stop(): void {
    this.#stopVideo();
    this.#stopAudio();
    this.stream = null;
    // Only a full stop re-bases the media clock: everything that was using it
    // is gone, so the next session is free to start from zero.
    this.#epochUs = 0;
    // Leaving the call is not a fault to recover from.
    if (this.#videoRecovery !== null) {
      clearTimeout(this.#videoRecovery);
      this.#videoRecovery = null;
    }
    this.#videoRecoveryWait = 0;
    this.#reportedMute = false;
  }

  /** Releases the camera, its encoder and the frame pump. */
  #stopVideo(): void {
    this.#videoRunning = false;
    this.#detachTrackWatch?.();
    this.#stopCaptureWatchdogIfIdle();

    if (this.#videoEncoder && this.#videoEncoder.state !== 'closed') this.#videoEncoder.close();
    this.#videoEncoder = null;
    if (this.#video) {
      this.#video.pause();
      this.#video.srcObject = null;
      this.#video = null;
    }
    this.#dropTracks('video');

    // Restart the cadence, so whatever starts next opens with a keyframe: a
    // subscriber that reconfigured on the new declaration cannot begin on a
    // delta frame.
    this.#frameIndex = 0;
    this.#lastEncodeMs = 0;
    this.#videoFor = null;
  }

  /** Releases the microphone, its encoder and the audio graph. */
  #stopAudio(): void {
    const wasSpeaking = this.#voice.speaking;
    this.#audioRunning = false;
    this.#stopCaptureWatchdogIfIdle();

    if (this.#audioEncoder && this.#audioEncoder.state !== 'closed') this.#audioEncoder.close();
    this.#audioEncoder = null;
    if (this.#tap) {
      this.#tap.port.onmessage = null;
      this.#tap.disconnect();
      this.#tap = null;
    }
    if (this.#audioCtx) {
      void this.#audioCtx.close();
      this.#audioCtx = null;
    }
    this.#dropTracks('audio');
    this.#denoiser.destroy();

    this.#audioConfigSent = false;
    // A half-filled packet belongs to the pipeline that was collecting it; the
    // next one starts its own, on its own clock.
    this.#opusLength = 0;
    this.#audioEpochUs = 0;
    this.#haveAudioEpoch = false;
    this.#skewFloorUs = 0;
    this.#skewWindowEndUs = 0;
    this.#voice = { speaking: false, level: 0, rfc6464: 127 };
    this.#audioFor = null;
    // Say so, rather than only recording it: a mute taken mid-word would
    // otherwise leave the local speaking ring latched on with nothing left to
    // clear it.
    if (wasSpeaking) this.onVoice?.(this.#voice);
  }

  /**
   * Records what the video pipeline should be compared against next time.
   *
   * The resolution is kept as it was *asked* for, not as granted: a camera that
   * cannot do 1080p hands back 720p, and asking again would otherwise look like
   * a change on every comparison. The device is the opposite — the granted one,
   * because the first open names none and the selection that follows names the
   * one it just got.
   *
   * A screen is deliberately left with no device at all. WebKit does report a
   * deviceId on a display track, but it is not a value this app can ask for, and
   * recording it made every later comparison mismatch — which meant a mute
   * mid-share re-opened the screen picker.
   */
  #rememberVideo(settings: VideoSettings, granted?: MediaStreamTrack): void {
    const deviceId = settings.source === 'screen'
      ? undefined
      : granted?.getSettings().deviceId ?? this.#videoFor?.deviceId ?? settings.deviceId;
    this.#videoFor = { ...settings, deviceId };
  }

  /** Stops and removes the live tracks of one kind, leaving the other alone. */
  #dropTracks(kind: 'video' | 'audio'): void {
    const stream = this.stream;
    if (!stream) return;
    const tracks = kind === 'video' ? stream.getVideoTracks() : stream.getAudioTracks();
    tracks.forEach((track) => {
      track.stop();
      stream.removeTrack(track);
    });
  }

  /**
   * Makes the next captured frame a keyframe.
   *
   * Used after a reconnect: the new session has no open group, and the
   * publisher will not start one on a delta frame, so without this the
   * remote view stays blank until the next scheduled keyframe.
   */
  forceKeyFrame(): void {
    this.#forceKeyFrame = true;
  }

  /** The local participant's current voice state. */
  get voice(): VoiceState {
    return this.#voice;
  }
}

export const capture = new Capture();

/**
 * Lists cameras and microphones. Labels are only populated once capture
 * permission has been granted, so callers that want named devices should
 * open a stream first.
 */
export async function listDevices(): Promise<{
  cameras: MediaDeviceInfo[];
  microphones: MediaDeviceInfo[];
}> {
  const devices = await navigator.mediaDevices.enumerateDevices();
  return {
    cameras: devices.filter((d) => d.kind === 'videoinput'),
    microphones: devices.filter((d) => d.kind === 'audioinput'),
  };
}
