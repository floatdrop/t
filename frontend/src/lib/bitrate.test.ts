import { describe, expect, it } from 'vitest';
import { BitrateController, floorFor, rungsFor, type LinkSample } from './bitrate';

const CEILING = 1_500_000;

/**
 * One 250 ms interval of a link, as the controller would see it.
 *
 * Written in packets rather than percentages because that is the whole point of
 * the window: a percentage of forty packets is a coarse number, and the
 * controller is what decides how much history to read it over.
 */
interface Link {
  /** Packets sent in the interval. Forty is roughly 1500 kbps. */
  packets: number;
  /** Packets lost in the interval; fractional means "one every few". */
  lost: number;
  rttMs: number;
  minRttMs: number;
}

/** A link with nothing wrong with it. */
const healthy: Link = { packets: 40, lost: 0, rttMs: 30, minRttMs: 28 };

/** Losing one packet in twenty: real congestion at any window length. */
const lossy: Link = { packets: 40, lost: 2, rttMs: 30, minRttMs: 28 };

/** No loss at all, and a buffer filling in front of us. */
const queued: Link = { packets: 40, lost: 0, rttMs: 400, minRttMs: 30 };

/** Turns a link description into the cumulative counters a session reports. */
class Uplink {
  #sent = 0;
  #lost = 0;

  next(link: Link): LinkSample {
    this.#sent += link.packets;
    this.#lost += link.lost;
    return {
      packetsSent: this.#sent,
      // Whole packets: the transport cannot report a fraction of one, and
      // rounding here is what makes "one lost packet every few intervals" a
      // thing the tests can express.
      packetsLost: Math.floor(this.#lost),
      rttMs: link.rttMs,
      minRttMs: link.minRttMs,
    };
  }
}

/** Feeds n intervals of one link and returns every target asked for. */
function run(c: BitrateController, up: Uplink, link: Link, n: number): number[] {
  const changes: number[] = [];
  for (let i = 0; i < n; i++) {
    const next = c.step(up.next(link));
    if (next !== null) changes.push(next);
  }
  return changes;
}

/** A controller past its warmup, on a link that is behaving. */
function settled(ceiling = CEILING, floor?: number): [BitrateController, Uplink] {
  const c = new BitrateController(ceiling, floor);
  const up = new Uplink();
  run(c, up, healthy, 10);
  return [c, up];
}

describe('rungsFor', () => {
  it('offers only rungs the source could have used anyway', () => {
    expect(rungsFor(1_500_000).at(-1)).toBe(1_500_000);
    expect(rungsFor(1_500_000).every((r) => r <= 1_500_000)).toBe(true);
    // A screen share has its own, higher ceiling.
    expect(rungsFor(3_000_000).at(-1)).toBe(3_000_000);
  });

  it('offers nothing below the floor the picture needs', () => {
    expect(rungsFor(1_500_000, 750_000)[0]).toBe(750_000);
    expect(rungsFor(1_500_000, 750_000).every((r) => r >= 750_000)).toBe(true);
  });

  it('never returns nothing to choose from', () => {
    // Below every rung. Something has to be asked for, and the lowest rung is
    // the least wrong answer; an empty ladder would mean no bitrate.
    expect(rungsFor(1000)).toHaveLength(1);
    // A ceiling under the floor is a source worth less than its size demands.
    // Nothing to adapt between, and the ceiling is still never exceeded.
    expect(rungsFor(500_000, 1_250_000)).toEqual([500_000]);
  });
});

describe('floorFor', () => {
  it('asks for more bits the more pixels there are', () => {
    expect(floorFor(360)).toBeLessThan(floorFor(720));
    expect(floorFor(720)).toBeLessThan(floorFor(1080));
  });

  it('does not let 720p fall to a rate that only carries 360p', () => {
    // The bug this exists for: 300 kbps is a watchable 360p and a blocky 720p,
    // and the ladder ran to it whatever size was being sent.
    expect(floorFor(720)).toBeGreaterThan(floorFor(360));
    expect(floorFor(720)).toBeGreaterThanOrEqual(750_000);
  });

  it('gives an unusually tall source the tallest floor it knows', () => {
    expect(floorFor(2160)).toBe(floorFor(1080));
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
    const up = new Uplink();
    // A connection probing for capacity looks congested. Reading that as a
    // verdict would walk the rate down before the link has said anything.
    expect(run(c, up, { packets: 40, lost: 16, rttMs: 900, minRttMs: 30 }, 4)).toEqual([]);
    expect(c.target).toBe(CEILING);
  });

  it('steps down on loss', () => {
    const [c, up] = settled();
    const changes = run(c, up, lossy, 20);
    expect(changes.length).toBeGreaterThan(0);
    expect(c.target).toBeLessThan(CEILING);
  });

  it('steps down on a standing queue with no loss at all', () => {
    const [c, up] = settled();
    // The earlier warning, and the only one a deep buffer gives: nothing is
    // lost, the delay is simply growing.
    const changes = run(c, up, queued, 10);
    expect(changes.length).toBeGreaterThan(0);
    expect(c.target).toBeLessThan(CEILING);
  });

  it('does not collapse to the floor on one burst', () => {
    const [c, up] = settled();
    // Two seconds of congested samples. One overflowing buffer produces a run
    // of them, and a step per sample would cross the whole ladder.
    const changes = run(c, up, { packets: 40, lost: 4, rttMs: 500, minRttMs: 30 }, 8);
    expect(changes).toHaveLength(1);
  });

  it('holds between the two thresholds', () => {
    const [c, up] = settled();
    // Not congested enough to give up a rung, not clean enough to earn one.
    // Without this band the loop trades a keyframe for nothing every few
    // seconds.
    const middling: Link = { packets: 40, lost: 0.4, rttMs: 90, minRttMs: 30 };
    expect(run(c, up, middling, 200)).toEqual([]);
    expect(c.target).toBe(CEILING);
  });

  it('does not read a single lost packet as congestion', () => {
    const [c, up] = settled();
    // One packet out of the forty an interval carries is 2.5 per cent of that
    // interval — above the threshold for stepping down, and above the one for
    // being clean too, so a quarter-second window had no band between them at
    // all. Over the four seconds the controller actually reads, it is 0.16 per
    // cent, which is what a healthy QUIC connection looks like.
    run(c, up, { packets: 40, lost: 1, rttMs: 30, minRttMs: 28 }, 1);
    expect(run(c, up, healthy, 20)).toEqual([]);
    expect(c.target).toBe(CEILING);
  });

  it('does not step down on a lone RTT spike', () => {
    const [c, up] = settled();
    // A single sample of standing queue is a retransmit or a scheduler hiccup,
    // not a full buffer. Stepping on it cost a rung that takes ten clean
    // seconds to earn back.
    for (let i = 0; i < 20; i++) {
      run(c, up, healthy, 9);
      run(c, up, queued, 1);
    }
    expect(c.target).toBe(CEILING);
  });

  it('concludes nothing from a link that has sent almost nothing', () => {
    const c = new BitrateController(CEILING);
    const up = new Uplink();
    // Half of two packets is a loss rate on paper and no evidence in fact —
    // the same problem a single sample has, only slower. Nothing in the window
    // but this, so there is no denominator worth dividing by.
    expect(run(c, up, { packets: 2, lost: 1, rttMs: 30, minRttMs: 28 }, 20)).toEqual([]);
    expect(c.target).toBe(CEILING);
  });

  it('starts the window over when the connection does', () => {
    const [c, up] = settled();
    run(c, up, lossy, 20);
    const dropped = c.target;
    // A re-dial resets the transport's counters. Differencing across that would
    // read as a negative interval, and the history describes a link that is
    // gone either way.
    const fresh = new Uplink();
    expect(run(c, fresh, healthy, 4)).toEqual([]);
    expect(c.target).toBe(dropped);
  });

  it('climbs back, but only after a sustained clean run', () => {
    const [c, up] = settled();
    run(c, up, lossy, 40);
    const dropped = c.target;
    expect(dropped).toBeLessThan(CEILING);

    // Well short of the run required: a link that is quiet for a moment has
    // not shown anything yet.
    expect(run(c, up, healthy, 20)).toEqual([]);
    expect(c.target).toBe(dropped);

    const changes = run(c, up, healthy, 40);
    expect(changes).toHaveLength(1);
    expect(c.target).toBeGreaterThan(dropped);
  });

  it('climbs back on a link that blips once in a while', () => {
    const [c, up] = settled();
    run(c, up, lossy, 40);
    const dropped = c.target;

    // Thirty clean samples, then one congested one, repeatedly. This is what
    // an ordinary link looks like, and it used to be a rate that never
    // recovered: one blip reset the whole clean run, and the climb needed ten
    // consecutive perfect seconds.
    for (let i = 0; i < 6; i++) {
      run(c, up, healthy, 30);
      run(c, up, queued, 1);
    }
    expect(c.target).toBeGreaterThan(dropped);
  });

  it('does not let congestion every second accumulate its way back up', () => {
    const [c, up] = settled();
    run(c, up, lossy, 40);
    const dropped = c.target;

    // Three clean samples to every congested one. A link congesting this often
    // has not recovered, and the decay is set so that it loses ground at least
    // as fast as it gains it.
    for (let i = 0; i < 40; i++) {
      run(c, up, healthy, 3);
      run(c, up, queued, 1);
    }
    expect(c.target).toBeLessThanOrEqual(dropped);
  });

  it('will not climb above the ceiling however long the link stays clean', () => {
    const c = new BitrateController(CEILING);
    const up = new Uplink();
    expect(run(c, up, healthy, 2000)).toEqual([]);
    expect(c.target).toBe(CEILING);
  });

  it('will not fall below the floor however bad the link gets', () => {
    const floor = floorFor(720);
    const [c, up] = settled(CEILING, floor);
    run(c, up, { packets: 40, lost: 32, rttMs: 4000, minRttMs: 30 }, 4000);
    // The floor is the resolution's: below it the picture is blocks, and the
    // answer is fewer pixels rather than fewer bits.
    expect(c.target).toBe(floor);
  });

  it('treats an unestablished minRtt as no queue rather than a huge one', () => {
    const [c, up] = settled();
    // Before any RTT sample lands, minRtt reads zero; subtracting it would make
    // every sample look like seconds of standing queue.
    expect(run(c, up, { packets: 40, lost: 0, rttMs: 250, minRttMs: 0 }, 20)).toEqual([]);
    expect(c.target).toBe(CEILING);
  });
});
