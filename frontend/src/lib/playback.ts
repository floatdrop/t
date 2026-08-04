/**
 * Playback for remote participants: one WebCodecs decoder per announced
 * track, video painted to a canvas and audio fed into a ring-buffer
 * AudioWorklet.
 *
 * The backend announces each inbound track with a handle before any frame
 * carrying that handle arrives, so a decoder is always configured and
 * waiting. Handles are the only key used here — a participant who changes
 * codec gets a new handle rather than a reconfigured decoder.
 */

import { bridge } from './bridge';
import { KIND_VIDEO, fromBase64, type MediaFrame, type RemoteTrack } from './protocol';
import { addPlayerModule } from './worklets';

/** Per-track playback counters for the debug panel. */
export interface PlaybackStats {
  handle: number;
  participant: string;
  kind: 'video' | 'audio';
  /** Decoded frames per second over the last sample interval. */
  fps: number;
  /** Frames received but not decodable yet (pre-keyframe, or errors). */
  dropped: number;
  decodeQueue: number;
  /** Audio only: samples buffered in the player worklet. */
  buffered?: number;
  underruns?: number;
}

interface VideoSink {
  kind: 'video';
  track: RemoteTrack;
  decoder: VideoDecoder;
  canvas: HTMLCanvasElement | null;
  ctx: CanvasRenderingContext2D | null;
  /** Latest decoded frame, held until a canvas is attached to paint it. */
  pending: VideoFrame | null;
  /** H.264 cannot start on a delta frame; gate until the first keyframe. */
  sawKeyFrame: boolean;
  decoded: number;
  dropped: number;
}

interface AudioSink {
  kind: 'audio';
  track: RemoteTrack;
  decoder: AudioDecoder;
  node: AudioWorkletNode | null;
  decoded: number;
  dropped: number;
  buffered: number;
  underruns: number;
}

type Sink = VideoSink | AudioSink;

export class Playback {
  #sinks = new Map<number, Sink>();
  #audioCtx: AudioContext | null = null;
  #audioReady: Promise<AudioContext> | null = null;
  #lastSample = 0;
  #stats: PlaybackStats[] = [];

  /** Configures a decoder for a newly announced remote track. */
  async add(track: RemoteTrack): Promise<void> {
    this.remove(track.handle);
    if (track.config.kind === 'video') {
      this.#addVideo(track);
    } else {
      await this.#addAudio(track);
    }
  }

  #addVideo(track: RemoteTrack): void {
    const sink: VideoSink = {
      kind: 'video',
      track,
      canvas: null,
      ctx: null,
      pending: null,
      sawKeyFrame: false,
      decoded: 0,
      dropped: 0,
      decoder: null as unknown as VideoDecoder,
    };
    sink.decoder = new VideoDecoder({
      output: (frame) => this.#paint(sink, frame),
      error: (err) =>
        bridge.report('ERROR', 'video decoder failed', {
          participant: track.participant,
          err: String(err),
        }),
    });

    const config: VideoDecoderConfig = {
      codec: track.config.codec,
      optimizeForLatency: true,
    };
    if (track.config.width) config.codedWidth = track.config.width;
    if (track.config.height) config.codedHeight = track.config.height;
    if (track.config.description) {
      config.description = fromBase64(track.config.description);
    }
    try {
      sink.decoder.configure(config);
    } catch (err) {
      bridge.report('ERROR', 'video decoder configure failed', {
        participant: track.participant,
        codec: track.config.codec,
        err: String(err),
      });
      return;
    }
    this.#sinks.set(track.handle, sink);
    bridge.report('INFO', 'video decoder ready', {
      participant: track.participant,
      handle: String(track.handle),
      codec: track.config.codec,
    });
  }

  async #addAudio(track: RemoteTrack): Promise<void> {
    const ctx = await this.#ensureAudio();
    const sink: AudioSink = {
      kind: 'audio',
      track,
      node: null,
      decoded: 0,
      dropped: 0,
      buffered: 0,
      underruns: 0,
      decoder: null as unknown as AudioDecoder,
    };

    const node = new AudioWorkletNode(ctx, 'pcm-player', { outputChannelCount: [1] });
    node.connect(ctx.destination);
    node.port.onmessage = (ev: MessageEvent<{ available: number; underruns: number }>) => {
      sink.buffered = ev.data.available;
      sink.underruns = ev.data.underruns;
    };
    sink.node = node;

    sink.decoder = new AudioDecoder({
      output: (data) => this.#play(sink, data),
      error: (err) =>
        bridge.report('ERROR', 'audio decoder failed', {
          participant: track.participant,
          err: String(err),
        }),
    });

    const config: AudioDecoderConfig = {
      codec: track.config.codec,
      sampleRate: track.config.sampleRate ?? 48000,
      numberOfChannels: track.config.channels ?? 1,
    };
    if (track.config.description) {
      config.description = fromBase64(track.config.description);
    }
    try {
      sink.decoder.configure(config);
    } catch (err) {
      bridge.report('ERROR', 'audio decoder configure failed', {
        participant: track.participant,
        codec: track.config.codec,
        err: String(err),
      });
      node.disconnect();
      return;
    }
    this.#sinks.set(track.handle, sink);
    bridge.report('INFO', 'audio decoder ready', {
      participant: track.participant,
      handle: String(track.handle),
      codec: track.config.codec,
      descriptionBytes: String(track.config.description ? 1 : 0),
    });
  }

  /**
   * Creates the shared playback AudioContext. One context for every
   * participant: contexts are a limited resource and mixing happens in the
   * graph anyway.
   */
  #ensureAudio(): Promise<AudioContext> {
    if (!this.#audioReady) {
      this.#audioReady = (async () => {
        const ctx = new AudioContext({ sampleRate: 48000, latencyHint: 'interactive' });
        await addPlayerModule(ctx);
        this.#audioCtx = ctx;
        return ctx;
      })();
    }
    return this.#audioReady;
  }

  /**
   * Resumes the playback context. Browsers start an AudioContext suspended
   * until a user gesture, so this runs from the click that joins the room.
   */
  async resume(): Promise<void> {
    const ctx = this.#audioCtx ?? (await this.#ensureAudio());
    if (ctx.state === 'suspended') await ctx.resume();
  }

  /** Retires a track's decoder. */
  remove(handle: number): void {
    const sink = this.#sinks.get(handle);
    if (!sink) return;
    this.#sinks.delete(handle);

    if (sink.kind === 'video') {
      sink.pending?.close();
      sink.pending = null;
      if (sink.decoder.state !== 'closed') sink.decoder.close();
    } else {
      if (sink.decoder.state !== 'closed') sink.decoder.close();
      if (sink.node) {
        sink.node.port.onmessage = null;
        sink.node.disconnect();
      }
    }
  }

  /** Retires every decoder — on leaving the room or disconnecting. */
  clear(): void {
    for (const handle of [...this.#sinks.keys()]) this.remove(handle);
  }

  /**
   * Binds the canvas a participant's video paints into. Called by the
   * video tile once its element exists, which is generally after the
   * decoder has already produced a frame.
   */
  attachCanvas(handle: number, canvas: HTMLCanvasElement | null): void {
    const sink = this.#sinks.get(handle);
    if (!sink || sink.kind !== 'video') return;
    sink.canvas = canvas;
    sink.ctx = canvas ? canvas.getContext('2d') : null;
    if (canvas && sink.pending) {
      const frame = sink.pending;
      sink.pending = null;
      this.#paint(sink, frame);
    }
  }

  /** Feeds one inbound frame to its decoder. */
  push(frame: MediaFrame): void {
    const sink = this.#sinks.get(frame.handle);
    if (!sink) return;

    if (frame.kind === KIND_VIDEO) {
      const video = sink as VideoSink;
      if (!video.sawKeyFrame) {
        if (!frame.keyFrame) {
          video.dropped++;
          return;
        }
        video.sawKeyFrame = true;
      }
      if (video.decoder.state !== 'configured') return;
      try {
        video.decoder.decode(new EncodedVideoChunk({
          type: frame.keyFrame ? 'key' : 'delta',
          timestamp: frame.timestamp,
          // Copy: the frame's payload is a view over the WebSocket read
          // buffer, and the decoder holds the chunk asynchronously.
          data: frame.payload.slice(),
        }));
      } catch (err) {
        video.dropped++;
        console.warn('video decode failed', frame.handle, err);
      }
      return;
    }

    const audio = sink as AudioSink;
    if (audio.decoder.state !== 'configured') return;
    try {
      audio.decoder.decode(new EncodedAudioChunk({
        // Every Opus packet stands on its own.
        type: 'key',
        timestamp: frame.timestamp,
        data: frame.payload.slice(),
      }));
    } catch (err) {
      audio.dropped++;
      console.warn('audio decode failed', frame.handle, err);
    }
  }

  #paint(sink: VideoSink, frame: VideoFrame): void {
    sink.decoded++;
    if (!sink.canvas || !sink.ctx) {
      // No tile mounted yet. Keep only the newest frame so a late-mounting
      // canvas paints something immediately instead of staying black.
      sink.pending?.close();
      sink.pending = frame;
      return;
    }
    if (sink.canvas.width !== frame.displayWidth || sink.canvas.height !== frame.displayHeight) {
      sink.canvas.width = frame.displayWidth;
      sink.canvas.height = frame.displayHeight;
    }
    try {
      sink.ctx.drawImage(frame, 0, 0);
    } catch (err) {
      console.warn('canvas paint failed', err);
    } finally {
      frame.close();
    }
  }

  #play(sink: AudioSink, data: AudioData): void {
    sink.decoded++;
    if (!sink.node) {
      data.close();
      return;
    }
    try {
      const samples = new Float32Array(data.numberOfFrames);
      data.copyTo(samples, { planeIndex: 0, format: 'f32-planar' });
      // Transfer rather than copy: the worklet consumes the buffer.
      sink.node.port.postMessage(samples, [samples.buffer]);
    } catch (err) {
      console.warn('audio copy failed', err);
    } finally {
      data.close();
    }
  }

  /** Samples per-track playback counters for the debug panel. */
  sampleStats(): PlaybackStats[] {
    const now = performance.now();
    const elapsed = this.#lastSample ? (now - this.#lastSample) / 1000 : 0;
    this.#lastSample = now;

    const out: PlaybackStats[] = [];
    for (const sink of this.#sinks.values()) {
      const row: PlaybackStats = {
        handle: sink.track.handle,
        participant: sink.track.nickname || sink.track.participant,
        kind: sink.kind,
        fps: elapsed > 0 ? sink.decoded / elapsed : 0,
        dropped: sink.dropped,
        decodeQueue: sink.decoder.decodeQueueSize,
      };
      if (sink.kind === 'audio') {
        row.buffered = sink.buffered;
        row.underruns = sink.underruns;
        // Ask the worklet for fresh buffer depth; it answers before the
        // next sample.
        sink.node?.port.postMessage('stats');
      }
      sink.decoded = 0;
      out.push(row);
    }
    this.#stats = out;
    return out;
  }

  get stats(): PlaybackStats[] {
    return this.#stats;
  }
}

export const playback = new Playback();
