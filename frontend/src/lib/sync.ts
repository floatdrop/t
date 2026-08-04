/**
 * Presentation timing — the arithmetic behind lip sync, kept apart from the
 * WebCodecs and Web Audio objects it drives so it can be reasoned about and
 * tested on its own.
 *
 * Audio is the master clock. It has to be: the output device consumes samples
 * at its own rate and cannot be told to wait, whereas a video frame can be
 * held for a few milliseconds and painted when its time comes. So the audio
 * ring buffer reports what it is currently playing, and video is scheduled
 * against that.
 *
 * Every timestamp here is microseconds on the *publishing* participant's
 * clock, which is what LOC carries. Two participants' clocks are unrelated,
 * so each is synchronised independently; comparing timestamps across
 * participants would be meaningless.
 */

/** A clock reading, and when locally it was taken. */
export interface ClockSample {
  /** Playout position in publisher microseconds. */
  playoutUs: number;
  /** performance.now() in milliseconds when this was reported. */
  atMs: number;
}

/**
 * Extrapolates a clock reading to now.
 *
 * The audio worklet reports every ~20 ms while video is presented every
 * animation frame, so readings have to be projected forward. Audio advances
 * in real time by definition — it is being played — so elapsed wall time is
 * exactly the right amount to add.
 */
export function projectClock(sample: ClockSample, nowMs: number): number {
  return sample.playoutUs + Math.max(0, nowMs - sample.atMs) * 1000;
}

/**
 * How far behind the clock a frame may be and still be worth showing.
 *
 * A frame later than this means we are not keeping up, and freezing on an old
 * picture is worse than showing a late one, so it is presented rather than
 * held back.
 */
export const MAX_LATE_US = 200_000;

/**
 * Chooses which queued frame to paint.
 *
 * timestamps must be ascending. Returns the index of the frame to present, or
 * -1 to present nothing yet. Everything before the returned index is stale and
 * the caller should discard it.
 */
export function presentIndex(timestamps: readonly number[], clockUs: number): number {
  if (timestamps.length === 0) return -1;

  // The newest frame that is already due.
  let due = -1;
  for (let i = 0; i < timestamps.length; i++) {
    if (timestamps[i] <= clockUs) due = i;
    else break;
  }
  if (due >= 0) return due;

  // Nothing is due. Normally that means waiting — but if the whole queue is
  // far in the future the clock and the stream disagree about the epoch
  // (a publisher reconnected, say), and waiting would freeze forever, so
  // fall back to the oldest frame and let the clock catch up.
  if (timestamps[0] - clockUs > MAX_LATE_US) return 0;
  return -1;
}

/**
 * Reports the audio/video offset for a presented frame: positive means the
 * picture is ahead of the sound. Diagnostic only — nothing is corrected from
 * it — but without it a sync bug is invisible, which is how the app shipped
 * with no sync at all.
 */
export function offsetMillis(frameUs: number, clockUs: number): number {
  return (frameUs - clockUs) / 1000;
}
