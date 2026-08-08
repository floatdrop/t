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
import { decodeAudioLevel } from './denoise';
import { KIND_VIDEO, fromBase64, type MediaFrame, type RemoteTrack } from './protocol';
import { offsetMillis, presentIndex, projectClock, type ClockSample } from './sync';
import { addPlayerModule, watchAudioContext, type PlayerChunk, type PlayerReport } from './worklets';

/**
 * The shared output limiter. Every participant sums into it, so it exists to
 * keep four people talking at once from clipping the device.
 *
 * A compressor with a high ratio just above the point where summing starts to
 * hurt, rather than a hard clipper: it only engages on the loud moments, and a
 * soft knee makes the engagement inaudible. Fast attack because a clip is
 * instantaneous; slow-ish release so it does not pump on speech syllables.
 */
const LIMITER = {
  threshold: -6, // dBFS
  knee: 6,
  ratio: 12,
  attack: 0.003, // seconds
  release: 0.25,
} as const;

/**
 * Most times a decoder may be rebuilt without ever producing a frame.
 *
 * A decoder error is terminal, so recovery means building a new one — but a
 * stream this decoder simply cannot handle would otherwise be rebuilt for every
 * frame that arrives, forever. The counter resets on output, so this only
 * counts failures that never got anywhere: a decoder that recovers has its
 * whole allowance back.
 */
const MAX_DECODER_RESTARTS = 5;

/**
 * Hard cap on the presentation queue, as a backstop rather than a working
 * limit: the sync arithmetic keeps it a handful of frames deep. It matters when
 * the render loop is not running at all — requestAnimationFrame stops while the
 * window is hidden — where frames would otherwise pile up for as long as that
 * lasts.
 */
const MAX_QUEUE = 60;

/**
 * How long the presentation loop may go without a tick before it is taken to
 * have stopped rather than to be running slowly.
 *
 * Well beyond any real frame interval — half a second is fifteen frames at 30
 * fps — so a display that is merely struggling is never mistaken for one that
 * has stopped. See #startWatchdog.
 */
const RENDER_STALL_MS = 500;

/** How often to check that the presentation loop is still being called. */
const RENDER_WATCHDOG_MS = 250;

/** How long to wait for the player worklet to load before failing. */
const MODULE_TIMEOUT_MS = 10000;

/**
 * Rejects if work has not settled within ms.
 *
 * For platform promises that are awaited on a path someone is waiting on: an
 * addModule that never settles is a join that never finishes, with no error to
 * show for it.
 */
async function withDeadline<T>(work: Promise<T>, ms: number, what: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const expired = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`playback: ${what} did not settle within ${ms}ms`)), ms);
  });
  try {
    return await Promise.race([work, expired]);
  } finally {
    clearTimeout(timer);
  }
}

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
  /** Codec string the decoder was configured with. */
  codec: string;
  /**
   * Video only: how far ahead of the audio clock the last presented frame
   * was, in milliseconds. Positive means the picture leads the sound. Absent
   * when the participant publishes no audio, since there is then no clock to
   * measure against.
   */
  avOffsetMs?: number;
  /** Video only: frames decoded but not yet due for presentation. */
  queued?: number;
  /**
   * Video only: the resolution frames are actually decoding at, which can
   * differ from what the catalog declared if the publisher's camera
   * negotiated something else.
   */
  width?: number;
  height?: number;
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
  /** Resolution of the most recently decoded frame. */
  width: number;
  height: number;
  /**
   * What the decoder was configured with, kept so it can be built again.
   *
   * A WebCodecs decoder error is terminal — the decoder closes and cannot be
   * reconfigured — so recovering means constructing a new one, and that needs
   * the config the catalog described.
   */
  config: VideoDecoderConfig;
  /**
   * Consecutive rebuilds since the last frame came out. Reset by output, so a
   * decoder that recovers starts its allowance again and only a decoder that
   * fails repeatedly without ever decoding anything runs out.
   */
  restarts: number;
  /**
   * Decoded frames awaiting their presentation time, oldest first. Painting
   * on decode is what left the picture unsynchronised: it ran as fast as the
   * decoder emitted, with nothing tying it to the sound.
   */
  queue: VideoFrame[];
  /**
   * Offset of the last presented frame, for the debug panel. Null while the
   * participant has no audio clock to measure against.
   */
  avOffsetMs: number | null;
}

interface AudioSink {
  kind: 'audio';
  track: RemoteTrack;
  decoder: AudioDecoder;
  node: AudioWorkletNode | null;
  /** Per-participant level, feeding the shared limiter. */
  gain: GainNode | null;
  decoded: number;
  dropped: number;
  buffered: number;
  underruns: number;
  /** What the decoder was configured with, kept so it can be built again. */
  config: AudioDecoderConfig;
  /** Consecutive rebuilds since the last frame came out. See the video sink. */
  restarts: number;
  /** Trims seen so far, so only an increase is reported. */
  trimmed: number;
  /**
   * Latest playout position reported by the worklet — the master clock for
   * this participant's video. Null until audio actually starts playing.
   */
  clock: ClockSample | null;
}

type Sink = VideoSink | AudioSink;

export class Playback {
  /**
   * Notified when a remote participant's voice activity changes, from the
   * audio level their frames carry.
   */
  onVoice: ((participant: string, speaking: boolean, level: number) => void) | null = null;

  /**
   * Notified when a participant's media has stopped for good on this side.
   *
   * Giving up is the one outcome nobody can infer from the tiles. A retired
   * video decoder leaves the last painted frame exactly where it was, which is
   * what a peer who stopped moving looks like; a retired audio decoder leaves
   * silence, which is what someone not talking sounds like. Both are states a
   * call produces normally, so neither reads as a fault.
   */
  onFailure: ((participant: string, detail: string) => void) | null = null;

  #sinks = new Map<number, Sink>();
  /**
   * Handles being built right now. A removal that arrives before the sink is
   * inserted is recorded here rather than lost — see add().
   */
  #wanted = new Set<number>();
  #audioCtx: AudioContext | null = null;
  #audioReady: Promise<AudioContext> | null = null;
  /** Shared limiter every participant's gain feeds. */
  #limiter: DynamicsCompressorNode | null = null;
  #renderHandle: number | null = null;
  /** performance.now() of the last presentation tick, for the watchdog. */
  #lastTickMs = 0;
  #watchdog: ReturnType<typeof setInterval> | null = null;
  #lastSample = 0;
  #stats: PlaybackStats[] = [];

  /** Configures a decoder for a newly announced remote track. */
  async add(track: RemoteTrack): Promise<void> {
    this.remove(track.handle);
    if (track.config.kind === 'video') {
      this.#addVideo(track);
      return;
    }
    // Marked as wanted before the await, and checked after it. #addAudio waits
    // on the shared context, and a trackGone arriving inside that window finds
    // no sink to remove — it is not inserted until the far side — so without
    // this the participant who just left is installed immediately afterwards
    // and never removed, holding a worklet node and a decoder for the rest of
    // the call.
    this.#wanted.add(track.handle);
    try {
      await this.#addAudio(track);
    } finally {
      if (!this.#wanted.delete(track.handle)) {
        this.remove(track.handle);
      }
    }
  }

  /**
   * Builds (or rebuilds) a video sink's decoder. Returns false if it could not
   * be configured at all, which is not recoverable and leaves no sink.
   *
   * Recovery matters more than it looks. A WebCodecs decoder error closes the
   * decoder for good, and push() then skips every later frame because the state
   * is no longer "configured" — silently, for the rest of the call. One decode
   * error used to freeze that participant's tile permanently, on whatever frame
   * happened to be last, with a single line in the log to say why.
   */
  #buildVideoDecoder(sink: VideoSink): boolean {
    const decoder = new VideoDecoder({
      output: (frame) => {
        // Proof the decoder works, so the restart allowance is restored.
        sink.restarts = 0;
        this.#enqueue(sink, frame);
      },
      error: (err) => this.#onVideoDecoderError(sink, err),
    });
    try {
      decoder.configure(sink.config);
    } catch (err) {
      bridge.report('ERROR', 'video decoder configure failed', {
        participant: sink.track.participant,
        codec: sink.track.config.codec,
        err: String(err),
      });
      return false;
    }
    sink.decoder = decoder;
    // Whatever the old decoder held is gone, and H.264 cannot resume on a
    // delta: wait for the next keyframe before feeding this one anything.
    // Every group opens on one, so the wait is at most a group.
    sink.sawKeyFrame = false;
    return true;
  }

  /** Replaces a decoder that has failed, while it is still worth trying. */
  #onVideoDecoderError(sink: VideoSink, err: unknown): void {
    if (this.#sinks.get(sink.track.handle) !== sink) return; // already retired

    sink.restarts++;
    if (sink.restarts > MAX_DECODER_RESTARTS) {
      bridge.report('ERROR', 'video decoder failed repeatedly; giving up', {
        participant: sink.track.participant,
        restarts: String(sink.restarts),
        err: String(err),
      });
      this.onFailure?.(sink.track.participant, 'their video cannot be decoded');
      return;
    }

    bridge.report('WARN', 'video decoder failed; rebuilding it', {
      participant: sink.track.participant,
      restarts: String(sink.restarts),
      err: String(err),
    });

    // Queued frames belong to the decoder that just died.
    for (const frame of sink.queue) frame.close();
    sink.queue.length = 0;
    sink.pending?.close();
    sink.pending = null;

    if (!this.#buildVideoDecoder(sink)) this.#sinks.delete(sink.track.handle);
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
      width: 0,
      height: 0,
      queue: [],
      avOffsetMs: null,
      restarts: 0,
      config: null as unknown as VideoDecoderConfig,
      decoder: null as unknown as VideoDecoder,
    };

    const config: VideoDecoderConfig = {
      codec: track.config.codec,
      optimizeForLatency: true,
    };
    if (track.config.width) config.codedWidth = track.config.width;
    if (track.config.height) config.codedHeight = track.config.height;
    if (track.config.description) {
      config.description = fromBase64(track.config.description);
    }
    sink.config = config;

    if (!this.#buildVideoDecoder(sink)) return;
    this.#sinks.set(track.handle, sink);
    this.#startRender();
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
      gain: null,
      decoded: 0,
      dropped: 0,
      buffered: 0,
      underruns: 0,
      trimmed: 0,
      restarts: 0,
      config: null as unknown as AudioDecoderConfig,
      clock: null,
      decoder: null as unknown as AudioDecoder,
    };

    // Non-null because #ensureAudio builds the limiter alongside the context,
    // and this ran after awaiting it.
    const limiter = this.#limiter!;

    const node = new AudioWorkletNode(ctx, 'pcm-player', { outputChannelCount: [1] });
    // Participant → its own gain → the shared limiter → output. Connecting
    // straight to the destination, as this used to, sums everyone at unity
    // with no headroom, so several loud speakers clip.
    const gain = ctx.createGain();
    gain.gain.value = 1;
    node.connect(gain);
    gain.connect(limiter);

    node.port.onmessage = (ev: MessageEvent<PlayerReport>) => {
      sink.buffered = ev.data.available;
      sink.underruns = ev.data.underruns;
      // Said out loud, and only on the edge: one trim is a transient the
      // listener hears as a skip, but a stream of them means this participant's
      // audio is arriving faster than it can be played and something upstream
      // is wrong. Silence about it is what hid two seconds of delay.
      if (ev.data.trimmed > sink.trimmed) {
        sink.trimmed = ev.data.trimmed;
        bridge.report('WARN', 'trimmed the audio buffer to bound its latency', {
          participant: track.participant,
          trims: String(sink.trimmed),
          // How deep it had got, not how deep it is now: a trim leaves the
          // buffer at its floor by construction, so reporting what is left
          // logged the same ~60 ms every time and threw away the one number
          // that says how far behind the sound had fallen.
          droppedFromMs: String(Math.round((ev.data.trimmedFrom / 48000) * 1000)),
          nowMs: String(Math.round((ev.data.available / 48000) * 1000)),
        });
      }
      // Only a playing buffer has a meaningful position; a prerolling or
      // starved one would report a clock that is not advancing, and video
      // scheduled against it would stall.
      sink.clock = ev.data.playing && ev.data.haveClock
        ? { playoutUs: ev.data.playoutUs, atMs: performance.now() }
        : null;
    };
    sink.node = node;
    sink.gain = gain;

    const config: AudioDecoderConfig = {
      codec: track.config.codec,
      sampleRate: track.config.sampleRate ?? 48000,
      numberOfChannels: track.config.channels ?? 1,
    };
    if (track.config.description) {
      config.description = fromBase64(track.config.description);
    }
    sink.config = config;

    if (!this.#buildAudioDecoder(sink)) {
      node.disconnect();
      gain.disconnect();
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
   * Builds (or rebuilds) an audio sink's decoder. Same reasoning as the video
   * one: the error is terminal, and without a rebuild that participant goes
   * silent for the rest of the call while everything still looks connected.
   *
   * No keyframe gate on the way back — every Opus packet stands on its own, so
   * the next one to arrive decodes.
   */
  #buildAudioDecoder(sink: AudioSink): boolean {
    const decoder = new AudioDecoder({
      output: (data) => {
        sink.restarts = 0;
        this.#play(sink, data);
      },
      error: (err) => this.#onAudioDecoderError(sink, err),
    });
    try {
      decoder.configure(sink.config);
    } catch (err) {
      bridge.report('ERROR', 'audio decoder configure failed', {
        participant: sink.track.participant,
        codec: sink.track.config.codec,
        err: String(err),
      });
      return false;
    }
    sink.decoder = decoder;
    return true;
  }

  #onAudioDecoderError(sink: AudioSink, err: unknown): void {
    if (this.#sinks.get(sink.track.handle) !== sink) return; // already retired

    sink.restarts++;
    if (sink.restarts > MAX_DECODER_RESTARTS) {
      bridge.report('ERROR', 'audio decoder failed repeatedly; giving up', {
        participant: sink.track.participant,
        restarts: String(sink.restarts),
        err: String(err),
      });
      this.onFailure?.(sink.track.participant, 'their audio cannot be decoded');
      return;
    }
    bridge.report('WARN', 'audio decoder failed; rebuilding it', {
      participant: sink.track.participant,
      restarts: String(sink.restarts),
      err: String(err),
    });
    if (!this.#buildAudioDecoder(sink)) this.#sinks.delete(sink.track.handle);
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
        watchAudioContext(ctx, 'playback');
        // Bounded, because join() awaits resume() which awaits this: a module
        // that never loads leaves the join button on "Connecting…" with no
        // timeout behind it and nothing to report.
        await withDeadline(addPlayerModule(ctx), MODULE_TIMEOUT_MS, 'pcm-player addModule');
        const limiter = ctx.createDynamicsCompressor();
        limiter.threshold.value = LIMITER.threshold;
        limiter.knee.value = LIMITER.knee;
        limiter.ratio.value = LIMITER.ratio;
        limiter.attack.value = LIMITER.attack;
        limiter.release.value = LIMITER.release;
        limiter.connect(ctx.destination);
        this.#limiter = limiter;
        this.#audioCtx = ctx;
        return ctx;
      })().catch((err) => {
        // Forgotten rather than memoised. Every audio sink waits on this one
        // context and nothing else ever builds another, so a rejection cached
        // here is every remote participant silent for the rest of the call —
        // and silent without a symptom, since the tiles keep painting.
        this.#audioReady = null;
        throw err;
      });
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
    // Withdraws a build in flight, so it is torn down when it lands.
    this.#wanted.delete(handle);
    const sink = this.#sinks.get(handle);
    if (!sink) return;
    this.#sinks.delete(handle);

    if (sink.kind === 'video') {
      sink.pending?.close();
      sink.pending = null;
      // Queued frames hold decoder resources; dropping the references is not
      // enough, they have to be closed.
      for (const frame of sink.queue) frame.close();
      sink.queue.length = 0;
      if (sink.decoder.state !== 'closed') sink.decoder.close();
      this.#stopRenderIfIdle();
    } else {
      if (sink.decoder.state !== 'closed') sink.decoder.close();
      if (sink.node) {
        sink.node.port.onmessage = null;
        sink.node.disconnect();
      }
      sink.gain?.disconnect();
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
      // A decoder that is closed or still being rebuilt takes nothing, and
      // every frame it turns away is a frame the viewer does not see. Counted
      // so the gap between a decoder that is failing and a track that is not
      // arriving is visible in the panel rather than being guessed at.
      if (video.decoder.state !== 'configured') {
        video.dropped++;
        return;
      }
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
    // The publisher's LOC AudioLevel property tells us they are speaking
    // without our having to measure the decoded audio ourselves.
    if (frame.audioLevel !== undefined) {
      const { voiceActivity, level } = decodeAudioLevel(frame.audioLevel);
      this.onVoice?.(sink.track.participant, voiceActivity, level);
    }
    if (audio.decoder.state !== 'configured') {
      audio.dropped++;
      return;
    }
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

  /**
   * Queues a decoded frame for presentation.
   *
   * Frames are counted as decoded here rather than when painted, so the fps
   * column keeps reporting what the decoder produced even if presentation is
   * dropping frames — the difference between the two is the interesting part.
   */
  #enqueue(sink: VideoSink, frame: VideoFrame): void {
    sink.decoded++;
    sink.width = frame.displayWidth;
    sink.height = frame.displayHeight;
    sink.queue.push(frame);
    // Counted, not just discarded. A queue this long means presentation has
    // stopped keeping up with the decoder, and the frames shed here are as
    // lost to the viewer as the ones that never arrived.
    while (sink.queue.length > MAX_QUEUE) {
      sink.queue.shift()!.close();
      sink.dropped++;
    }
  }

  /**
   * Drives presentation for every video sink, once per display refresh.
   *
   * One shared loop rather than a timer per sink: presentation is bounded by
   * the display anyway, so anything finer only burns CPU, and a single rAF
   * callback also means every tile updates in the same paint.
   */
  #startRender(): void {
    if (this.#renderHandle !== null) return;
    const tick = () => {
      this.#renderHandle = requestAnimationFrame(tick);
      const now = performance.now();
      this.#lastTickMs = now;
      for (const sink of this.#sinks.values()) {
        if (sink.kind === 'video') this.#present(sink, now);
      }
    };
    this.#lastTickMs = performance.now();
    this.#renderHandle = requestAnimationFrame(tick);
    this.#startWatchdog();
  }

  /**
   * Restarts the presentation loop if it stops being called.
   *
   * The loop keeps itself alive by requesting the next frame from inside the
   * current one, which is the ordinary shape and has exactly one failure mode:
   * if a callback is never delivered, the chain ends and there is nothing left
   * to notice. #renderHandle still holds the id of that scheduled callback, so
   * it reads as running and #startRender declines to start a second one, and
   * the only other caller is a track being added.
   *
   * WebKit does drop them. Resizing a window on macOS blocks the main thread
   * for the drag, and the loop does not always come back afterwards: measured
   * on a real call, every remote tile froze on its last frame while the page
   * around it carried on — audio playing, counters ticking, plots moving,
   * because timers and the audio thread are untouched. Nothing in the app was
   * wrong and nothing could recover it, since presentation is the one thing
   * with no other way of being driven.
   *
   * A timer is the right watchdog precisely because it is a different clock:
   * it survives what stopped the thing it is watching. Only while something is
   * visible to present — a hidden page is *supposed* to stop painting, and
   * restarting the loop into it would be a wasted request every tick.
   */
  #startWatchdog(): void {
    if (this.#watchdog !== null) return;
    this.#watchdog = setInterval(() => {
      if (document.visibilityState !== 'visible') return;
      if (this.#renderHandle === null) return;
      if (performance.now() - this.#lastTickMs < RENDER_STALL_MS) return;

      bridge.report('WARN', 'presentation loop stalled; restarting it', {
        sinceMs: String(Math.round(performance.now() - this.#lastTickMs)),
      });
      cancelAnimationFrame(this.#renderHandle);
      this.#renderHandle = null;
      this.#startRender();
    }, RENDER_WATCHDOG_MS);
  }

  #stopRenderIfIdle(): void {
    if (this.#renderHandle === null) return;
    for (const sink of this.#sinks.values()) {
      if (sink.kind === 'video') return;
    }
    cancelAnimationFrame(this.#renderHandle);
    this.#renderHandle = null;
    if (this.#watchdog !== null) {
      clearInterval(this.#watchdog);
      this.#watchdog = null;
    }
  }

  /** Presents whichever queued frame is due against the audio clock. */
  #present(sink: VideoSink, nowMs: number): void {
    if (sink.queue.length === 0) return;

    const clockUs = this.#clockFor(sink.track.participant, nowMs);
    let index: number;
    if (clockUs === null) {
      // The participant publishes no audio, or it has not started playing.
      // There is nothing to synchronise to, so show the newest frame — which
      // is also the lowest-latency thing to do.
      index = sink.queue.length - 1;
      sink.avOffsetMs = null;
    } else {
      index = presentIndex(sink.queue.map((frame) => frame.timestamp), clockUs);
      if (index < 0) return; // Nothing due yet; hold the queue.
      sink.avOffsetMs = offsetMillis(sink.queue[index].timestamp, clockUs);
    }

    // Frames before the chosen one have been overtaken and are never shown.
    for (let i = 0; i < index; i++) sink.queue[i].close();
    const frame = sink.queue[index];
    sink.queue.splice(0, index + 1);

    if (!sink.canvas || !sink.ctx) {
      // No tile mounted yet. Keep only the newest frame so a late-mounting
      // canvas paints something immediately instead of staying black.
      sink.pending?.close();
      sink.pending = frame;
      return;
    }
    this.#paint(sink, frame);
  }

  /**
   * The playout position of a participant's audio, projected to now, or null
   * if they have no clock.
   *
   * Looked up by participant rather than held on the video sink because the
   * two tracks are announced independently and in either order, so a
   * back-reference would need fixing up on both paths.
   */
  #clockFor(participant: string, nowMs: number): number | null {
    for (const sink of this.#sinks.values()) {
      if (sink.kind !== 'audio' || sink.track.participant !== participant) continue;
      return sink.clock ? projectClock(sink.clock, nowMs) : null;
    }
    return null;
  }

  #paint(sink: VideoSink, frame: VideoFrame): void {
    if (!sink.canvas || !sink.ctx) {
      frame.close();
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
      // The timestamp travels with the samples: it is what lets the worklet
      // report a playout position, and so what video is synchronised to.
      const chunk: PlayerChunk = { samples, timestampUs: data.timestamp };
      // Transfer rather than copy: the worklet consumes the buffer.
      sink.node.port.postMessage(chunk, [samples.buffer]);
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
        codec: sink.track.config.codec,
      };
      if (sink.kind === 'video') {
        row.width = sink.width;
        row.height = sink.height;
        row.queued = sink.queue.length;
        if (sink.avOffsetMs !== null) row.avOffsetMs = sink.avOffsetMs;
      }
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
