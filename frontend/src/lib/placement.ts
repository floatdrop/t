/**
 * Where an arriving audio chunk belongs in the playout buffer.
 *
 * Kept apart from the worklet, and free of any state, because it is the one
 * piece of the player that is a decision rather than a mechanism — and because
 * a worklet cannot be tested in this project at all. The processor runs as a
 * source string in an AudioWorkletGlobalScope; nothing in the test runner can
 * reach it. So the arithmetic lives here, is exercised here, and is injected
 * into the worklet source verbatim so that there is one implementation of it
 * rather than two that can drift.
 *
 * The problem it solves: chunks normally arrive in order and are appended, and
 * that was the only thing the player could do with one. A chunk that arrives
 * out of order was appended too — played in the wrong order, and dating every
 * sample written after it wrongly. Measured against a real relay behind a
 * bottleneck, that happened to 3% of audio chunks, the median 42 ms out of
 * place; on a healthy path it never happened at all.
 *
 * There is room to fix it for nothing. The buffer already holds 60 to 250 ms,
 * so a chunk that is late by less than that has not been played yet and its
 * place in the queue is still there to be written into. No latency is added to
 * gain the ordering — the depth was already being paid for jitter.
 */

/** What the player should do with a chunk. */
export type Placement =
  /** Append at the write cursor and advance the clock — the ordinary path. */
  | { action: 'append' }
  /** Write it this many samples behind the write cursor, over what is queued. */
  | { action: 'place'; behind: number }
  /** Already played. Nothing can be done with it. */
  | { action: 'drop' };

/**
 * Decides where a chunk goes.
 *
 * `writeUs` is the timestamp the next appended sample would carry, `available`
 * how many samples are queued and unplayed, and `length` how many samples the
 * chunk holds. `haveClock` is false until the first chunk establishes the
 * reference, when there is nothing to be out of order with.
 *
 * Only a chunk lying *entirely* behind the write cursor is placed, which is
 * what a reordered Opus packet is: they are a uniform 20 ms and an inversion
 * moves whole packets. Anything straddling the cursor is appended, exactly as
 * it was before this existed — the overlap would have to be split between
 * overwriting and appending, for a case that does not arise.
 */
export function placementFor(
  haveClock: boolean,
  writeUs: number,
  timestampUs: number,
  available: number,
  length: number,
  rate: number,
): Placement {
  if (!haveClock || typeof timestampUs !== 'number') return { action: 'append' };

  const behind = Math.round(((writeUs - timestampUs) / 1e6) * rate);
  if (behind < length) return { action: 'append' };
  // Past the oldest sample still queued: the sound it belonged in has been
  // heard, and writing it now would overwrite whatever took its place.
  if (behind > available) return { action: 'drop' };
  return { action: 'place', behind };
}
