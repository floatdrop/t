import { describe, expect, it } from 'vitest';
import {
  DEFAULT_VIDEO_CODEC,
  isVideoCodecId,
  resolveVideoCodec,
  videoCodecOptions,
  videoCodecSpec,
  type VideoCodecSupport,
} from './codec';

/** What the probe returns for a platform that took everything, with layers. */
const all: VideoCodecSupport[] = [
  { id: 'h264', scalabilityMode: 'L1T2' },
  { id: 'h264-high', scalabilityMode: 'L1T2' },
  { id: 'hevc', scalabilityMode: 'L1T2' },
];

describe('codec strings', () => {
  it('names the lowest H.264 level that carries the stream', () => {
    // 720p30 is exactly 3600 macroblocks at exactly 108,000 a second, which is
    // level 3.1 to the last one of each.
    expect(videoCodecSpec('h264').codec(1280, 720, 30)).toBe('avc1.42E01F');
    // 1080p needs 8160, past 3.1's ceiling, so the string has to say 4.0.
    expect(videoCodecSpec('h264').codec(1920, 1080, 30)).toBe('avc1.42E028');
    // Past every size this app asks for — a very large display being shared —
    // lands on the catch-all rather than describing a stream we do not send.
    expect(videoCodecSpec('h264').codec(7680, 4320, 30)).toBe('avc1.42E033');
  });

  it('keeps the level and changes only the profile for High', () => {
    // The same stream, described as High rather than constrained baseline: the
    // level is a property of the size and rate, not of the profile.
    expect(videoCodecSpec('h264-high').codec(1280, 720, 30)).toBe('avc1.64001F');
    expect(videoCodecSpec('h264-high').codec(1920, 1080, 30)).toBe('avc1.640028');
  });

  it('picks HEVC levels on the same rungs', () => {
    expect(videoCodecSpec('hevc').codec(1280, 720, 30)).toBe('hvc1.1.6.L93.B0');
    expect(videoCodecSpec('hevc').codec(1920, 1080, 30)).toBe('hvc1.1.6.L120.B0');
    expect(videoCodecSpec('hevc').codec(3840, 2160, 30)).toBe('hvc1.1.6.L153.B0');
  });

  it('counts a partial macroblock as a whole one', () => {
    // 1080 is not a multiple of 16, so the last row of macroblocks is mostly
    // padding and still has to be counted — 8160 rather than 8100. Rounding the
    // other way would put 1080p30 on level 3.1, which cannot carry it.
    expect(videoCodecSpec('h264').codec(1920, 1080, 30)).not.toBe('avc1.42E01F');
  });
});

describe('resolveVideoCodec', () => {
  it('uses the pick when the platform took it', () => {
    expect(resolveVideoCodec('hevc', all)).toBe('hevc');
    expect(resolveVideoCodec('h264', all)).toBe('h264');
  });

  it('falls back down the list, never up', () => {
    // A platform that refuses High must not be answered with HEVC: that is a
    // heavier decode chosen on behalf of everyone else in the room.
    const noHigh: VideoCodecSupport[] = [{ id: 'h264' }, { id: 'hevc' }];
    expect(resolveVideoCodec('h264-high', noHigh)).toBe('h264');
    expect(resolveVideoCodec('hevc', [{ id: 'h264-high' }])).toBe('h264-high');
  });

  it('walks up only when there is nothing plainer', () => {
    // Nothing below the pick survived, so the alternative is the only codec
    // there is — better than configuring one the platform refused.
    expect(resolveVideoCodec('h264', [{ id: 'hevc' }])).toBe('hevc');
  });

  it('defers to the pick when the probe found nothing', () => {
    // The probe is advisory and configure() is authoritative: a platform that
    // answered no to everything is certainly encoding something.
    expect(resolveVideoCodec('h264-high', [])).toBe('h264-high');
  });
});

describe('videoCodecOptions', () => {
  it('offers only what the platform took, in list order', () => {
    const rows = videoCodecOptions([{ id: 'hevc' }, { id: 'h264' }], 'h264');
    expect(rows.map((r) => r.id)).toEqual(['h264', 'hevc']);
    expect(rows.every((r) => r.available)).toBe(true);
  });

  it('keeps a refused pick listed, marked unavailable', () => {
    // Otherwise the select has no row for its own value and shows an empty box.
    const rows = videoCodecOptions([{ id: 'h264' }], 'hevc');
    expect(rows.map((r) => r.id)).toEqual(['h264', 'hevc']);
    expect(rows.find((r) => r.id === 'hevc')?.available).toBe(false);
  });

  it('says so when a codec is only encodable flat', () => {
    // The cost of the pick that is not about the picture: no enhancement layer
    // means a relay under pressure sheds the group instead of half the frames.
    const rows = videoCodecOptions([{ id: 'h264', scalabilityMode: 'L1T2' }, { id: 'hevc' }], 'hevc');
    expect(rows.find((r) => r.id === 'hevc')?.note).toMatch(/No temporal layers here/);
    expect(rows.find((r) => r.id === 'h264')?.note).not.toMatch(/No temporal layers here/);
  });

  it('offers the pick alone until the probe answers', () => {
    const rows = videoCodecOptions([], 'h264-high');
    expect(rows.map((r) => r.id)).toEqual(['h264-high']);
    // An unanswered probe is not a refusal.
    expect(rows[0].available).toBe(true);
  });
});

describe('stored preferences', () => {
  it('accepts what this build offers and nothing else', () => {
    expect(isVideoCodecId(DEFAULT_VIDEO_CODEC)).toBe(true);
    // A value left behind by a build that offered more.
    expect(isVideoCodecId('av1')).toBe(false);
  });
});
