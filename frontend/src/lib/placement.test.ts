import { describe, expect, it } from 'vitest';
import { placementFor } from './placement';

const RATE = 48_000;
const CHUNK = 960; // 20 ms of Opus at 48 kHz
const CHUNK_US = 20_000;

/** The buffer's state when the next appended sample would carry writeUs. */
function at(writeUs: number, availableMs: number) {
  return { writeUs, available: (availableMs / 1000) * RATE };
}

describe('placementFor', () => {
  it('appends the chunk that continues the stream', () => {
    const { writeUs, available } = at(1_000_000, 120);
    expect(placementFor(true, writeUs, writeUs, available, CHUNK, RATE))
      .toEqual({ action: 'append' });
  });

  it('appends before a clock reference exists', () => {
    // Nothing has been written, so nothing can be out of order with it.
    expect(placementFor(false, 0, 5_000_000, 0, CHUNK, RATE))
      .toEqual({ action: 'append' });
  });

  it('places a chunk that arrived one packet late', () => {
    // The write cursor has moved a packet past this chunk's timestamp and the
    // buffer still holds 120 ms, so its slot is there to be written into.
    const { writeUs, available } = at(1_000_000, 120);
    expect(placementFor(true, writeUs, writeUs - CHUNK_US, available, CHUNK, RATE))
      .toEqual({ action: 'place', behind: CHUNK });
  });

  it('places a chunk that is late by nearly the whole buffer', () => {
    const { writeUs, available } = at(1_000_000, 120);
    const behindUs = 100_000; // 100 ms of a 120 ms queue
    expect(placementFor(true, writeUs, writeUs - behindUs, available, CHUNK, RATE))
      .toEqual({ action: 'place', behind: (behindUs / 1e6) * RATE });
  });

  it('drops a chunk the buffer has already played past', () => {
    // 200 ms late against 120 ms of queue: the sound it belonged in has been
    // heard, and writing it would overwrite whatever took its place.
    const { writeUs, available } = at(1_000_000, 120);
    expect(placementFor(true, writeUs, writeUs - 200_000, available, CHUNK, RATE))
      .toEqual({ action: 'drop' });
  });

  it('appends a chunk that straddles the write cursor', () => {
    // Half a packet behind: the overlap would have to be split between
    // overwriting and appending, and a reordered Opus packet never lands here.
    const { writeUs, available } = at(1_000_000, 120);
    expect(placementFor(true, writeUs, writeUs - CHUNK_US / 2, available, CHUNK, RATE))
      .toEqual({ action: 'append' });
  });

  it('appends a chunk from the future, leaving the resync to the player', () => {
    // A publisher whose epoch jumped. Placement has nothing to say about that;
    // the player's own RESYNC_US rule does.
    const { writeUs, available } = at(1_000_000, 120);
    expect(placementFor(true, writeUs, writeUs + 5_000_000, available, CHUNK, RATE))
      .toEqual({ action: 'append' });
  });

  // worklets.ts injects this function into the player's source string by
  // stringifying it, because an AudioWorklet cannot import a module. That only
  // works while the compiled function stands on its own — a bundler that
  // hoisted a helper or a constant out of it would produce a worklet that
  // throws on load, in the audio path, at runtime, where nothing here would
  // see it.
  it('still works when stringified into a standalone source', () => {
    const injected = new Function(
      `const placementFor = ${placementFor.toString()}; return placementFor;`,
    )() as typeof placementFor;

    const { writeUs, available } = at(1_000_000, 120);
    expect(injected(true, writeUs, writeUs - CHUNK_US, available, CHUNK, RATE))
      .toEqual({ action: 'place', behind: CHUNK });
    expect(injected(true, writeUs, writeUs - 200_000, available, CHUNK, RATE))
      .toEqual({ action: 'drop' });
    expect(injected(true, writeUs, writeUs, available, CHUNK, RATE))
      .toEqual({ action: 'append' });
  });

  it('drops rather than places when the buffer has run dry', () => {
    // Nothing queued: every sample written has been played, so a late chunk
    // has nowhere to go.
    expect(placementFor(true, 1_000_000, 1_000_000 - CHUNK_US, 0, CHUNK, RATE))
      .toEqual({ action: 'drop' });
  });
});
