/**
 * Choosing what to ask the video encoder for, from what the link is doing.
 *
 * There was no answer to congestion on this side at all. The encoder was
 * configured once and changed only when a setting did, so a link that could not
 * carry the picture was sent it anyway — and what answered that was the relay,
 * shedding the enhancement layer, timing a subgroup out, or giving up on the
 * subscription. Those are the right last resorts and the wrong first one: each
 * costs a subscriber frames or a re-subscribe, for a problem the publisher could
 * have avoided by sending less.
 *
 * The signal is our own uplink to the relay, and deliberately only that. One
 * encoder serves every subscriber, so there is no per-subscriber rate to pick;
 * what is downstream of the relay is the relay's to manage, and it already does,
 * by shedding a layer built to be shed. What is ours is the hop we can measure.
 *
 * Rungs rather than a continuous rate, because every change costs an encoder
 * reconfigure and a keyframe with it. A controller free to move by a few per
 * cent would spend that constantly for a difference nobody can see. Six steps
 * covers the useful range at a resolution the picture actually reflects.
 *
 * Loss and queueing delay together, because they fail at different times. Loss
 * is the honest signal on a link that drops, and says nothing until a buffer has
 * already overflowed; the RTT standing above its own floor is what a filling
 * buffer looks like before that, and is the earlier warning on the deep buffers
 * a cellular uplink has. Either one is enough to step down.
 *
 * Down fast and up slow, with separate thresholds for the two directions. That
 * asymmetry is the whole of the hysteresis: a rate that is too high is being
 * paid for in frames right now, while a rate that is too low costs only sharpness
 * — so backing off is worth doing on the first evidence, and climbing is worth
 * making sure of. It also stops the loop oscillating around a threshold, which
 * would be a keyframe every couple of seconds for no gain.
 */

/**
 * The rates the controller may choose, lowest first.
 *
 * Filtered against the ceiling for the source in use, so this ladder is the
 * shape of the steps rather than a claim about any particular source. The
 * spacing widens as it climbs because that is how the picture reads: the
 * difference between 300 and 500 kbps at 720p is the difference between mush
 * and watchable, and between 2 and 3 Mbps is one nobody will name.
 */
export const BITRATE_RUNGS = [
  300_000, 500_000, 750_000, 1_000_000, 1_250_000, 1_500_000, 2_000_000, 3_000_000,
] as const;

/**
 * Loss above which the rate steps down, as a percentage of packets sent.
 *
 * Not zero. QUIC uses loss to find the path's capacity, so a healthy connection
 * loses packets occasionally and a controller that treated the first one as
 * congestion would walk itself to the floor on a link that was fine.
 */
const LOSS_DOWN_PERCENT = 2;

/** Loss below which a sample counts as clean enough to climb on. */
const LOSS_UP_PERCENT = 0.5;

/**
 * Queueing delay above which the rate steps down, in milliseconds.
 *
 * Measured as the smoothed RTT standing above the minimum this connection has
 * ever seen, which is the part of the delay we are causing rather than the part
 * the path costs. A hundred milliseconds of standing queue is already audible as
 * lag and is well clear of ordinary RTT wander.
 */
const RTT_DOWN_MS = 100;

/** Standing queue below which a sample counts as clean enough to climb on. */
const RTT_UP_MS = 30;

/**
 * How many consecutive clean samples earn a step up.
 *
 * At the 250 ms metrics cadence this is ten seconds of a link with no loss and
 * no standing queue. Long, on purpose: climbing is what risks causing the
 * congestion this exists to avoid, and the cost of climbing late is sharpness
 * nobody was promised.
 */
const CLEAN_SAMPLES_TO_CLIMB = 40;

/**
 * Shortest gap between steps down, in samples.
 *
 * One overflowing buffer produces congested samples for as long as it takes to
 * drain, and stepping on each of them would cross the whole ladder in a second
 * — a collapse to the floor from one burst. Two seconds is long enough for a
 * step to show up in what is being measured.
 */
const DOWN_COOLDOWN_SAMPLES = 8;

/**
 * Samples to ignore at the start of a track.
 *
 * minRtt has to see a real minimum before "above the minimum" means anything,
 * and the first samples of a connection are also when it is probing hardest for
 * capacity. Acting on those would be reading the handshake as congestion.
 */
const WARMUP_SAMPLES = 8;

/** What the controller reads out of one metrics sample. */
export interface LinkSample {
  /** Packets lost as a percentage of packets sent in the interval. */
  lossPercent: number;
  /** Smoothed RTT, milliseconds. */
  rttMs: number;
  /** Lowest RTT this connection has seen, milliseconds. */
  minRttMs: number;
}

/** Rungs at or below ceiling, lowest first; never empty. */
export function rungsFor(ceiling: number): number[] {
  const usable = BITRATE_RUNGS.filter((r) => r <= ceiling);
  return usable.length > 0 ? [...usable] : [BITRATE_RUNGS[0]];
}

/**
 * Picks a video bitrate from what the uplink is doing.
 *
 * One per video pipeline: it carries the current rung and the run of clean
 * samples behind it, both of which belong to the encoder that is running rather
 * than to the call.
 */
export class BitrateController {
  #rungs: number[];
  #index: number;
  #clean = 0;
  #sinceChange = 0;
  #warmup = 0;

  /**
   * Starts at the ceiling rather than in the middle of the ladder. The ceiling
   * is what this app sent before there was a controller at all, so a link that
   * was carrying it keeps carrying it and nothing has to climb back to where it
   * already was; a link that cannot will say so within a sample or two.
   */
  constructor(ceiling: number) {
    this.#rungs = rungsFor(ceiling);
    this.#index = this.#rungs.length - 1;
  }

  /** The rate currently asked for. */
  get target(): number {
    return this.#rungs[this.#index];
  }

  /**
   * Folds in one metrics sample and returns the new target, or null when
   * nothing should change — which is almost every sample.
   */
  step(sample: LinkSample): number | null {
    this.#sinceChange++;
    if (this.#warmup < WARMUP_SAMPLES) {
      this.#warmup++;
      return null;
    }

    // Standing queue: the part of the delay we are causing. A minRtt that has
    // not been established yet reads as no queue rather than as a huge one.
    const standing = sample.minRttMs > 0 ? sample.rttMs - sample.minRttMs : 0;
    const congested = sample.lossPercent >= LOSS_DOWN_PERCENT || standing >= RTT_DOWN_MS;

    if (congested) {
      // The run of clean samples is broken whether or not a step follows, so a
      // link that is congested every few seconds never accumulates its way up.
      this.#clean = 0;
      if (this.#index === 0 || this.#sinceChange < DOWN_COOLDOWN_SAMPLES) return null;
      this.#index--;
      this.#sinceChange = 0;
      return this.target;
    }

    const clean = sample.lossPercent <= LOSS_UP_PERCENT && standing <= RTT_UP_MS;
    if (!clean) {
      // Between the two thresholds: not congested enough to give up a rung, not
      // clean enough to earn one. Holding here is what keeps the loop off a
      // keyframe every couple of seconds.
      return null;
    }

    this.#clean++;
    if (this.#index === this.#rungs.length - 1) return null;
    if (this.#clean < CLEAN_SAMPLES_TO_CLIMB) return null;

    this.#clean = 0;
    this.#index++;
    this.#sinceChange = 0;
    return this.target;
  }
}
