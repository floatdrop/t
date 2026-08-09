import { describe, expect, it } from 'vitest';
import {
  tileColumns,
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


describe('tileWidth', () => {
  it('divides the viewport by the columns the grid will use', () => {
    // Three tiles is two columns; 1000 less padding and one gap, halved.
    expect(tileWidth(3, 1000)).toBeCloseTo((1000 - 24 - 10) / 2);
  });

  it('never goes negative on a viewport narrower than its own padding', () => {
    expect(tileWidth(2, 10)).toBe(0);
  });
});
