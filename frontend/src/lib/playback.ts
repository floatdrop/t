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
 * Shortest gap between rebuilds of one decoder.
 *
 * A decoder error is terminal, so recovery means building a new one — and a
 * stream this decoder cannot handle would be rebuilt for every frame that
 * arrives if nothing paced it. That used to be answered with an allowance of
 * five attempts, after which the participant's video was given up on for the
 * rest of the call: a permanently blank tile that no amount of the link
 * recovering could bring back, from a burst of five errors that may have lasted
 * a second.
 *
 * A link that cannot carry video right now is not a link that will never carry
 * it, so the allowance is gone and this paces the retries instead. It keeps
 * trying for as long as the track exists, once a second, which costs nothing
 * against a decoder that will never work and costs a second against one that
 * only needed the network to come back.
 */
const DECODER_REBUILD_INTERVAL_MS = 1000;

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

/**
 * Shortest gap between canvas resizes, per tile.
 *
 * Setting width or height on a canvas is not an assignment: the specification
 * requires the bitmap and the context to be cleared and reinitialised, and in
 * WebKit that can mean releasing and reallocating a GPU-backed surface. One of
 * those per size change is unavoidable and cheap. One per *frame* is a picture
 * that spends most of its life cleared, which is what a frozen or flickering
 * tile looks like from the outside.
 *
 * Nothing in this app is supposed to change size per frame — the guard below
 * only fires when the dimensions really differ — but the size is chosen by a
 * publisher this client does not control, and a publisher whose Auto
 * resolution oscillates, or whose layer flips, would drive it. The first
 * change is taken immediately, so a genuine switch is not delayed; a second
 * within this window waits, and the frame is scaled into the old backing store
 * meanwhile, which is a slightly soft picture rather than a cleared one.
 */
const CANVAS_RESIZE_INTERVAL_MS = 500;

/**
 * Shortest gap between underrun warnings for one participant.
 *
 * Each underrun is one distinct starvation, not one per render quantum of it:
 * the counter sits after the preroll branch in pcm-player's process(), so it
 * increments only on the transition into starvation and not again until the
 * buffer has refilled and drained a second time. A count of 180 is 180 audible
 * gaps.
 *
 * This throttles the log rather than the counter, and only because a bad
 * stretch produces genuinely many of them. The number in the line is what to
 * read; `since` says how many it stands for.
 */
const UNDERRUN_LOG_INTERVAL_MS = 5000;


/** The 2D context a tile is drawn into. */
function painterFor(canvas: HTMLCanvasElement): Painter | null {
  return canvas.getContext('2d');
}

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
  /**
   * Video only: how many times this tile's canvas has been resized.
   *
   * Expected to be one per resolution the publisher has sent — a layer switch
   * or an Auto change. A number that climbs with the frame count means
   * something is driving a size change per frame, which clears the bitmap
   * every time and is what a flickering or frozen tile looks like.
   */
  resizes?: number;
  /** Codec string the decoder was configured with. */
  codec: string;
  /** Video only: frames decoded but not yet painted. */
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

/**
 * How a tile is painted: a 2D context, drawn into synchronously.
 *
 * There used to be a choice here, preferring an ImageBitmapRenderingContext
 * on the reasoning that transferring a bitmap beats blitting a frame and that
 * WebKit has a history of not drawing a WebCodecs VideoFrame into a 2D context
 * well. It was worth trying and it did not pay: tiles still froze, and the
 * asynchronous conversion it required brought a latch, an expiry, a sequence
 * number and a skip counter with it. Measured against the thing it was meant
 * to fix, it fixed nothing.
 */
type Painter = CanvasRenderingContext2D;

interface VideoSink {
  kind: 'video';
  track: RemoteTrack;
  decoder: VideoDecoder;
  canvas: HTMLCanvasElement | null;
  painter: Painter | null;
  /** Latest decoded frame, held until a canvas is attached to paint it. */
  pending: VideoFrame | null;
  /** H.264 cannot start on a delta frame; gate until the first keyframe. */
  sawKeyFrame: boolean;
  decoded: number;
  dropped: number;
  /** When the canvas was last resized, and how often — see the interval. */
  lastResizeMs: number;
  resizes: number;
  /** Latched once a paint has failed, so the warning is not repeated. */
  paintFailed: boolean;
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
   * Rebuilds since the last frame came out, reset by output. Reported in the
   * log so a decoder that keeps failing is visible; nothing bounds it any more.
   */
  restarts: number;
  /** A rebuild waiting out its interval, so a failing decoder is rebuilt once
   * rather than once per frame. */
  rebuildTimer: ReturnType<typeof setTimeout> | null;
  /**
   * Decoded frames awaiting their presentation time, oldest first. Painting
   * on decode is what left the picture unsynchronised: it ran as fast as the
   * decoder emitted, with nothing tying it to the sound.
   */
  queue: VideoFrame[];
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
  /** Rebuilds since the last frame came out. See the video sink. */
  restarts: number;
  rebuildTimer: ReturnType<typeof setTimeout> | null;
  /** Trims seen so far, so only an increase is reported. */
  trimmed: number;
  /** When an underrun was last logged, so a run of them is not a run of logs. */
  lastUnderrunLogMs: number;
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

  /** Replaces a decoder that has failed. It keeps trying for as long as the
   * track exists — see DECODER_REBUILD_INTERVAL_MS. */
  #onVideoDecoderError(sink: VideoSink, err: unknown): void {
    if (this.#sinks.get(sink.track.handle) !== sink) return; // already retired
    // One rebuild in flight is enough: a dying decoder reports per frame, and
    // every one of those errors is the same fault.
    if (sink.rebuildTimer !== null) return;

    sink.restarts++;
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

    sink.rebuildTimer = setTimeout(() => {
      sink.rebuildTimer = null;
      if (this.#sinks.get(sink.track.handle) !== sink) return;
      if (!this.#buildVideoDecoder(sink)) {
        // Configure refused the description outright, which retrying with the
        // same one cannot fix. This is the only way video is given up on now.
        this.#sinks.delete(sink.track.handle);
        this.onFailure?.(sink.track.participant, 'their video cannot be decoded');
      }
    }, DECODER_REBUILD_INTERVAL_MS);
  }

  #addVideo(track: RemoteTrack): void {
    const sink: VideoSink = {
      kind: 'video',
      track,
      canvas: null,
      painter: null,
      pending: null,
      sawKeyFrame: false,
      decoded: 0,
      dropped: 0,
      lastResizeMs: 0,
      resizes: 0,
      paintFailed: false,
      width: 0,
      height: 0,
      queue: [],
      restarts: 0,
      rebuildTimer: null,
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
      lastUnderrunLogMs: 0,
      restarts: 0,
      rebuildTimer: null,
      config: null as unknown as AudioDecoderConfig,
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
      // Said out loud, and only on the edge: one trim is a transient the
      // listener hears as a skip, but a stream of them means this participant's
      // audio is arriving faster than it can be played and something upstream
      // is wrong. Silence about it is what hid two seconds of delay.
      if (ev.data.trimmed > sink.trimmed) {
        sink.trimmed = ev.data.trimmed;
        const arrived = Math.round(ev.data.arrivedMs);
        const elapsed = Math.round(ev.data.sinceTrimMs);
        bridge.report('WARN', 'trimmed the audio buffer to bound its latency', {
          participant: track.participant,
          trims: String(sink.trimmed),
          // The interval that filled it, not the depth it reached. The depth is
          // within one 20 ms packet of the ceiling every single time, so it
          // said nothing; these two say whether a burst landed or the reader
          // stalled, and how big the burst was.
          arrivedMs: String(arrived),
          overMs: String(elapsed > 0 ? arrived - elapsed : 0),
          sinceLastTrimMs: String(elapsed),
          underrunsSince: String(ev.data.underrunsSinceTrim),
        });
      }
      // Counted since the beginning and never once said out loud, which is why
      // every trim in the logs looked unexplained. An underrun is what turns an
      // upstream stall into a burst: the reader stops taking samples until the
      // preroll has rebuilt, so nothing drains while the backlog lands, and the
      // trim that follows is the second half of one event rather than a
      // separate fault. Throttled, because a bad patch produces a run of them
      // and the count is the interesting part.
      if (ev.data.underruns > sink.underruns) {
        const now = performance.now();
        const missed = ev.data.underruns - sink.underruns;
        sink.underruns = ev.data.underruns;
        if (now - sink.lastUnderrunLogMs >= UNDERRUN_LOG_INTERVAL_MS) {
          sink.lastUnderrunLogMs = now;
          bridge.report('WARN', 'audio underran; the buffer is refilling', {
            participant: track.participant,
            underruns: String(sink.underruns),
            since: String(missed),
            bufferedMs: String(Math.round((ev.data.available / 48000) * 1000)),
          });
        }
      }
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
    if (sink.rebuildTimer !== null) return;

    sink.restarts++;
    bridge.report('WARN', 'audio decoder failed; rebuilding it', {
      participant: sink.track.participant,
      restarts: String(sink.restarts),
      err: String(err),
    });
    sink.rebuildTimer = setTimeout(() => {
      sink.rebuildTimer = null;
      if (this.#sinks.get(sink.track.handle) !== sink) return;
      if (!this.#buildAudioDecoder(sink)) {
        this.#sinks.delete(sink.track.handle);
        this.onFailure?.(sink.track.participant, 'their audio cannot be decoded');
      }
    }, DECODER_REBUILD_INTERVAL_MS);
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
    // A rebuild scheduled for a track that has gone would build a decoder
    // nothing will ever feed, and hold the sink alive to do it.
    if (sink.rebuildTimer !== null) {
      clearTimeout(sink.rebuildTimer);
      sink.rebuildTimer = null;
    }

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
    sink.painter = canvas ? painterFor(canvas) : null;
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
        if (sink.kind === 'video') this.#present(sink);
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

  /**
   * Presents the newest decoded frame, once per display refresh.
   *
   * Video used to be scheduled against the participant's audio playout clock,
   * so a frame waited until its timestamp came due. That bought lip sync and
   * charged for it in coupling: audio that stalled, starved or stopped
   * reporting took the picture with it, and a fault in either medium showed up
   * as a fault in both. They are independent now — video is painted as fast as
   * it decodes, sound played as fast as it arrives, neither waiting on the
   * other.
   *
   * What that gives up is real. Nothing aligns the two timelines any more, and
   * before there was any synchronisation the picture led the sound by around
   * two thirds of a second. This is a deliberate step back from that, to make
   * each medium diagnosable without the other.
   *
   * Newest rather than every frame, and still on the display's clock: a
   * backfilled group arrives as a burst, and painting each frame as it decoded
   * would run a second of video past in a few milliseconds. Showing the newest
   * per refresh skips instead of fast-forwarding.
   */
  #present(sink: VideoSink): void {
    if (sink.queue.length === 0) return;

    const index = sink.queue.length - 1;

    // Frames before the chosen one have been overtaken and are never shown.
    for (let i = 0; i < index; i++) sink.queue[i].close();
    const frame = sink.queue[index];
    sink.queue.splice(0, index + 1);

    if (!sink.canvas || !sink.painter) {
      // No tile mounted yet. Keep only the newest frame so a late-mounting
      // canvas paints something immediately instead of staying black.
      sink.pending?.close();
      sink.pending = frame;
      return;
    }
    this.#paint(sink, frame);
  }

  #paint(sink: VideoSink, frame: VideoFrame): void {
    const painter = sink.painter;
    if (!sink.canvas || !painter) {
      frame.close();
      return;
    }

    // Sized from the frame either way.
    //
    // transferFromImageBitmap is specified to give the canvas the bitmap's
    // dimensions, and relying on that was wrong: the element keeps its default
    // 300x150 here, so a sixteen-by-nine picture was being drawn into a two-by-
    // one buffer and came out stretched sideways. The frame carries the size
    // synchronously, before any conversion, so it costs nothing to say it.
    if (sink.canvas.width !== frame.displayWidth || sink.canvas.height !== frame.displayHeight) {
      const now = performance.now();
      if (now - sink.lastResizeMs >= CANVAS_RESIZE_INTERVAL_MS) {
        sink.canvas.width = frame.displayWidth;
        sink.canvas.height = frame.displayHeight;
        sink.lastResizeMs = now;
        sink.resizes++;
      }
    }

    try {
      // Drawn straight from the decoder's frame, synchronously, in this tick.
      //
      // There used to be a bitmaprenderer path here: convert to an ImageBitmap
      // and transfer that into the canvas, which is the faster of the two on
      // paper. It was reached asynchronously, so it needed a one-at-a-time
      // latch, an expiry for when the promise never came back, a sequence
      // number so an abandoned conversion could not paint over a fresh one,
      // and a counter for the frames it dropped meanwhile — four mechanisms
      // whose shared failure was a frozen tile. It did not stop tiles
      // freezing, so what it bought did not cover what it cost.
      //
      // Synchronous drawing has no such states: the frame is drawn and closed
      // in the same tick, which is also what makes its lifetime obvious rather
      // than a question about which conversion won a race.
      //
      // Scaled to whatever the backing store currently is, rather than drawn
      // at the frame's own size. They are the same whenever the resize above
      // was taken, and while one is being held this is what keeps the picture
      // filling the tile instead of being cropped into a canvas that has not
      // caught up yet.
      painter.drawImage(frame, 0, 0, sink.canvas.width, sink.canvas.height);
    } catch (err) {
      this.#paintFailed(sink, err);
    } finally {
      frame.close();
    }
  }


  #paintFailed(sink: VideoSink, err: unknown): void {
    if (sink.paintFailed) return;
    sink.paintFailed = true;
    bridge.report('WARN', 'canvas would not accept a decoded frame', {
      participant: sink.track.participant,
      handle: String(sink.track.handle),
      err: String(err),
    });
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
      const chunk: PlayerChunk = { samples };
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
        row.resizes = sink.resizes;
        row.queued = sink.queue.length;
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
