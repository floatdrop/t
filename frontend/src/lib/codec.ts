/**
 * Which codec the local video encoder is configured with, and which of them
 * this WebView will actually accept.
 *
 * There used to be one answer here, written into capture.ts: constrained
 * baseline H.264, at the lowest level that could carry the stream. Baseline is
 * the safest bitstream anything will decode and it is also the weakest — no
 * CABAC, no 8x8 transform, no B-frames — so at the rates a call runs on, it is
 * what puts blocks on a face that moves. The rate was never the problem: the
 * profile was spending it badly.
 *
 * So the codec is a setting now, and this module is what a setting needs — the
 * list, the codec string each one is written as, and the framing members that
 * only apply to its family. Nothing here decides anything on its own: capture
 * resolves a preference against `videoCodecSupport()` before it configures an
 * encoder, so what is published is always something this platform said yes to.
 *
 * Everything past the encoder is codec-agnostic and stays that way. The Go half
 * moves bytes and reads the keyframe flag out of the bridge header rather than
 * the bitstream; playback configures a decoder from the codec string in the
 * catalog. Adding a family is an entry in the table below and nothing else.
 */

/** A codec the picker can offer. */
export type VideoCodecId = 'h264' | 'h264-high' | 'hevc';

/**
 * The SVC mode the primary video encoding asks for.
 *
 * One spatial layer, two temporal ones: frames alternate between a base layer
 * that stands on its own and an enhancement layer nothing else references. The
 * backend maps the layer onto the subgroup it publishes the frame in, so the
 * enhancement layer is separately declinable and separately sheddable.
 *
 * L1T2 rather than L1T3: shedding takes 30 fps to 15, which reads as a slightly
 * less fluid picture. L1T3's bottom rung is 7.5 fps, which reads as broken, and
 * the middle rung is a second decision to get right for a saving the first rung
 * already mostly banked.
 *
 * Asked for per codec rather than assumed, because whether a platform honours
 * it is a property of the encoder behind the family — VideoToolbox's H.264 and
 * its HEVC are two different encoders, and neither the API nor the spec
 * promises they answer the same way. `videoCodecSupport()` finds out; on macOS
 * the answer is that H.264 layers and HEVC does not.
 *
 * Kept as a constant because two things have to agree on how many layers there
 * are — this and the two bits the bridge header spends on the layer id.
 */
export const VIDEO_SCALABILITY_MODE = 'L1T2';

/**
 * The H.264 levels this will ask for, lowest first, with the limits §A.3.1
 * sets on each: macroblocks per frame, and macroblocks per second.
 *
 * A level is a ceiling on what a decoder must be prepared for, so naming one
 * too low is naming a stream we do not send. 3.1 stops at 3600 macroblocks —
 * exactly 1280x720, and not one row more — while 1080p needs 8160. Both the
 * top rung of VIDEO_LADDER and every screen share ask for 1080p, so a fixed
 * 3.1 described neither: WebKit is free to refuse the configuration outright,
 * and a decoder that believes the string is entitled to size its buffers for
 * 720p and meet a frame it has no room for.
 *
 * 5.1 is not a size anything here asks for. It is the catch to a source that
 * outran the constraints put on it, so an unusual display cannot end the call
 * by being large.
 */
const H264_LEVELS = [
  { idc: 0x1f, maxFrameMBs: 3600, maxMBsPerSec: 108_000 }, // 3.1 — through 720p30
  { idc: 0x28, maxFrameMBs: 8192, maxMBsPerSec: 245_760 }, // 4.0 — through 1080p30
  { idc: 0x33, maxFrameMBs: 36864, maxMBsPerSec: 983_040 }, // 5.1 — the catch-all
] as const;

/**
 * The same ladder for HEVC, from Annex A.4.1, counted in luma samples rather
 * than macroblocks: a picture size and a sample rate per level.
 *
 * The rungs are chosen to line up with the H.264 ones — 3.1 through 720p30, 4.0
 * through 1080p30, 5.1 as the catch — so a change of codec is not also a change
 * of what a subscriber is told to be ready for.
 */
const HEVC_LEVELS = [
  { idc: 93, maxLumaPs: 983_040, maxLumaSr: 33_177_600 }, // 3.1
  { idc: 120, maxLumaPs: 2_228_224, maxLumaSr: 66_846_720 }, // 4.0
  { idc: 153, maxLumaPs: 8_912_896, maxLumaSr: 267_386_880 }, // 5.1
] as const;

/** How many luma samples one frame of this size occupies, macroblock-aligned. */
function lumaSamples(width: number, height: number): number {
  // A partial macroblock still costs a whole one, in either codec's arithmetic.
  return Math.ceil(width / 16) * 16 * (Math.ceil(height / 16) * 16);
}

function h264String(profile: string, width: number, height: number, framerate: number): string {
  const frameMBs = Math.ceil(width / 16) * Math.ceil(height / 16);
  const level =
    H264_LEVELS.find(
      (l) => frameMBs <= l.maxFrameMBs && frameMBs * framerate <= l.maxMBsPerSec,
    ) ?? H264_LEVELS[H264_LEVELS.length - 1];
  return `avc1.${profile}${level.idc.toString(16).toUpperCase().padStart(2, '0')}`;
}

function hevcString(width: number, height: number, framerate: number): string {
  const samples = lumaSamples(width, height);
  const level =
    HEVC_LEVELS.find((l) => samples <= l.maxLumaPs && samples * framerate <= l.maxLumaSr) ??
    HEVC_LEVELS[HEVC_LEVELS.length - 1];
  // Main profile, main tier: profile space 0 / idc 1, compatibility flags 6,
  // then the level, then the constraint bytes Safari itself writes.
  //
  // `hvc1` rather than `hev1`, and the difference is not cosmetic: hvc1 says the
  // parameter sets are out of band, hev1 says they are in the bitstream. WebKit
  // encodes HEVC length-prefixed with an hvcC whatever framing is asked of it
  // (measured — see framing() below), so out of band is what this actually is.
  return `hvc1.1.6.L${level.idc}.B0`;
}

/** One codec the picker can offer, and everything configuring it needs. */
interface VideoCodecSpec {
  id: VideoCodecId;
  /** How it is named in the picker. */
  label: string;
  /** The one line under it, saying what the choice costs and buys. */
  note: string;
  /** The codec string for one stream, at the lowest level that carries it. */
  codec(width: number, height: number, framerate: number): string;
  /**
   * The framing members that belong to this family and no other.
   *
   * Annex B puts the parameter sets in the bitstream ahead of every keyframe,
   * so a subscriber can start decoding from any group without an out-of-band
   * description — which is what lets a catalog carry nothing but a codec string
   * and a subscriber join mid-call. The capture path still forwards a
   * description if an encoder emits one anyway, so a family that has no
   * in-band form would degrade rather than break.
   */
  framing(): Partial<VideoEncoderConfig>;
}

/**
 * The codecs on offer, in ascending order of what a decoder has to be capable
 * of. Fallback walks back down this list, so the order is load-bearing: the
 * entry a platform is least likely to refuse comes first.
 */
export const VIDEO_CODECS: readonly VideoCodecSpec[] = [
  {
    id: 'h264',
    label: 'H.264 Baseline',
    note: 'The most compatible bitstream, and the blockiest under motion.',
    codec: (w, h, f) => h264String('42E0', w, h, f),
    framing: () => ({ avc: { format: 'annexb' } }),
  },
  {
    id: 'h264-high',
    label: 'H.264 High',
    note: 'Same codec, same bitrate, fewer artifacts. Any decoder of the last decade.',
    codec: (w, h, f) => h264String('6400', w, h, f),
    framing: () => ({ avc: { format: 'annexb' } }),
  },
  {
    id: 'hevc',
    label: 'HEVC',
    note: 'The best picture per bit here, and the heaviest to decode.',
    codec: hevcString,
    // Nothing to ask for. WebCodecs has an `hevc: { format: 'annexb' }` member
    // and WebKit ignores it: measured, every HEVC configuration here — with the
    // member, without it, spelled hvc1 or hev1 — came back length-prefixed with
    // a 105-byte hvcC description, and a decoder configured without that
    // description failed on every single frame. So the description is what
    // carries the config, the codec string says so, and capture re-declares the
    // track with it as soon as the encoder hands it over.
    framing: () => ({}),
  },
] as const;

/**
 * What a fresh install encodes with.
 *
 * High rather than baseline. It is the same codec to every decoder that
 * matters — High has been mandatory in hardware for well over a decade — and it
 * spends a call's bitrate far better, which is the whole complaint baseline
 * answers badly. `resolveVideoCodec` drops back to baseline on a platform that
 * refuses it, so naming it here costs nothing on one that does.
 */
export const DEFAULT_VIDEO_CODEC: VideoCodecId = 'h264-high';

export function videoCodecSpec(id: VideoCodecId): VideoCodecSpec {
  return VIDEO_CODECS.find((c) => c.id === id) ?? VIDEO_CODECS[0];
}

/** What this platform said about one codec. */
export interface VideoCodecSupport {
  id: VideoCodecId;
  /**
   * The scalability mode it approved, or undefined if it would only take the
   * configuration flat. A codec is offered either way — flat video is a
   * degradation of what the call can survive under load, not a broken call —
   * but the difference is worth knowing before the encoder is built rather
   * than by watching what comes out of it.
   */
  scalabilityMode?: string;
}

/**
 * The size the probe asks about.
 *
 * The middle of VIDEO_LADDER, which is also the default: a probe at 1080p would
 * turn a platform that only encodes 720p into a platform with no codecs at all,
 * and the level in the codec string is chosen per stream anyway — so what is
 * being asked here is whether the family works, not whether one size does.
 * `isConfigSupported` is willing to approve sizes the platform then refuses to
 * encode, so the real answer always comes from configure().
 */
const PROBE = { width: 1280, height: 720, framerate: 30, bitrate: 1_500_000 } as const;

let probed: Promise<VideoCodecSupport[]> | null = null;

/**
 * Asks this WebView which of the codecs above it can both encode and decode,
 * once per run.
 *
 * Both, deliberately. A call is symmetric — everyone publishes and everyone
 * subscribes — so a codec this build can produce and cannot play is one that
 * works until a second person joins on the same platform, which is the worst
 * shape a media fault can have.
 *
 * A codec that throws or is refused is simply absent from the result: the
 * picker lists what came back, and the encoder resolves against it. Nothing
 * here reports a failure, because "this Mac has no AV1 encoder" is not a fault
 * in this app.
 */
export function videoCodecSupport(): Promise<VideoCodecSupport[]> {
  probed ??= probeVideoCodecs();
  return probed;
}

async function probeVideoCodecs(): Promise<VideoCodecSupport[]> {
  const found: VideoCodecSupport[] = [];
  for (const spec of VIDEO_CODECS) {
    const codec = spec.codec(PROBE.width, PROBE.height, PROBE.framerate);
    if (!(await canDecode(codec))) continue;

    const base: VideoEncoderConfig = {
      codec,
      width: PROBE.width,
      height: PROBE.height,
      bitrate: PROBE.bitrate,
      framerate: PROBE.framerate,
      bitrateMode: 'constant',
      latencyMode: 'realtime',
      ...spec.framing(),
    };
    // Layered first, flat second. Asking only about the flat configuration
    // would say yes on a platform that then refuses scalabilityMode at
    // configure() — the spec has that throw, and a throw there closes the
    // encoder for the rest of the call.
    if (await canEncode({ ...base, scalabilityMode: VIDEO_SCALABILITY_MODE })) {
      found.push({ id: spec.id, scalabilityMode: VIDEO_SCALABILITY_MODE });
    } else if (await canEncode(base)) {
      found.push({ id: spec.id });
    }
  }
  return found;
}

async function canEncode(config: VideoEncoderConfig): Promise<boolean> {
  try {
    return (await VideoEncoder.isConfigSupported(config)).supported === true;
  } catch {
    // A configuration the platform cannot even parse rejects with a TypeError,
    // which is an answer rather than an error.
    return false;
  }
}

async function canDecode(codec: string): Promise<boolean> {
  try {
    const support = await VideoDecoder.isConfigSupported({
      codec,
      codedWidth: PROBE.width,
      codedHeight: PROBE.height,
      optimizeForLatency: true,
    });
    return support.supported === true;
  } catch {
    return false;
  }
}

/**
 * The codec to actually configure, given what was asked for and what came back
 * from the probe.
 *
 * Down the list rather than up: a platform that will not take the pick is
 * answered with something plainer, never with something more demanding. Walking
 * up would answer "H.264 High is unavailable" with HEVC, which is a heavier
 * decode chosen on behalf of everyone else in the room.
 *
 * A probe that found nothing at all resolves to the pick unchanged. That case
 * means the platform answered no to a codec it is certainly encoding — the
 * probe is advisory, `configure()` is authoritative — so the honest thing is to
 * try what was asked for and let the encoder's error path say why not.
 */
export function resolveVideoCodec(
  want: VideoCodecId,
  support: readonly VideoCodecSupport[],
): VideoCodecId {
  if (support.length === 0) return want;
  if (support.some((s) => s.id === want)) return want;

  const index = VIDEO_CODECS.findIndex((c) => c.id === want);
  for (let i = index - 1; i >= 0; i--) {
    if (support.some((s) => s.id === VIDEO_CODECS[i].id)) return VIDEO_CODECS[i].id;
  }
  for (let i = index + 1; i < VIDEO_CODECS.length; i++) {
    if (support.some((s) => s.id === VIDEO_CODECS[i].id)) return VIDEO_CODECS[i].id;
  }
  return want;
}

/** One row of the codec picker. */
export interface VideoCodecOption {
  id: VideoCodecId;
  label: string;
  note: string;
  /**
   * False for a codec this platform refused. Such a row is only ever the
   * current pick — a preference stored by a build, or on a machine, where it
   * worked — and it is listed rather than dropped so the picker can say what is
   * selected instead of showing an empty box. `resolveVideoCodec` is what the
   * encoder actually runs on.
   *
   * True while the probe is still out, for the same reason `resolveVideoCodec`
   * defers to the pick then: an unanswered probe is not a refusal.
   */
  available: boolean;
}

/**
 * What a codec costs when this platform will only encode it flat.
 *
 * Worth saying in the picker rather than only in the log, because it is the one
 * cost of a codec choice that is not about the picture: a flat stream has one
 * subgroup, so a relay under pressure has no enhancement layer to shed and
 * takes the whole group instead. Measured on macOS, where VideoToolbox layers
 * H.264 and does not layer HEVC.
 */
const FLAT_NOTE = 'No temporal layers here, so a tight link loses the picture rather than half the frame rate.';

/**
 * The rows to offer, in the order of VIDEO_CODECS, given what the probe found
 * and what is currently picked.
 *
 * A probe that has not answered yet offers the current pick alone. Listing
 * everything until it does would let someone choose a codec in the second
 * before the platform said no to it.
 */
export function videoCodecOptions(
  support: readonly VideoCodecSupport[],
  current: VideoCodecId,
): VideoCodecOption[] {
  return VIDEO_CODECS.filter(
    (c) => c.id === current || support.some((s) => s.id === c.id),
  ).map((c) => {
    const approved = support.find((s) => s.id === c.id);
    return {
      id: c.id,
      label: c.label,
      note: approved && !approved.scalabilityMode ? `${c.note} ${FLAT_NOTE}` : c.note,
      available: support.length === 0 || approved !== undefined,
    };
  });
}

/** Whether a string names a codec this build offers, for a stored preference. */
export function isVideoCodecId(value: string): value is VideoCodecId {
  return VIDEO_CODECS.some((c) => c.id === value);
}
