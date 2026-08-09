/**
 * The conference grid's geometry, and what it implies for what we send.
 *
 * Split out of Conference.svelte because two things need the same numbers. The
 * grid uses them to lay tiles out; "Auto" resolution uses them to work out how
 * big our own tile is on everyone else's screen, which is the only honest basis
 * for choosing what to encode. Sending 1080p into a tile 400 px wide spends
 * bitrate on pixels that are thrown away before anyone sees them.
 *
 * The tiles hold 16/9 and the grid scrolls, so height never constrains a tile —
 * width and the column count decide the size on their own.
 */

import { VIDEO_LADDER, type VideoRung } from './capture';

/** The grid's own padding, in CSS pixels. Applied by Conference.svelte. */
export const GRID_PADDING = 12;

/** The gap between tiles, in CSS pixels. Applied by Conference.svelte. */
export const GRID_GAP = 10;

/**
 * Most columns the grid will use, however many people are in the call.
 *
 * A cap rather than a square root all the way up: past three across, tiles stop
 * being faces and start being thumbnails, and the grid is better off scrolling.
 */
export const MAX_COLUMNS = 3;

/** How the grid arranges `tiles` tiles, the local one included. */
export function tileColumns(tiles: number): number {
  return Math.min(MAX_COLUMNS, Math.max(1, Math.ceil(Math.sqrt(tiles))));
}

/** How wide one tile comes out, in CSS pixels. */
export function tileWidth(tiles: number, viewportWidth: number): number {
  const columns = tileColumns(tiles);
  const inner = viewportWidth - GRID_PADDING * 2 - GRID_GAP * (columns - 1);
  return Math.max(0, inner) / columns;
}

/**
 * Beyond this, extra device pixels cost real bitrate and buy nothing anyone can
 * see. A 2x display is worth matching; a 3x one is not worth 2.25 times the
 * pixels to do it.
 */
const MAX_PIXEL_RATIO = 2;

export interface AutoVideoInput {
  /** Everyone with a tile, including us. */
  tiles: number;
  /** Window width in CSS pixels — the grid spans it. */
  viewportWidth: number;
  /** Device pixels per CSS pixel, so a retina tile is not fed a soft picture. */
  pixelRatio: number;
  /** The selected video bitrate, which is what a size has to be carried by. */
  bitrate: number;
}

/**
 * The size Auto asks for right now.
 *
 * Two limits, and the smaller wins. The grid says how many device pixels the
 * tile can actually show, so the first rung wide enough to fill it is as far up
 * as there is any point going. The bitrate says which rungs can be carried at
 * all, so a generous tile on a thin budget still comes down.
 *
 * The floor is the bottom rung whatever either says: 360p is where a face stops
 * being a face, and no arrangement of the grid is worth going below it.
 */
/**
 * What Auto will actually spend on the rung it picked.
 *
 * Choosing the size is only half of following the grid. A rung is chosen
 * because it is as much picture as the tile can show — but the bitrate is the
 * user's, chosen once for the call, and spending all of it on a tile that came
 * down to 360p buys nothing: past a point more bits on a small picture are
 * simply a cleaner small picture, and the budget would be better left unspent
 * on a link that is now carrying several streams instead of one. This is the
 * same waste the rung avoids, counted in bits rather than pixels.
 *
 * The ceiling is the next rung's minBitrate, which is already the number that
 * says where this size stops being the right one to ask for: with more than
 * that to spend, a bigger picture would be the better use of it — and Auto has
 * already established there is no room for a bigger picture. So the budget is
 * capped there rather than cut to this rung's own minimum, which is the point
 * where the size stops being worth asking for at all, not what it deserves.
 *
 * The top rung has nothing above it and so no ceiling: 1080p is as far as this
 * goes, and whatever was selected is spent on it.
 */
export function autoVideoBitrate(rung: VideoRung, selected: number): number {
  const next = VIDEO_LADDER[VIDEO_LADDER.findIndex((r) => r.width === rung.width) + 1];
  return next ? Math.min(selected, next.minBitrate) : selected;
}




export function autoVideoRung(input: AutoVideoInput): VideoRung {
  const wanted =
    tileWidth(input.tiles, input.viewportWidth) * Math.min(input.pixelRatio, MAX_PIXEL_RATIO);

  const affordable = VIDEO_LADDER.filter((rung) => rung.minBitrate <= input.bitrate);
  // Never empty, so the floor holds even at a bitrate below what the bottom
  // rung asks for. That is a bitrate the call has to live with; it is not a
  // reason to send a picture nobody can read a face from.
  const candidates = affordable.length > 0 ? affordable : [VIDEO_LADDER[0]];

  return candidates.find((rung) => rung.width >= wanted) ?? candidates[candidates.length - 1];
}
