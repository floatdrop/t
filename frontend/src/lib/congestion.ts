/**
 * Deciding whether the inbound path is over capacity.
 *
 * Kept apart from the store, and free of runes, because this is the one piece
 * of the media path that is a policy rather than a mechanism: thresholds, a
 * hold, a recovery period, and a rule about whose evidence counts. All of that
 * is worth being able to test without a session, a bridge or a browser.
 *
 * The input is the drift meter — how fast each publisher's audio is arriving
 * later than the clock that produced it. See internal/telemetry/skew.go for
 * why that derivative means what it means; here it is simply a number per
 * publisher, in milliseconds of delay accumulated per second of wall clock.
 */

import type { TrackMetrics } from './protocol';

/**
 * How fast a publisher's audio must be falling behind to count as congestion,
 * in milliseconds per second.
 *
 * A healthy path measures within a tenth of this and a real bottleneck a
 * hundred times it, so there is a wide gap to sit in. The debug panel warns
 * past 1 ms/s, which is where clock drift between two machines stops being a
 * plausible explanation; acting is held to ten times that.
 */
export const DRIFT_CONGESTED_MS_PER_SEC = 10;

/**
 * How long the drift must hold before the picture is taken down. A single
 * sample can catch a burst that is already draining, and a layer switch costs
 * a wait for the next keyframe.
 */
export const DRIFT_CONGESTED_HOLD_MS = 2000;

/**
 * How long it must stay quiet before the full picture is asked for again.
 *
 * Far longer than the drop, and deliberately so: the two mistakes are not the
 * same size. Going back up costs a fresh SUBSCRIBE, a new handle, a new
 * decoder and a wait for the next keyframe, so being wrong upwards is visible
 * in a way being wrong downwards is not.
 */
export const DRIFT_QUIET_MS = 15000;

/**
 * How many publishers must be drifting at once for the cause to be our own
 * downlink rather than one publisher's uplink.
 *
 * Capped by how many there are to ask: in a two-person call there is no second
 * opinion to be had.
 */
export const DRIFT_MIN_PUBLISHERS = 2;

/**
 * Picks the drift readings that bear on our own inbound path.
 *
 * Inbound audio only. Outbound tracks describe what we send; video carries no
 * reading, because a publisher emits frames as its encoder finishes them and a
 * keyframe takes materially longer than a delta — which is encoder noise, not
 * the network. A track that has not accumulated enough history yet reports
 * nothing at all, and is not evidence either way.
 */
export function inboundDrifts(tracks: TrackMetrics[] | undefined): number[] {
  return (tracks ?? [])
    .filter((t) => !t.label.startsWith('out/') && t.label.endsWith('/audio'))
    .map((t) => t.skewMillisPerSec)
    .filter((d): d is number => d !== undefined);
}

/**
 * Whether the inbound path is failing to carry what is being sent to us.
 *
 * Fed one sample at a time; reports when its mind changes. It leads the
 * relay's own verdict by several seconds — the relay says we could not keep up
 * only after it has stopped forwarding, whereas a filling queue can be watched
 * on the way there.
 */
export class CongestionDetector {
  #congested = false;
  #driftingSince: number | null = null;
  #quietSince: number | null = null;

  get congested(): boolean {
    return this.#congested;
  }

  /** Forgets everything, for a session that has been replaced. */
  reset(): void {
    this.#congested = false;
    this.#driftingSince = null;
    this.#quietSince = null;
  }

  /**
   * Feeds one sample's worth of per-publisher drift, and reports whether the
   * verdict changed.
   *
   * `now` is monotonic milliseconds, supplied rather than read so the policy
   * can be exercised over an hour of call in a millisecond of test.
   *
   * The count is what matters, not any single reading. Drift is measured per
   * publisher, so one publisher drifting alone says something about *their*
   * uplink or the relay's path for that one stream — and taking our own
   * picture down would be answering the wrong question, since their upload
   * does not shrink because we subscribed to a different track. Several
   * drifting together is the one thing they have in common, which is us.
   *
   * With a single publisher the two causes cannot be told apart. It is acted
   * on anyway: being wrong costs one layer switch, and doing nothing costs a
   * call that stays broken.
   */
  update(drifts: number[], now: number): boolean {
    // No readings is not evidence of health. Everyone may have just joined,
    // or every publisher may have gone silent; either way the last verdict
    // stands rather than being quietly revoked.
    if (drifts.length === 0) return false;

    const drifting = drifts.filter((d) => d > DRIFT_CONGESTED_MS_PER_SEC).length;
    const needed = Math.min(DRIFT_MIN_PUBLISHERS, drifts.length);

    if (drifting >= needed) {
      this.#quietSince = null;
      this.#driftingSince ??= now;
      if (!this.#congested && now - this.#driftingSince >= DRIFT_CONGESTED_HOLD_MS) {
        this.#congested = true;
        return true;
      }
      return false;
    }

    this.#driftingSince = null;
    this.#quietSince ??= now;
    if (this.#congested && now - this.#quietSince >= DRIFT_QUIET_MS) {
      this.#congested = false;
      return true;
    }
    return false;
  }
}
