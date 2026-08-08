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
import {
  HANDLE_LOCAL_AUDIO,
  HANDLE_LOCAL_VIDEO,
  KIND_AUDIO,
  KIND_VIDEO,
  toBase64,
  toBytes,
} from './protocol';
import { DENOISE_FRAME, Denoiser, type VoiceState } from './denoise';
import { addTapModule, type TapBlock } from './worklets';

/** Seconds between forced keyframes — also the video group length. */
const KEYFRAME_INTERVAL_SEC = 2;

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
  /**
   * The bitrate below which this size stops being worth asking for.
   *
   * What keeps "Auto" honest. A rung is only a candidate once the selected
   * bitrate can actually feed it: 1080p carried at 1.5 Mbps is mush where the
   * same budget carries 720p cleanly, so a big tile on a small budget should
   * come down to a size the budget can hold rather than up to the one it looks
   * like it wants.
   */
  minBitrate: number;
}

/** The sizes on offer, smallest first. Auto reads this in order. */
export const VIDEO_LADDER: readonly VideoRung[] = [
  { width: 640, height: 360, label: '640 × 360', minBitrate: 400_000 },
  { width: 854, height: 480, label: '854 × 480', minBitrate: 700_000 },
  { width: 1280, height: 720, label: '1280 × 720', minBitrate: 1_200_000 },
  { width: 1920, height: 1080, label: '1920 × 1080', minBitrate: 2_500_000 },
];

/**
 * The resolution setting that names no size, and instead follows the grid.
 *
 * Not a rung: it resolves to one, and which one changes as people join and
 * leave. See autoVideoRung in layout.ts.
 */
export const RESOLUTION_AUTO = 'auto';

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
    have.bitrate === want.bitrate
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
  videoKbps: number;
  encodeQueue: number;
  audioFps: number;
  audioKbps: number;
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
  #videoFor: VideoSettings | null = null;
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
  #audioFrames = 0;
  #audioBytes = 0;
  #keyFrames = 0;
  #dropped = 0;
  #lastSample = 0;
  #stats: CaptureStats = {
    videoFps: 0, videoKbps: 0, encodeQueue: 0,
    audioFps: 0, audioKbps: 0, keyFrames: 0, dropped: 0,
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
    await el.play();
    this.#video = el;

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
    encoder.configure({
      codec,
      width,
      height,
      bitrate: settings.bitrate,
      framerate,
      latencyMode: 'realtime',
      // Annex B puts SPS/PPS in the bitstream ahead of every keyframe, so
      // a subscriber can start decoding from any group without an
      // out-of-band description.
      avc: { format: 'annexb' },
    });
    this.#videoEncoder = encoder;

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
      bitrate: String(settings.bitrate),
      state: encoder.state,
    });

    // Declare the track now rather than on first output: the backend needs
    // it in its catalog before remote participants can subscribe, and
    // waiting for a chunk would delay that by a frame interval.
    bridge.send({
      type: 'track',
      track: {
        kind: 'video',
        codec,
        width,
        height,
        framerate,
        bitrate: settings.bitrate,
      },
    });

    const keyEvery = Math.max(1, Math.round(framerate * KEYFRAME_INTERVAL_SEC));

    /** Least time between encoded frames — see FRAME_GAP_TOLERANCE. */
    const minFrameGapMs = (1000 / framerate) * FRAME_GAP_TOLERANCE;

    const pump = () => {
      if (!this.#videoRunning || !this.#videoEncoder || !this.#video) return;

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
          this.#videoEncoder.encode(frame, {
            keyFrame: forced || this.#frameIndex % keyEvery === 0,
          });
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
    this.#videoRunning = true;
    el.requestVideoFrameCallback(pump);
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
  #failVideo(encoder: VideoEncoder, reason: string, attrs: Record<string, string>): void {
    if (this.#videoEncoder !== encoder) return;
    this.#videoRunning = false;
    bridge.report('ERROR', reason, attrs);
    bridge.send({ type: 'untrack', untrack: 'video' });
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

    if (!bridge.sendFrame({
      kind: KIND_VIDEO,
      handle: HANDLE_LOCAL_VIDEO,
      timestamp: chunk.timestamp,
      keyFrame: isKey,
      config,
      payload,
    })) {
      this.#dropped++;
    }
  }

  async #startAudio(settings: AudioSettings): Promise<void> {
    const track = this.stream!.getAudioTracks()[0];
    const ctx = new AudioContext({ sampleRate: SAMPLE_RATE, latencyHint: 'interactive' });
    try {
      await addTapModule(ctx);
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
      bridge.send({ type: 'untrack', untrack: 'audio' });
      return;
    }
    this.#audioCtx = ctx;

    const encoder = new AudioEncoder({
      output: (chunk, meta) => this.#onAudioChunk(chunk, meta),
      error: (err) => bridge.report('ERROR', 'audio encoder failed', { err: String(err) }),
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
        bridge.send({
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

  /** Samples the counters into per-second rates for the debug panel. */
  sampleStats(): CaptureStats {
    const now = performance.now();
    const elapsed = this.#lastSample ? (now - this.#lastSample) / 1000 : 0;
    this.#lastSample = now;
    if (elapsed > 0) {
      this.#stats = {
        videoFps: this.#videoFrames / elapsed,
        videoKbps: (this.#videoBytes * 8) / 1000 / elapsed,
        encodeQueue: this.#videoEncoder?.encodeQueueSize ?? 0,
        audioFps: this.#audioFrames / elapsed,
        audioKbps: (this.#audioBytes * 8) / 1000 / elapsed,
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
      if (!this.#videoRunning) bridge.send({ type: 'untrack', untrack: 'video' });
      if (!this.#audioRunning) bridge.send({ type: 'untrack', untrack: 'audio' });
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
  }

  /** Releases the camera, its encoder and the frame pump. */
  #stopVideo(): void {
    this.#videoRunning = false;

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
