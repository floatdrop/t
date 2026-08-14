import { describe, expect, it } from 'vitest';
import { BitrateController, rungsFor, type LinkSample } from './bitrate';

const CEILING = 1_500_000;

/** A sample of a link with nothing wrong with it. */
const healthy: LinkSample = { lossPercent: 0, rttMs: 30, minRttMs: 28 };

/** Feeds n samples and returns every target the controller asked for. */
function run(c: BitrateController, sample: LinkSample, n: number): number[] {
  const changes: number[] = [];
  for (let i = 0; i < n; i++) {
    const next = c.step(sample);
    if (next !== null) changes.push(next);
  }
  return changes;
}

describe('rungsFor', () => {
  it('offers only rungs the source could have used anyway', () => {
    expect(rungsFor(1_500_000).at(-1)).toBe(1_500_000);
    expect(rungsFor(1_500_000).every((r) => r <= 1_500_000)).toBe(true);
    // A screen share has its own, higher ceiling.
    expect(rungsFor(3_000_000).at(-1)).toBe(3_000_000);
  });

  it('never returns nothing to choose from', () => {
    // Below every rung. Something has to be asked for, and the floor is the
    // least wrong answer; returning an empty ladder would mean no bitrate.
    expect(rungsFor(1000)).toHaveLength(1);
  });
});

describe('BitrateController', () => {
  it('starts at the ceiling', () => {
    // What this app sent before there was a controller. A link carrying it
    // should not have to climb back to where it already was.
    expect(new BitrateController(CEILING).target).toBe(CEILING);
  });

  it('ignores the first samples of a connection', () => {
    const c = new BitrateController(CEILING);
    // A connection probing for capacity looks congested. Reading that as a
    // verdict would walk the rate down before the link has said anything.
    expect(run(c, { lossPercent: 40, rttMs: 900, minRttMs: 30 }, 4)).toEqual([]);
    expect(c.target).toBe(CEILING);
  });

  it('steps down on loss', () => {
    const c = new BitrateController(CEILING);
    run(c, healthy, 10); // clear the warmup
    const changes = run(c, { lossPercent: 5, rttMs: 30, minRttMs: 28 }, 10);
    expect(changes.length).toBeGreaterThan(0);
    expect(c.target).toBeLessThan(CEILING);
  });

  it('steps down on a standing queue with no loss at all', () => {
    const c = new BitrateController(CEILING);
    run(c, healthy, 10);
    // The earlier warning, and the only one a deep buffer gives: nothing is
    // lost, the delay is simply growing.
    const changes = run(c, { lossPercent: 0, rttMs: 400, minRttMs: 30 }, 10);
    expect(changes.length).toBeGreaterThan(0);
    expect(c.target).toBeLessThan(CEILING);
  });

  it('does not collapse to the floor on one burst', () => {
    const c = new BitrateController(CEILING);
    run(c, healthy, 10);
    // Two seconds of congested samples. One overflowing buffer produces a run
    // of them, and a step per sample would cross the whole ladder.
    const changes = run(c, { lossPercent: 9, rttMs: 500, minRttMs: 30 }, 8);
    expect(changes).toHaveLength(1);
  });

  it('holds between the two thresholds', () => {
    const c = new BitrateController(CEILING);
    run(c, healthy, 10);
    // Not congested enough to give up a rung, not clean enough to earn one.
    // Without this band the loop trades a keyframe for nothing every few
    // seconds.
    const middling: LinkSample = { lossPercent: 1, rttMs: 90, minRttMs: 30 };
    expect(run(c, middling, 200)).toEqual([]);
    expect(c.target).toBe(CEILING);
  });

  it('climbs back, but only after a sustained clean run', () => {
    const c = new BitrateController(CEILING);
    run(c, healthy, 10);
    run(c, { lossPercent: 9, rttMs: 30, minRttMs: 28 }, 40);
    const dropped = c.target;
    expect(dropped).toBeLessThan(CEILING);

    // Well short of the run required: a link that is quiet for a moment has
    // not shown anything yet.
    expect(run(c, healthy, 20)).toEqual([]);
    expect(c.target).toBe(dropped);

    const changes = run(c, healthy, 40);
    expect(changes).toHaveLength(1);
    expect(c.target).toBeGreaterThan(dropped);
  });

  it('will not climb above the ceiling however long the link stays clean', () => {
    const c = new BitrateController(CEILING);
    expect(run(c, healthy, 2000)).toEqual([]);
    expect(c.target).toBe(CEILING);
  });

  it('will not fall below the floor however bad the link gets', () => {
    const c = new BitrateController(CEILING);
    run(c, healthy, 10);
    const floor = rungsFor(CEILING)[0];
    run(c, { lossPercent: 80, rttMs: 4000, minRttMs: 30 }, 4000);
    expect(c.target).toBe(floor);
  });

  it('does not let intermittent congestion accumulate its way back up', () => {
    const c = new BitrateController(CEILING);
    run(c, healthy, 10);
    run(c, { lossPercent: 9, rttMs: 30, minRttMs: 28 }, 10);
    const dropped = c.target;

    // Thirty clean samples then one bad one, repeatedly. The clean run is
    // broken every time, so this never earns the climb — which is the point: a
    // link that congests every few seconds has not recovered.
    for (let i = 0; i < 20; i++) {
      run(c, healthy, 30);
      c.step({ lossPercent: 9, rttMs: 30, minRttMs: 28 });
    }
    expect(c.target).toBeLessThanOrEqual(dropped);
  });

  it('treats an unestablished minRtt as no queue rather than a huge one', () => {
    const c = new BitrateController(CEILING);
    run(c, healthy, 10);
    // Before any RTT sample lands, minRtt reads zero; subtracting it would make
    // every sample look like seconds of standing queue.
    expect(run(c, { lossPercent: 0, rttMs: 250, minRttMs: 0 }, 20)).toEqual([]);
    expect(c.target).toBe(CEILING);
  });
});
