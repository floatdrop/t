import { describe, expect, it } from 'vitest';
import {
  FULL_TILE_WIDTH,
  SMALL_TILE_WIDTH,
  tileColumns,
  tilesTakeSmallVideo,
  tileWidth,
} from './layout';

/**
 * Builds an input whose tile comes out near a chosen device-pixel width, so
 * these read as "a tile this big" rather than as arithmetic about viewports.
 */
function tilesOf(deviceWidth: number, tiles = 2, pixelRatio = 2) {
  const columns = tileColumns(tiles);
  // Invert tileWidth: it takes the padding and gaps off before dividing.
  const cssWidth = deviceWidth / pixelRatio;
  const viewportWidth = cssWidth * columns + 24 + 10 * (columns - 1);
  return { tiles, viewportWidth, pixelRatio };
}

describe('tilesTakeSmallVideo', () => {
  it('takes the small layer for a tile that cannot show more', () => {
    expect(tilesTakeSmallVideo(tilesOf(SMALL_TILE_WIDTH - 100), false)).toBe(true);
  });

  it('takes the full picture for a tile with room for it', () => {
    expect(tilesTakeSmallVideo(tilesOf(FULL_TILE_WIDTH + 200), false)).toBe(false);
  });

  // One threshold means a tile sitting on it changes layer on every nudge, and
  // a layer change costs a SUBSCRIBE, a decoder and a backfilled group.
  it('does not flip back the moment it crosses the entry threshold', () => {
    const justInside = tilesOf(SMALL_TILE_WIDTH + 20);
    // Coming from the full picture, a tile this size stays full.
    expect(tilesTakeSmallVideo(justInside, false)).toBe(false);
    // Already on the small layer, the same size stays small rather than
    // bouncing straight back.
    expect(tilesTakeSmallVideo(justInside, true)).toBe(true);
  });

  it('only returns to the full picture once clear of the upper threshold', () => {
    expect(tilesTakeSmallVideo(tilesOf(FULL_TILE_WIDTH - 20), true)).toBe(true);
    expect(tilesTakeSmallVideo(tilesOf(FULL_TILE_WIDTH + 20), true)).toBe(false);
  });

  // The two thresholds must not cross, or the band between them would be a
  // state the decision can never leave.
  it('has an upper threshold above the lower one', () => {
    expect(FULL_TILE_WIDTH).toBeGreaterThan(SMALL_TILE_WIDTH);
  });

  // Before layout has happened there is no measurement, and guessing costs a
  // layer change either way.
  it('keeps the current answer when there is nothing to measure', () => {
    const unmeasured = { tiles: 2, viewportWidth: 0, pixelRatio: 2 };
    expect(tilesTakeSmallVideo(unmeasured, true)).toBe(true);
    expect(tilesTakeSmallVideo(unmeasured, false)).toBe(false);
  });

  // Expanding renders one tile at the full width, which is the case that used
  // to be computed as though the grid were still multi-column — so maximising
  // a face fetched the smaller encoding and made it softer.
  it('gives a solo tile the full picture', () => {
    const grid = { tiles: 5, viewportWidth: 1280, pixelRatio: 1 };
    expect(tilesTakeSmallVideo(grid, false)).toBe(true);
    expect(tilesTakeSmallVideo({ ...grid, tiles: 1 }, false)).toBe(false);
  });
});

describe('tileWidth', () => {
  it('divides the viewport by the columns the grid will use', () => {
    // Three tiles is two columns; 1000 less padding and one gap, halved.
    expect(tileWidth(3, 1000)).toBeCloseTo((1000 - 24 - 10) / 2);
  });

  it('never goes negative on a viewport narrower than its own padding', () => {
    expect(tileWidth(2, 10)).toBe(0);
  });
});
