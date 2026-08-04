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
import { addTapModule } from './worklets';

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
}

export const defaultVideoSettings: VideoSettings = {
  width: 1280,
  height: 720,
  framerate: 30,
  bitrate: 1_500_000,
};

export const defaultAudioSettings: AudioSettings = {
  bitrate: 32_000,
};

/** Live counters for the debug panel. */
export interface CaptureStats {
  videoFps: number;
  videoKbps: number;
  encodeQueue: number;
  audioFps: number;
  audioKbps: number;
  keyFrames: number;
  dropped: number;
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

export class Capture {
  stream: MediaStream | null = null;

  #video: HTMLVideoElement | null = null;
  #videoEncoder: VideoEncoder | null = null;
  #audioEncoder: AudioEncoder | null = null;
  #audioCtx: AudioContext | null = null;
  #tap: AudioWorkletNode | null = null;

  #running = false;
  #frameIndex = 0;
  /** Shared epoch so audio and video timestamps sit on one clock. */
  #epochUs = 0;
  #audioSamples = 0;

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
  };

  /**
   * Opens the requested devices. Call before start(). Returns the stream
   * so the caller can show a local preview.
   */
  async open(video: VideoSettings | null, audio: AudioSettings | null): Promise<MediaStream> {
    this.stop();

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
    if (!constraints.video && !constraints.audio) {
      throw new Error('capture: neither camera nor microphone selected');
    }

    this.stream = await navigator.mediaDevices.getUserMedia(constraints);
    bridge.report('INFO', 'capture devices opened', {
      tracks: this.stream.getTracks().map((t) => `${t.kind}:${t.label}`).join(', '),
    });
    return this.stream;
  }

  /** Starts encoding and publishing whatever open() acquired. */
  async start(video: VideoSettings | null, audio: AudioSettings | null): Promise<void> {
    if (!this.stream) throw new Error('capture: open() must run before start()');
    this.#running = true;
    this.#epochUs = performance.now() * 1000;

    if (video && this.stream.getVideoTracks().length > 0) {
      await this.#startVideo(video);
    }
    if (audio && this.stream.getAudioTracks().length > 0) {
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
      if (!this.#running || !this.#videoEncoder || !this.#video) return;
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
          this.#videoEncoder.encode(frame, { keyFrame: this.#frameIndex % keyEvery === 0 });
          this.#frameIndex++;
        } catch (err) {
          bridge.report('WARN', 'video frame capture failed', { err: String(err) });
        } finally {
          frame?.close();
        }
      }
      this.#video.requestVideoFrameCallback(pump);
    };
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
    this.#audioSamples = 0;
    tap.port.onmessage = (ev: MessageEvent<Float32Array>) => {
      if (!this.#running || !this.#audioEncoder) return;
      const pcm = ev.data;
      // Timestamps come from a sample counter, not the wall clock: the
      // encoder needs them monotonic and exactly the width of the audio
      // they describe, which per-callback clock reads would not be.
      const timestamp = Math.round((this.#audioSamples / SAMPLE_RATE) * 1e6);
      try {
        const data = new AudioData({
          format: 'f32-planar',
          sampleRate: SAMPLE_RATE,
          numberOfFrames: pcm.length,
          numberOfChannels: 1,
          timestamp,
          // The worklet posts a plain Float32Array; the DOM types widen
          // this slot to the SharedArrayBuffer-backed case we never hit.
          data: pcm as unknown as BufferSource,
        });
        this.#audioEncoder.encode(data);
        data.close();
        this.#audioSamples += pcm.length;
      } catch (err) {
        bridge.report('WARN', 'audio encode failed', { err: String(err) });
      }
    };
    this.#audioBitrate = settings.bitrate;
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
    })) {
      this.#dropped++;
    }
  }

  #audioConfigSent = false;
  #audioBitrate = 0;

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

  /** Releases the devices, encoders and audio graph. */
  stop(): void {
    this.#running = false;

    if (this.#videoEncoder && this.#videoEncoder.state !== 'closed') this.#videoEncoder.close();
    this.#videoEncoder = null;
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
    if (this.#video) {
      this.#video.pause();
      this.#video.srcObject = null;
      this.#video = null;
    }
    this.stream?.getTracks().forEach((t) => t.stop());
    this.stream = null;

    this.#frameIndex = 0;
    this.#audioConfigSent = false;
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
