/**
 * AudioWorklet processors, as source strings loaded through blob URLs.
 *
 * They are strings rather than separate files because an AudioWorklet
 * module must be fetched from a URL, and a blob URL sidesteps having to
 * emit and locate a second bundle inside the WebView's custom-scheme
 * origin. Verified to work in this WebView.
 */

/**
 * pcm-tap forwards every render quantum of captured microphone audio to
 * the main thread, which is how this app gets PCM at all: WebKit has no
 * MediaStreamTrackProcessor, so there is no Insertable Streams path from a
 * MediaStreamTrack to an AudioData.
 */
const TAP_SOURCE = `
class PCMTap extends AudioWorkletProcessor {
  process(inputs) {
    const channel = inputs[0] && inputs[0][0];
    if (channel && channel.length) {
      // Copy: the render buffer is reused after process returns.
      this.port.postMessage(new Float32Array(channel));
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
 */
const PLAYER_SOURCE = `
// Two seconds at 48 kHz. Large enough to absorb network jitter, small
// enough that the buffer can never hide a real stall.
const CAPACITY = 96000;

class PCMPlayer extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buffer = new Float32Array(CAPACITY);
    this.read = 0;
    this.write = 0;
    this.available = 0;
    this.underruns = 0;
    // Hold output until this much audio has queued, so playback does not
    // start on the very first packet and then immediately starve.
    this.prerollFrames = 2880; // 60 ms
    this.playing = false;
    this.port.onmessage = (ev) => {
      if (ev.data === 'stats') {
        this.port.postMessage({available: this.available, underruns: this.underruns});
        return;
      }
      this.push(ev.data);
    };
  }

  push(samples) {
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
    return true;
  }
}
registerProcessor('pcm-player', PCMPlayer);
`;

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
