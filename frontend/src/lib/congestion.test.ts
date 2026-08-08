import { describe, expect, it } from 'vitest';
import {
  CongestionDetector,
  DRIFT_CONGESTED_HOLD_MS,
  DRIFT_CONGESTED_MS_PER_SEC,
  DRIFT_QUIET_MS,
  inboundDrifts,
} from './congestion';
import type { TrackMetrics } from './protocol';

/** Comfortably over and under the line, so nothing here turns on rounding. */
const BAD = DRIFT_CONGESTED_MS_PER_SEC * 10;
const FINE = 0.1;

/**
 * Feeds samples a quarter-second apart, the rate metrics actually arrive at,
 * and returns the clock afterwards.
 */
function feed(d: CongestionDetector, drifts: number[], forMs: number, from = 0): number {
  let now = from;
  for (const end = from + forMs; now <= end; now += 250) {
    d.update(drifts, now);
  }
  return now;
}

describe('inboundDrifts', () => {
  const track = (label: string, skew?: number): TrackMetrics => ({
    label,
    kbps: 0,
    objects: 0,
    groups: 0,
    skewMillisPerSec: skew,
  });

  it('takes inbound audio and nothing else', () => {
    expect(
      inboundDrifts([
        track('in/aaa/audio', 5),
        // Our own publications describe what we send, not what we receive.
        track('out/audio', 99),
        // Video carries no reading: a keyframe takes materially longer to
        // encode than a delta, which is encoder noise rather than the network.
        track('in/aaa/video', 99),
      ]),
    ).toEqual([5]);
  });

  it('ignores a track with no reading yet', () => {
    // Absent and zero are different answers — zero means keeping up, and a
    // subscription seconds old has simply not accumulated enough history.
    expect(inboundDrifts([track('in/aaa/audio'), track('in/bbb/audio', 3)])).toEqual([3]);
  });

  it('survives a metrics message carrying no tracks', () => {
    expect(inboundDrifts(undefined)).toEqual([]);
    expect(inboundDrifts([])).toEqual([]);
  });
});

describe('CongestionDetector', () => {
  it('stays quiet on a healthy path', () => {
    const d = new CongestionDetector();
    feed(d, [FINE, FINE, FINE], DRIFT_QUIET_MS * 2);
    expect(d.congested).toBe(false);
  });

  it('does not act on drift that has not held long enough', () => {
    const d = new CongestionDetector();
    // A burst that is already draining looks exactly like the start of
    // congestion, and a layer switch costs a keyframe wait.
    feed(d, [BAD, BAD], DRIFT_CONGESTED_HOLD_MS - 500);
    expect(d.congested).toBe(false);
  });

  it('acts once the drift has held', () => {
    const d = new CongestionDetector();
    feed(d, [BAD, BAD], DRIFT_CONGESTED_HOLD_MS + 500);
    expect(d.congested).toBe(true);
  });

  it('reports the change exactly once', () => {
    const d = new CongestionDetector();
    let changes = 0;
    for (let now = 0; now <= DRIFT_CONGESTED_HOLD_MS * 3; now += 250) {
      if (d.update([BAD, BAD], now)) changes++;
    }
    expect(changes).toBe(1);
  });

  // One publisher drifting says something about their uplink, and their upload
  // does not get smaller because we subscribed to a different track. Several
  // drifting together is the one thing they have in common: us.
  it('needs more than one publisher to blame our own link', () => {
    const d = new CongestionDetector();
    feed(d, [BAD, FINE, FINE], DRIFT_CONGESTED_HOLD_MS * 3);
    expect(d.congested).toBe(false);
  });

  it('acts when several publishers drift together', () => {
    const d = new CongestionDetector();
    feed(d, [BAD, BAD, FINE], DRIFT_CONGESTED_HOLD_MS + 500);
    expect(d.congested).toBe(true);
  });

  // In a two-person call there is no second opinion to be had, and doing
  // nothing costs a call that stays broken.
  it('acts on a single publisher when it is the only one', () => {
    const d = new CongestionDetector();
    feed(d, [BAD], DRIFT_CONGESTED_HOLD_MS + 500);
    expect(d.congested).toBe(true);
  });

  it('recovers only after the full quiet period', () => {
    const d = new CongestionDetector();
    let now = feed(d, [BAD, BAD], DRIFT_CONGESTED_HOLD_MS + 500);
    expect(d.congested).toBe(true);

    now = feed(d, [FINE, FINE], DRIFT_QUIET_MS - 2000, now);
    expect(d.congested).toBe(true);

    feed(d, [FINE, FINE], 3000, now);
    expect(d.congested).toBe(false);
  });

  it('restarts the quiet period when drift returns', () => {
    const d = new CongestionDetector();
    let now = feed(d, [BAD, BAD], DRIFT_CONGESTED_HOLD_MS + 500);

    // Almost recovered, then it gets bad again: the wait starts over rather
    // than resuming, or a link that is bad half the time would flap.
    now = feed(d, [FINE, FINE], DRIFT_QUIET_MS - 1000, now);
    now = feed(d, [BAD, BAD], 1000, now);
    now = feed(d, [FINE, FINE], DRIFT_QUIET_MS - 1000, now);
    expect(d.congested).toBe(true);

    feed(d, [FINE, FINE], 2000, now);
    expect(d.congested).toBe(false);
  });

  it('restarts the hold when the drift lets up', () => {
    const d = new CongestionDetector();
    let now = feed(d, [BAD, BAD], DRIFT_CONGESTED_HOLD_MS - 500);
    now = feed(d, [FINE, FINE], 500, now);
    // The hold has to start again, or intermittent spikes would accumulate
    // into a verdict none of them earned.
    feed(d, [BAD, BAD], DRIFT_CONGESTED_HOLD_MS - 500, now);
    expect(d.congested).toBe(false);
  });

  // Everyone may have just joined, or every publisher may have gone silent.
  // Neither is evidence that the link recovered.
  it('holds its verdict when there is nothing to measure', () => {
    const d = new CongestionDetector();
    let now = feed(d, [BAD, BAD], DRIFT_CONGESTED_HOLD_MS + 500);
    expect(d.congested).toBe(true);

    for (const end = now + DRIFT_QUIET_MS * 2; now <= end; now += 250) {
      expect(d.update([], now)).toBe(false);
    }
    expect(d.congested).toBe(true);
  });

  it('forgets everything on reset', () => {
    const d = new CongestionDetector();
    feed(d, [BAD, BAD], DRIFT_CONGESTED_HOLD_MS + 500);
    expect(d.congested).toBe(true);

    d.reset();
    expect(d.congested).toBe(false);
    // And the hold starts from scratch rather than from the old timestamps,
    // which would make the next session act on the previous one's history.
    feed(d, [BAD, BAD], DRIFT_CONGESTED_HOLD_MS - 500);
    expect(d.congested).toBe(false);
  });

  it('is not tripped by a reading exactly on the threshold', () => {
    const d = new CongestionDetector();
    feed(d, [DRIFT_CONGESTED_MS_PER_SEC, DRIFT_CONGESTED_MS_PER_SEC], DRIFT_CONGESTED_HOLD_MS * 2);
    expect(d.congested).toBe(false);
  });
});
