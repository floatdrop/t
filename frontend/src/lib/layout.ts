/**
 * The conference grid's geometry.
 *
 * Split out of Conference.svelte because the numbers were shared: the grid laid
 * tiles out with them, and "Auto" resolution worked backwards from them to
 * decide how big our own tile was on everyone else's screen. Auto is gone — see
 * the note on VIDEO_LADDER — so this is now just what the grid needs, kept
 * separate because it is arithmetic and testable where a component is not.
 */

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
