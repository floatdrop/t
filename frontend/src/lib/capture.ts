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

export interface VideoSettings {
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
 * H.264 baseline, level 3.1. Baseline keeps the bitstream to what every
 * decoder handles, and the probe confirmed both encode and decode support
 * with Annex B framing in this WebView.
 */
const VIDEO_CODEC = 'avc1.42E01F';
const AUDIO_CODEC = 'opus';

/** Microphone sample rate. Opus is defined at 48 kHz. */
const SAMPLE_RATE = 48000;

/** How much audio one tap block carries, in microseconds. */
const AUDIO_BLOCK_US = (DENOISE_FRAME / SAMPLE_RATE) * 1e6;

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
  async open(video: VideoSettings | null, audio: AudioSettings | null): Promise<MediaStream> {
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
    const constraints: MediaStreamConstraints = {};
    if (video) {
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

    const fresh = await navigator.mediaDevices.getUserMedia(constraints);
    if (!this.stream) this.stream = new MediaStream();
    fresh.getTracks().forEach((track) => this.stream!.addTrack(track));
    bridge.report('INFO', 'capture devices opened', {
      tracks: fresh.getTracks().map((t) => `${t.kind}:${t.label}`).join(', '),
    });

    // What each pipeline is running on is remembered as the device that was
    // granted, not the one asked for. The first open names no device at all,
    // and the selection that follows names the one it just got — recording the
    // request would make those two look different and rebuild for nothing.
    const cam = fresh.getVideoTracks()[0];
    if (video && cam) {
      this.#videoFor = { ...video, deviceId: cam.getSettings().deviceId ?? video.deviceId };
    }

    const mic = fresh.getAudioTracks()[0];
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
  async start(video: VideoSettings | null, audio: AudioSettings | null): Promise<void> {
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
    const actual = track.getSettings();
    const width = actual.width ?? settings.width;
    const height = actual.height ?? settings.height;
    const framerate = actual.frameRate ?? settings.framerate;

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
      error: (err) => bridge.report('ERROR', 'video encoder failed', { err: String(err) }),
    });
    bridge.report('INFO', 'video encoder configured', {
      codec: VIDEO_CODEC,
      size: `${width}x${height}`,
      framerate: String(Math.round(framerate)),
      bitrate: String(settings.bitrate),
    });
    encoder.configure({
      codec: VIDEO_CODEC,
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

    // Declare the track now rather than on first output: the backend needs
    // it in its catalog before remote participants can subscribe, and
    // waiting for a chunk would delay that by a frame interval.
    bridge.send({
      type: 'track',
      track: {
        kind: 'video',
        codec: VIDEO_CODEC,
        width,
        height,
        framerate,
        bitrate: settings.bitrate,
      },
    });

    const keyEvery = Math.max(1, Math.round(framerate * KEYFRAME_INTERVAL_SEC));
    const pump = () => {
      if (!this.#videoRunning || !this.#videoEncoder || !this.#video) return;
      // Dropping frames while the encoder is backed up keeps latency
      // bounded; queueing them would only push the backlog further out.
      if (this.#videoEncoder.encodeQueueSize > 2) {
        this.#dropped++;
      } else {
        let frame: VideoFrame | null = null;
        try {
          frame = new VideoFrame(this.#video, {
            timestamp: Math.round(performance.now() * 1000 - this.#epochUs),
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

    // The running pipeline is what "unchanged" now means, so record what it was
    // built with — encoder settings included, since a bitrate can be chosen
    // before joining and only takes effect here. The device stays as the
    // acquisition resolved it; the resolution stays as it was *asked* for,
    // because a camera that cannot do 1080p grants 720p and asking again would
    // otherwise look like a change on every comparison.
    this.#videoFor = { ...settings, deviceId: this.#videoFor?.deviceId ?? settings.deviceId };
    this.#videoRunning = true;
    el.requestVideoFrameCallback(pump);
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
    await addTapModule(ctx);
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

  /** Denoises one frame, tracks voice activity, and encodes it. */
  #emitAudioFrame(frame: Float32Array, captureUs: number): void {
    if (!this.#audioEncoder || this.#audioEncoder.state !== 'configured') return;

    const wasSpeaking = this.#voice.speaking;
    this.#voice = this.#denoiser.process(frame);
    this.#stats.denoiseActive = this.#denoiser.active;
    if (this.#voice.speaking !== wasSpeaking) {
      this.onVoice?.(this.#voice);
    }

    const timestamp = Math.round(this.#audioEpochUs + captureUs);
    try {
      const data = new AudioData({
        format: 'f32-planar',
        sampleRate: SAMPLE_RATE,
        numberOfFrames: frame.length,
        numberOfChannels: 1,
        timestamp,
        // AudioData copies on construction, so handing it the reused
        // accumulation buffer is safe. The DOM types widen this slot to
        // the SharedArrayBuffer-backed case we never hit.
        data: frame as unknown as BufferSource,
      });
      this.#audioEncoder.encode(data);
      data.close();
    } catch (err) {
      bridge.report('WARN', 'audio encode failed', { err: String(err) });
    }
  }

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
  async switchDevices(
    video: VideoSettings | null,
    audio: AudioSettings | null,
  ): Promise<MediaStream | null> {
    try {
      if (video || audio) {
        await this.open(video, audio);
        await this.start(video, audio);
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
