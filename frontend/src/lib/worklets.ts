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
 * pcm-player plays decoded audio from a ring buffer the main thread fills,
 * and reports what it is currently playing so video can be synchronised to
 * it.
 *
 * A ring buffer rather than scheduling AudioBufferSourceNodes because
 * decoded Opus arrives every 20 ms: hundreds of one-shot nodes per minute
 * per participant would both allocate heavily and click at every seam.
 * Underrun outputs silence and is counted, so the debug panel can show it.
 *
 * The playout clock needs only one scalar. Buffered audio is contiguous, so
 * the timestamp of the sample about to be *heard* is the timestamp of the
 * sample about to be *written* minus everything still queued. A gap — a lost
 * packet — breaks that assumption, so an incoming chunk that does not
 * continue the previous one resets the reference instead of mis-dating every
 * sample after it.
 */
const PLAYER_SOURCE = `
// Two seconds at 48 kHz. Large enough to absorb network jitter, small
// enough that the buffer can never hide a real stall.
const CAPACITY = 96000;

// How much audio may queue before the buffer is trimmed back, and what it is
// trimmed back to.
//
// Without this the buffer had no way back down. Nothing drains it: the reader
// takes exactly one sample per output sample, so any moment where the writer
// got ahead — a stall in the audio thread, a burst off the network, a decoder
// backlog flushing — stayed ahead for the rest of the call. Measured on a
// five-way call, every participant's buffer sat pinned at CAPACITY, which is
// two full seconds of delay between someone speaking and anyone hearing it.
// The overrun path in pushSamples does not help: by the time it engages the
// latency is already the whole buffer, and dropping one sample per sample
// written is exactly what holds it there.
//
// So the queue is bounded instead, on the same reasoning capture.ts drops
// audio that is already late: a gap is audible once, and permanent latency is
// audible for the rest of the call. The high-water mark is four times the
// preroll, so ordinary jitter never reaches it.
const MAX_BUFFER = 12000; // 250 ms
const TRIM_TO = 2880;     // 60 ms, the preroll depth

// A chunk whose timestamp misses the expected one by more than this is
// treated as a new reference rather than a continuation.
const RESYNC_US = 5000;

// How often to report the playout clock, in render quanta. Eight quanta at
// 48 kHz is about 21 ms — often enough for a 60 Hz render loop to interpolate
// between reports without noticeable error.
const REPORT_EVERY = 8;

class PCMPlayer extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buffer = new Float32Array(CAPACITY);
    this.read = 0;
    this.write = 0;
    this.available = 0;
    this.underruns = 0;
    // Counted rather than done quietly: a buffer that keeps needing trimming
    // is a real fault, and silence about it is what let two seconds of delay
    // go unnoticed in the first place.
    this.trimmed = 0;
    // Hold output until this much audio has queued, so playback does not
    // start on the very first packet and then immediately starve.
    this.prerollFrames = 2880; // 60 ms
    this.playing = false;
    // Publisher-clock timestamp of the next sample to be written.
    this.writeUs = 0;
    this.haveClock = false;
    this.quanta = 0;
    this.port.onmessage = (ev) => {
      if (ev.data === 'stats') {
        this.report();
        return;
      }
      this.push(ev.data.samples, ev.data.timestampUs);
    };
  }

  // playoutUs is the timestamp of the sample about to leave the buffer.
  report() {
    this.port.postMessage({
      available: this.available,
      underruns: this.underruns,
      trimmed: this.trimmed,
      playing: this.playing,
      haveClock: this.haveClock,
      playoutUs: this.writeUs - (this.available / sampleRate) * 1e6,
    });
  }

  push(samples, timestampUs) {
    if (typeof timestampUs === 'number') {
      if (!this.haveClock || Math.abs(timestampUs - this.writeUs) > RESYNC_US) {
        this.writeUs = timestampUs;
        this.haveClock = true;
      }
    }
    this.writeUs += (samples.length / sampleRate) * 1e6;
    this.pushSamples(samples);
    this.trim();
  }

  // Drops the oldest audio when too much has queued, which is the only thing
  // that actually shortens the queue. The playout clock needs no correction:
  // it is derived from what is still buffered, so skipping ahead in the buffer
  // is skipping ahead in time, and the video scheduled against it follows.
  trim() {
    if (this.available <= MAX_BUFFER) return;
    const drop = this.available - TRIM_TO;
    this.read = (this.read + drop) % CAPACITY;
    this.available -= drop;
    this.trimmed++;
  }

  pushSamples(samples) {
    for (let i = 0; i < samples.length; i++) {
      this.buffer[this.write] = samples[i];
      this.write = (this.write + 1) % CAPACITY;
      if (this.available < CAPACITY) {
        this.available++;
      } else {
        // Overrun: the reader is behind. Drop the oldest sample so the
        // buffer tracks the live edge rather than accumulating delay.
        this.read = (this.read + 1) % CAPACITY;
      }
    }
  }

  process(_inputs, outputs) {
    const out = outputs[0][0];
    if (!out) return true;

    if (!this.playing) {
      if (this.available < this.prerollFrames) {
        out.fill(0);
        return true;
      }
      this.playing = true;
    }

    if (this.available < out.length) {
      out.fill(0);
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
  /** How many times the queue has been trimmed back to bound its latency. */
  trimmed: number;
  /** False while prerolling or after a starve, when the clock is not moving. */
  playing: boolean;
  /** False until a timestamped chunk has arrived. */
  haveClock: boolean;
  /** Publisher-clock timestamp of the sample about to be heard. */
  playoutUs: number;
}

/** What pcm-player expects on its port. */
export interface PlayerChunk {
  samples: Float32Array;
  /** Publisher-clock timestamp of the first sample, in microseconds. */
  timestampUs: number;
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

/** Registers the pcm-tap processor on ctx. Idempotent per context. */
export async function addTapModule(ctx: AudioContext): Promise<void> {
  await ctx.audioWorklet.addModule(blobURL('pcm-tap', TAP_SOURCE));
}

/** Registers the pcm-player processor on ctx. Idempotent per context. */
export async function addPlayerModule(ctx: AudioContext): Promise<void> {
  await ctx.audioWorklet.addModule(blobURL('pcm-player', PLAYER_SOURCE));
}
