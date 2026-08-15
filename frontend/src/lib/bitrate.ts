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
 * a cellular uplink has. Either one is enough to step down, once it has been
 * seen twice running.
 *
 * Down fast and up slow, with separate thresholds for the two directions. That
 * asymmetry is the whole of the hysteresis: a rate that is too high is being
 * paid for in frames right now, while a rate that is too low costs only sharpness
 * — so backing off is worth doing on the first evidence, and climbing is worth
 * making sure of. It also stops the loop oscillating around a threshold, which
 * would be a keyframe every couple of seconds for no gain.
 *
 * Loss is measured over the controller's own window rather than the sample it
 * arrives in, because a sample is a quarter second and a quarter second is not
 * enough packets to have a loss *rate*. At 1500 kbps that interval carries
 * around forty packets, so one lost packet reads as 2.5 per cent — above the
 * threshold for stepping down, and above the threshold for being clean too, so
 * the band between them did not exist and every sample was either perfect or
 * congested. The first version of this walked to the floor on links that were
 * fine and could not climb back off it: the run of clean samples was reset by
 * the same single packet. Four seconds of history, and a minimum number of
 * packets before the ratio is read at all, is what makes the thresholds mean
 * what they say.
 *
 * The clean run decays rather than resets for the other half of that. Ten
 * unbroken seconds is a condition a real link rarely meets — one blip in the
 * ninth second cost all of it — so a congested sample now costs a second of
 * progress instead. Intermittent congestion still cannot accumulate its way up,
 * because a link that congests every second or two loses ground faster than it
 * gains it.
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
 * The lowest rung worth sending a given picture height at, in bits per second.
 *
 * A floor is a statement about resolution, not about the link: 300 kbps carries
 * 360p and turns 720p into blocks, so a ladder that runs to 300 kbps whatever
 * the size is one that ends in an unwatchable picture. Below the floor the
 * honest move is fewer pixels rather than fewer bits — and this app does not
 * change resolution mid-call on purpose (see VIDEO_LADDER), so the floor is
 * where adaptation stops. A link that cannot carry even that is handed back to
 * the relay, which shreds the enhancement layer built to be shed.
 *
 * Listed shortest first; a height taller than the last entry takes its floor.
 */
const HEIGHT_FLOORS = [
  { height: 360, floor: 300_000 },
  { height: 480, floor: 500_000 },
  { height: 720, floor: 750_000 },
  { height: 1080, floor: 1_250_000 },
] as const;

/** The lowest rate worth encoding `height` at. */
export function floorFor(height: number): number {
  for (const rung of HEIGHT_FLOORS) {
    if (height <= rung.height) return rung.floor;
  }
  return HEIGHT_FLOORS[HEIGHT_FLOORS.length - 1].floor;
}

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
 * How many samples of packet counts the loss ratio is taken over.
 *
 * Four seconds at the 250 ms metrics cadence. Long enough that one lost packet
 * is a fraction of a per cent rather than a verdict, and short enough that the
 * ratio still describes the link as it is now: loss that stopped four seconds
 * ago has left the window.
 */
const LOSS_WINDOW_SAMPLES = 16;

/**
 * Fewest packets in the window for the loss ratio to be read at all.
 *
 * Below this the denominator is too small for a percentage to mean anything —
 * the same problem as a single sample, only slower. An unreadable ratio counts
 * as no evidence rather than as loss, so a nearly idle uplink is not held down
 * by a statistic it never produced.
 */
const LOSS_MIN_PACKETS = 50;

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
 * What a congested sample costs the run of clean ones behind it.
 *
 * A second of progress, against the ten seconds the climb needs. Resetting the
 * run outright is what this replaced: it made the climb conditional on ten
 * consecutive perfect samples, which a link with any ordinary jitter never
 * produces, so a rate that stepped down once stayed down for the call. A decay
 * still keeps intermittent congestion from accumulating its way up — one bad
 * sample every four costs exactly as much as those four earned.
 */
const CLEAN_PENALTY_SAMPLES = 4;

/**
 * How many consecutive congested samples a step down needs.
 *
 * Half a second. The loss signal is already taken over four seconds of history,
 * so this is really a condition on the other one: a standing queue is read from
 * a single sample, and a lone RTT spike — a Wi-Fi retransmit, a scheduler
 * hiccup — is not a full buffer. Confirming it costs a quarter second of
 * reacting late, against a rung that then takes ten clean seconds to earn back.
 */
const CONGESTED_SAMPLES_TO_STEP = 2;

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
  /**
   * Packets sent on this connection since it was established.
   *
   * Cumulative rather than the interval's own rate, so the controller can take
   * its own window over them — see LOSS_WINDOW_SAMPLES. A count that goes
   * backwards is a new connection, and resets the window rather than reading as
   * an enormous negative interval.
   */
  packetsSent: number;
  /** Packets lost on this connection since it was established. */
  packetsLost: number;
  /** Smoothed RTT, milliseconds. */
  rttMs: number;
  /** Lowest RTT this connection has seen, milliseconds. */
  minRttMs: number;
}

/**
 * Rungs between floor and ceiling, lowest first; never empty.
 *
 * A ceiling below the floor leaves nothing to choose between — a source worth
 * less than its size demands — and gets the ceiling itself as its one rung, so
 * nothing is ever asked for above what the setting said. Below the bottom of
 * the ladder even that is not a rate worth encoding, and the lowest rung is the
 * least wrong answer.
 */
export function rungsFor(ceiling: number, floor: number = BITRATE_RUNGS[0]): number[] {
  const usable = BITRATE_RUNGS.filter((r) => r >= floor && r <= ceiling);
  if (usable.length > 0) return [...usable];
  return [Math.max(ceiling, BITRATE_RUNGS[0])];
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
  /** Consecutive congested samples — see CONGESTED_SAMPLES_TO_STEP. */
  #congested = 0;
  #sinceChange = 0;
  #warmup = 0;
  /** Per-interval (sent, lost) counts, oldest first — see LOSS_WINDOW_SAMPLES. */
  #window: Array<{ sent: number; lost: number }> = [];
  /** The cumulative counts the last sample carried, to difference against. */
  #prev: { sent: number; lost: number } | null = null;

  /**
   * Starts at the ceiling rather than in the middle of the ladder. The ceiling
   * is what this app sent before there was a controller at all, so a link that
   * was carrying it keeps carrying it and nothing has to climb back to where it
   * already was; a link that cannot will say so within a sample or two.
   *
   * The floor is the resolution's, not the link's: see floorFor.
   */
  constructor(ceiling: number, floor: number = BITRATE_RUNGS[0]) {
    this.#rungs = rungsFor(ceiling, floor);
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
    this.#record(sample);
    if (this.#warmup < WARMUP_SAMPLES) {
      this.#warmup++;
      return null;
    }

    // Standing queue: the part of the delay we are causing. A minRtt that has
    // not been established yet reads as no queue rather than as a huge one.
    const standing = sample.minRttMs > 0 ? sample.rttMs - sample.minRttMs : 0;
    // A ratio the window is too thin to support is no evidence either way: it
    // neither steps the rate down nor stops a clean link climbing.
    const loss = this.#loss();
    const congested = (loss !== null && loss >= LOSS_DOWN_PERCENT) || standing >= RTT_DOWN_MS;

    if (congested) {
      this.#congested++;
      // Charged on every congested sample, confirmed or not, so a link that is
      // congested every few seconds cannot accumulate its way back up.
      this.#clean = Math.max(0, this.#clean - CLEAN_PENALTY_SAMPLES);
      if (this.#congested < CONGESTED_SAMPLES_TO_STEP) return null;
      if (this.#index === 0 || this.#sinceChange < DOWN_COOLDOWN_SAMPLES) return null;
      this.#index--;
      return this.#changed();
    }
    this.#congested = 0;

    const clean = (loss === null || loss <= LOSS_UP_PERCENT) && standing <= RTT_UP_MS;
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
    return this.#changed();
  }

  /** Folds one sample's cumulative counters into the loss window. */
  #record(sample: LinkSample): void {
    const prev = this.#prev;
    this.#prev = { sent: sample.packetsSent, lost: sample.packetsLost };
    if (!prev) return;
    const sent = sample.packetsSent - prev.sent;
    const lost = sample.packetsLost - prev.lost;
    if (sent < 0 || lost < 0) {
      // Counters that went backwards belong to a connection this window knows
      // nothing about. Its history describes a link that no longer exists.
      this.#window = [];
      return;
    }
    this.#window.push({ sent, lost });
    if (this.#window.length > LOSS_WINDOW_SAMPLES) this.#window.shift();
  }

  /** Loss over the window as a percentage, or null if too few packets to say. */
  #loss(): number | null {
    let sent = 0;
    let lost = 0;
    for (const s of this.#window) {
      sent += s.sent;
      lost += s.lost;
    }
    if (sent < LOSS_MIN_PACKETS) return null;
    return (100 * lost) / sent;
  }

  /**
   * Settles the bookkeeping that follows a step and returns the new target.
   *
   * The window is dropped with the change: it describes the rate that was being
   * sent, and the question from here is whether the new one fits. Keeping it
   * would let the losses that caused a step down cause the next one too, for as
   * long as they sat in the window — four seconds of stepping on evidence
   * already acted on.
   */
  #changed(): number {
    this.#sinceChange = 0;
    this.#window = [];
    return this.target;
  }
}
