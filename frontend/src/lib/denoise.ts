/**
 * Local noise suppression and voice-activity detection.
 *
 * # What handles what
 *
 * **Echo** is handled by the platform, not here. `getUserMedia`'s
 * `echoCancellation` constraint engages macOS's own AEC, which has access to
 * the render reference signal — what the speakers are actually playing. Code
 * running in this page does not, so a WASM echo canceller here could only
 * guess at what to subtract and would do strictly worse. `capture.ts` asks
 * for the constraint and reports what the browser actually applied.
 *
 * **Noise** is handled here, after the platform's own suppressor.
 * [RNNoise](https://github.com/xiph/rnnoise) is a small recurrent network
 * (a few hundred KB of weights) that removes the stationary noise a
 * general-purpose suppressor leaves behind — fans, hum, keyboard hiss — and
 * returns a voice-activity probability as a side effect, which is what
 * drives the speaking indicator.
 *
 * # Where it runs
 *
 * On the main thread, between the capture AudioWorklet and the AudioEncoder,
 * rather than inside the worklet. The worklet already ships its PCM to the
 * main thread (there is no MediaStreamTrackProcessor in WebKit, so that hop
 * exists regardless), and an AudioWorkletGlobalScope has neither `fetch` nor
 * `atob` to instantiate the module with. Processing costs about 1% of a core.
 *
 * The module is a ~5 MB chunk, so it is imported dynamically: startup does
 * not pay for it, and a failure to load degrades to platform-only
 * suppression plus an energy-based VAD rather than breaking capture.
 */

import { bridge } from './bridge';

/** RNNoise's fixed frame size at 48 kHz — 480 samples, 10 ms. */
export const DENOISE_FRAME = 480;

/**
 * RNNoise expects samples scaled as 16-bit PCM (±32768) rather than the
 * ±1.0 the Web Audio API produces.
 */
const PCM16_SCALE = 32768;

/** VAD probability at or above which a frame counts as speech. */
const SPEECH_THRESHOLD = 0.65;

/**
 * Voice-activity state, smoothed over time. Raw per-frame decisions flicker
 * far too fast to drive a border.
 */
export interface VoiceState {
  /** True while the participant is considered to be speaking. */
  speaking: boolean;
  /** Smoothed 0..1 loudness, for the indicator's intensity. */
  level: number;
  /** RFC 6464 byte: bit 7 voice activity, bits 0-6 magnitude in -dBov. */
  rfc6464: number;
}

/**
 * Denoiser wraps RNNoise, or stands in for it when the module could not be
 * loaded. Feed it exactly DENOISE_FRAME samples at a time.
 */
export class Denoiser {
  /** True when the RNNoise model is actually running. */
  active = false;
  /** Set when loading failed, for the debug panel. */
  loadError = '';

  #state: { processFrame(frame: Float32Array): number; destroy(): void } | null = null;
  #scaled = new Float32Array(DENOISE_FRAME);

  /** Smoothed speech probability, and the hysteresis latch it drives. */
  #probability = 0;
  #speaking = false;
  #level = 0;
  /** Frames of sub-threshold audio before the latch releases (~300 ms). */
  #holdFrames = 0;

  /**
   * Loads RNNoise. Resolves either way: a failure leaves the denoiser
   * inactive, and everything downstream keeps working on the platform's own
   * suppression with an energy-based VAD.
   */
  async load(): Promise<void> {
    if (this.#state) return;
    try {
      const { Rnnoise } = await import('@shiguredo/rnnoise-wasm');
      const rnnoise = await Rnnoise.load();
      if (rnnoise.frameSize !== DENOISE_FRAME) {
        throw new Error(`unexpected RNNoise frame size ${rnnoise.frameSize}`);
      }
      this.#state = rnnoise.createDenoiseState();
      this.active = true;
      bridge.report('INFO', 'noise suppression active', { model: 'rnnoise' });
    } catch (err) {
      this.loadError = err instanceof Error ? err.message : String(err);
      this.active = false;
      bridge.report('WARN', 'noise suppression unavailable, using platform only', {
        err: this.loadError,
      });
    }
  }

  /**
   * Denoises one frame in place and returns the resulting voice state.
   * frame must hold exactly DENOISE_FRAME samples.
   */
  process(frame: Float32Array): VoiceState {
    let probability: number;

    if (this.#state) {
      // RNNoise works on 16-bit-scaled samples and writes its output back
      // over the input, so scale in, process, scale out.
      for (let i = 0; i < DENOISE_FRAME; i++) this.#scaled[i] = frame[i] * PCM16_SCALE;
      probability = this.#state.processFrame(this.#scaled);
      for (let i = 0; i < DENOISE_FRAME; i++) frame[i] = this.#scaled[i] / PCM16_SCALE;
    } else {
      // No model: derive a probability from frame energy so the speaking
      // indicator still works. -50 dBFS is roughly speech onset for a
      // desk microphone.
      probability = rms(frame) > 0.003 ? 1 : 0;
    }

    return this.#track(probability, rms(frame));
  }

  /**
   * Smooths the raw probability into a latched speaking decision.
   *
   * Attack is fast and release is slow: a speaking border that flickers on
   * every inter-word pause is worse than none, while a late release is
   * invisible.
   */
  #track(probability: number, amplitude: number): VoiceState {
    this.#probability = this.#probability * 0.6 + probability * 0.4;
    this.#level = Math.max(this.#level * 0.85, Math.min(1, amplitude * 12));

    if (this.#probability >= SPEECH_THRESHOLD) {
      this.#speaking = true;
      this.#holdFrames = 30; // 300 ms at 10 ms per frame
    } else if (this.#holdFrames > 0) {
      this.#holdFrames--;
    } else {
      this.#speaking = false;
    }

    return {
      speaking: this.#speaking,
      level: this.#level,
      rfc6464: encodeAudioLevel(amplitude, this.#speaking),
    };
  }

  destroy(): void {
    this.#state?.destroy();
    this.#state = null;
    this.active = false;
    this.#probability = 0;
    this.#speaking = false;
    this.#level = 0;
    this.#holdFrames = 0;
  }
}

function rms(frame: Float32Array): number {
  let sum = 0;
  for (let i = 0; i < frame.length; i++) sum += frame[i] * frame[i];
  return Math.sqrt(sum / frame.length);
}

/**
 * Packs an amplitude and a voice-activity flag into the byte RFC 6464
 * defines and LOC's AudioLevel property carries: bit 7 is voice activity,
 * bits 0-6 the magnitude in -dBov where 0 is loudest and 127 is silence.
 */
export function encodeAudioLevel(amplitude: number, voiceActivity: boolean): number {
  // -dBov relative to full scale, clamped into the 7-bit range.
  const dbov = amplitude > 0 ? -20 * Math.log10(Math.min(1, amplitude)) : 127;
  const magnitude = Math.max(0, Math.min(127, Math.round(dbov)));
  return (voiceActivity ? 0x80 : 0) | magnitude;
}

/** Splits an RFC 6464 byte back into its magnitude and activity flag. */
export function decodeAudioLevel(byte: number): { level: number; voiceActivity: boolean } {
  return { level: byte & 0x7f, voiceActivity: (byte & 0x80) !== 0 };
}
