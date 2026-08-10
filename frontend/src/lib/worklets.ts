import { bridge } from './bridge';

/**
 * AudioWorklet processors, as source strings loaded through blob URLs.
 *
 * They are strings rather than separate files because an AudioWorklet
 * module must be fetched from a URL, and a blob URL sidesteps having to
 * emit and locate a second bundle inside the WebView's custom-scheme
 * origin. Verified to work in this WebView.
 */

/**
 * pcm-tap forwards captured microphone audio to the main thread, which is how
 * this app gets PCM at all: WebKit has no MediaStreamTrackProcessor, so there
 * is no Insertable Streams path from a MediaStreamTrack to an AudioData.
 *
 * Two things it does beyond copying, both of which exist because the main
 * thread is not reliably fast enough:
 *
 * Blocks, not quanta. A render quantum is 128 samples — 2.67 ms — so forwarding
 * each one costs 375 messages a second, all of which the main thread has to
 * drain while also decoding everyone else's video. Batching to the denoiser's
 * frame size cuts that to 100.
 *
 * A capture timestamp. Without one the main thread cannot tell audio it is
 * handling promptly from audio that has been sitting in the port queue, and a
 * backlog built during a busy moment becomes permanent latency rather than a
 * transient. Measured on a real two-party call, startup jank alone put the
 * audio 600 ms behind and it never recovered. currentFrame is the authority
 * here: it counts the audio hardware's own samples, so it cannot be skewed by
 * whatever the main thread is doing.
 */
const TAP_SOURCE = `
// The denoiser's frame size, so the main thread gets whole frames and never
// has to reconcile two different block sizes.
const FRAME = 480;

class PCMTap extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buffer = new Float32Array(FRAME);
    this.length = 0;
  }

  process(inputs) {
    const channel = inputs[0] && inputs[0][0];
    if (!channel || !channel.length) return true;

    let offset = 0;
    while (offset < channel.length) {
      const take = Math.min(FRAME - this.length, channel.length - offset);
      this.buffer.set(channel.subarray(offset, offset + take), this.length);
      this.length += take;
      offset += take;
      if (this.length < FRAME) continue;

      const samples = this.buffer;
      // A fresh buffer because the old one is transferred away, not copied.
      this.buffer = new Float32Array(FRAME);
      this.length = 0;
      // currentFrame is the first sample of this quantum, so the block that
      // just finished began FRAME samples before the point we have reached.
      const startFrame = currentFrame + offset - FRAME;
      this.port.postMessage(
        { samples, captureUs: (startFrame / sampleRate) * 1e6 },
        [samples.buffer],
      );
    }
    return true;
  }
}
registerProcessor('pcm-tap', PCMTap);
`;

/**
 * pcm-player plays decoded audio from a ring buffer the main thread fills.
 *
 * A ring buffer rather than scheduling AudioBufferSourceNodes because
 * decoded Opus arrives every 20 ms: hundreds of one-shot nodes per minute
 * per participant would both allocate heavily and click at every seam.
 * Underrun outputs silence and is counted, so the debug panel can show it.
 *
 * Samples carry no timestamp. There used to be one, and a write clock that
 * resynchronised whenever a chunk did not continue the previous one — built
 * when video was scheduled against a playout position derived from it. Video
 * is no longer scheduled against anything, so the arithmetic ran on every
 * chunk and nothing read the result. A gap in the sound is audible whether or
 * not the buffer knows the timestamp either side of it: closing one needs a
 * sparse buffer, which this is not, and the ring is contiguous by
 * construction.
 */
const PLAYER_SOURCE = `
// Two seconds at 48 kHz. Large enough to absorb network jitter, small
// enough that the buffer can never hide a real stall.
const CAPACITY = 96000;

// One 20 ms Opus packet, which is the unit everything here arrives in.
const PACKET = 960;

// The depth the buffer aims to hold, and how far above it audio may pile up
// before arrivals are refused. Both follow the link rather than being fixed.
//
// A jitter buffer has to cover the longest hole the path produces: audio is
// written at 1x and read at 1x, so the only thing that empties it is a gap in
// delivery, and the only thing that overfills it is the burst that follows.
// Measured on a cellular uplink to a remote relay, this path delivers in
// bursts of 160 to 420 ms separated by holes of the same order.
//
// The pair before this was fixed at a 250 ms ceiling and a 120 ms floor, which
// left 130 ms of headroom — just under that burst size — so the buffer crossed
// its ceiling on almost every burst. One nine-minute call logged 929 of them.
// The numbers were not badly chosen; they were chosen against a link that
// behaved differently, and nothing made them follow this one.
//
// So the target is the worst hole seen in the last GAP_WINDOW seconds, plus a
// packet, and the ceiling is twice it. Those cover the two different things: the
// target is what the reader plays through a hole, while the headroom above it is
// what the burst on the other side of that hole lands in — and on a stream
// delivered at 1x the burst is the same size as the hole, which a fixed margin
// cannot follow. It starts at the floor, so a healthy call prerolls in 60 ms and
// stays there, and only a path that actually stalls pays for the depth.
//
// The target is capped well below what some links would ask for. Past 300 ms
// the buffer stops being jitter absorption and becomes the fault: a link with
// holes that long cannot carry a conversation whatever this does, and bounded
// latency is worth more there than completeness.
const MIN_TARGET = 3 * PACKET;  // 60 ms
const MAX_TARGET = 15 * PACKET; // 300 ms — beyond this, latency is the fault
const GAP_WINDOW = 10; // seconds of history the target is drawn from

// What happens at the ceiling, which is the other half of it.
//
// The old policy discarded the oldest audio — the samples about to be played —
// and did it on every push that crossed the line. During a burst that fires
// repeatedly: the listener hears 30 ms of speech, loses 130 ms, hears 30 ms.
// The trim log caught six inside one second. The same quantity of audio is lost
// either way, but one contiguous cut is intelligible and six interleaved ones
// are not.
//
// So arrivals are refused instead while the buffer is full. What is already
// queued plays through uninterrupted and the loss lands in one place. It also
// cannot run away: refusing at the ceiling bounds the depth by construction,
// which is what the trim was really for.

// How often to report buffer counters, in render quanta.
//
// It used to be eight — about 21 ms — because a render loop interpolated
// between reports to schedule video against the playout clock. Nothing is
// scheduled against it any more; the only reader left is a debug panel
// sampling a few times a second, and forty-seven messages a second per remote
// participant onto the main thread for that is a cost with nothing on the
// other side of it. Sixty-four quanta is about 170 ms, which no panel can tell
// from live.
const REPORT_EVERY = 64;

class PCMPlayer extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buffer = new Float32Array(CAPACITY);
    this.read = 0;
    this.write = 0;
    this.available = 0;
    this.underruns = 0;
    // Counted rather than done quietly: a buffer that keeps refusing audio is
    // a real fault, and silence about it is what let two seconds of delay go
    // unnoticed in the first place.
    this.discards = 0;

    // What a discard is measured by: the interval leading up to it, not the
    // depth at the moment of it.
    //
    // The depth cannot say anything, because it is pinned to the ceiling by
    // construction whatever caused it. A measurement of "every trim fired
    // between 253 and 269 ms" was once taken from the old policy and read as a
    // finding about the buffer; it was arithmetic, and a 640 ms burst and a
    // one-packet creep produced identical numbers.
    //
    // What discriminates is how much audio arrived against how much time
    // passed. Equal means the reader stalled; more arrived than elapsed means a
    // burst landed, and how much more is how big it was.
    this.pushedSince = 0;
    this.lastDiscardFrame = 0;
    this.underrunsAtDiscard = 0;
    this.sinceDiscardMs = 0;
    this.arrivedMs = 0;
    this.underrunsSinceDiscard = 0;

    // The adaptive target, and the history it is drawn from: one bucket per
    // second of GAP_WINDOW, each holding the worst delivery gap seen in it.
    this.target = MIN_TARGET;
    this.lastPushFrame = -1;
    this.gaps = new Float32Array(GAP_WINDOW);
    this.gapBucket = -1;

    this.playing = false;
    this.quanta = 0;
    this.port.onmessage = (ev) => {
      if (ev.data === 'stats') {
        this.report();
        return;
      }
      this.push(ev.data.samples);
    };
  }

  ceiling() {
    return this.target * 2;
  }

  report() {
    this.port.postMessage({
      available: this.available,
      underruns: this.underruns,
      discards: this.discards,
      targetMs: (this.target / sampleRate) * 1000,
      sinceDiscardMs: this.sinceDiscardMs,
      arrivedMs: this.arrivedMs,
      underrunsSinceDiscard: this.underrunsSinceDiscard,
    });
  }

  // trackGap sizes the target from the worst hole in delivery lately.
  //
  // The hole is what the buffer has to play through, so it is the thing worth
  // measuring — not the jitter of individual arrivals, and not the depth the
  // buffer happens to reach, which the ceiling censors. Bucketed by second so
  // one bad moment ages out after GAP_WINDOW rather than holding the target up
  // for the rest of the call.
  trackGap() {
    const now = currentFrame;
    if (this.lastPushFrame >= 0) {
      const bucket = Math.floor(now / sampleRate) % GAP_WINDOW;
      if (bucket !== this.gapBucket) {
        this.gapBucket = bucket;
        this.gaps[bucket] = 0;
      }
      const gap = now - this.lastPushFrame;
      if (gap > this.gaps[bucket]) this.gaps[bucket] = gap;

      let worst = 0;
      for (let i = 0; i < GAP_WINDOW; i++) {
        if (this.gaps[i] > worst) worst = this.gaps[i];
      }
      const want = worst + PACKET;
      this.target = Math.max(MIN_TARGET, Math.min(MAX_TARGET, want));
    }
    this.lastPushFrame = now;
  }

  push(samples) {
    this.trackGap();
    // Counted as arrived whether or not it is kept, since this is what the
    // interval measurement is about.
    this.pushedSince += samples.length;

    if (this.available + samples.length > this.ceiling()) {
      this.discard();
      return;
    }
    this.pushSamples(samples);
  }

  // discard refuses an arrival and records what led to it.
  discard() {
    this.discards++;

    // currentFrame is the audio hardware's own sample counter, so this is real
    // elapsed time and not something the main thread can skew. Zero on the
    // first discard, which has no interval behind it.
    this.sinceDiscardMs = this.lastDiscardFrame > 0
      ? ((currentFrame - this.lastDiscardFrame) / sampleRate) * 1000
      : 0;
    this.arrivedMs = (this.pushedSince / sampleRate) * 1000;
    this.underrunsSinceDiscard = this.underruns - this.underrunsAtDiscard;

    this.lastDiscardFrame = currentFrame;
    this.pushedSince = 0;
    this.underrunsAtDiscard = this.underruns;
  }

  pushSamples(samples) {
    for (let i = 0; i < samples.length; i++) {
      this.buffer[this.write] = samples[i];
      this.write = (this.write + 1) % CAPACITY;
      if (this.available < CAPACITY) {
        this.available++;
      } else {
        // Unreachable while the ceiling holds, and kept as the backstop it is:
        // the reader is behind, so the oldest sample goes rather than the ring
        // wrapping over itself.
        this.read = (this.read + 1) % CAPACITY;
      }
    }
  }

  process(_inputs, outputs) {
    const out = outputs[0][0];
    if (!out) return true;

    // Refill to the target, not to a fixed floor.
    //
    // This is the start-up preroll and the recovery from an underrun both, and
    // they want the same depth for the same reason. Resuming on 60 ms after a
    // stall put the reader three packets ahead of a path delivering in bursts
    // of eight, which guaranteed the next starvation — the buffer spent the
    // call falling over, refilling barely, and falling over again.
    if (!this.playing) {
      if (this.available < this.target) {
        out.fill(0);
        return true;
      }
      this.playing = true;
    }

    if (this.available < out.length) {
      out.fill(0);
      // One increment per starvation, not per quantum of it: this is the
      // transition, and the branch above holds until the buffer has refilled.
      this.underruns++;
      this.playing = false;
      return true;
    }
    for (let i = 0; i < out.length; i++) {
      out[i] = this.buffer[this.read];
      this.read = (this.read + 1) % CAPACITY;
      this.available--;
    }
    if (++this.quanta % REPORT_EVERY === 0) {
      this.report();
    }
    return true;
  }
}
registerProcessor('pcm-player', PCMPlayer);
`;

/** One block of captured PCM, as pcm-tap posts it. */
export interface TapBlock {
  samples: Float32Array;
  /**
   * When the block's first sample was captured, on the AudioContext's own
   * clock (the same base as `AudioContext.currentTime`), in microseconds.
   */
  captureUs: number;
}

/** What pcm-player posts back, either periodically or on a 'stats' request. */
export interface PlayerReport {
  /** Samples still queued in the ring buffer. */
  available: number;
  underruns: number;
  /** How many arrivals have been refused to bound the buffer's latency. */
  discards: number;
  /** The depth the buffer is currently aiming to hold, which follows the link. */
  targetMs: number;
  /**
   * The interval leading up to the last discard, which is what says why it
   * happened.
   *
   * `arrivedMs` against `sinceDiscardMs` is the discriminating comparison:
   * audio arriving faster than time passes is a burst, and the difference is
   * its size, while the two being equal means the reader stalled instead. The
   * depth at the moment of a discard measures nothing — it is pinned to the
   * ceiling by construction.
   */
  sinceDiscardMs: number;
  arrivedMs: number;
  underrunsSinceDiscard: number;
}

/** What pcm-player expects on its port. */
export interface PlayerChunk {
  samples: Float32Array;
}

const moduleURLs = new Map<string, string>();

function blobURL(name: string, source: string): string {
  let url = moduleURLs.get(name);
  if (!url) {
    url = URL.createObjectURL(new Blob([source], { type: 'application/javascript' }));
    moduleURLs.set(name, url);
  }
  return url;
}

/**
 * Keeps an AudioContext running, and says so when it does not.
 *
 * A context is resumed once, from the gesture that joins the room, and then
 * trusted for the rest of the call. It does not deserve that: macOS can
 * interrupt one — audio focus taken by another app, an output device removed,
 * sleep and wake — and it does not resume itself. Nothing else notices,
 * because everything downstream keeps working on a graph that is no longer
 * being rendered.
 *
 * The two contexts fail differently and both silently. The playback one is
 * shared by every remote participant, so an interruption is the whole room
 * going quiet at once while the tiles carry on painting, video no longer
 * having any dependency on audio to give the silence away. The capture one
 * stops the tap, so the microphone goes dead while the local preview and
 * everyone else's audio continue.
 *
 * Resuming needs no gesture once one has been given, so this can simply ask.
 */
export function watchAudioContext(ctx: AudioContext, role: 'capture' | 'playback'): void {
  ctx.onstatechange = () => {
    if (ctx.state === 'running' || ctx.state === 'closed') return;
    bridge.report('WARN', 'audio context stopped running; resuming it', {
      role,
      state: ctx.state,
    });
    void ctx.resume().catch((err) => {
      bridge.report('ERROR', 'audio context would not resume', {
        role,
        state: ctx.state,
        err: String(err),
      });
    });
  };
}

/** Registers the pcm-tap processor on ctx. Idempotent per context. */
export async function addTapModule(ctx: AudioContext): Promise<void> {
  await ctx.audioWorklet.addModule(blobURL('pcm-tap', TAP_SOURCE));
}

/** Registers the pcm-player processor on ctx. Idempotent per context. */
export async function addPlayerModule(ctx: AudioContext): Promise<void> {
  await ctx.audioWorklet.addModule(blobURL('pcm-player', PLAYER_SOURCE));
}
