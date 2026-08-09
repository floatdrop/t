import { describe, expect, it } from 'vitest';
import { MAX_COLUMNS, tileColumns } from './layout';

describe('tileColumns', () => {
  it('grows with the square root of the tile count', () => {
    // One tile fills the row; four make a square; the counts in between round
    // up rather than leaving a tile on a row of its own.
    expect(tileColumns(1)).toBe(1);
    expect(tileColumns(2)).toBe(2);
    expect(tileColumns(3)).toBe(2);
    expect(tileColumns(4)).toBe(2);
    expect(tileColumns(5)).toBe(3);
  });

  it('caps rather than growing without limit', () => {
    // Past three across a tile stops being a face, so the grid scrolls instead.
    expect(tileColumns(16)).toBe(MAX_COLUMNS);
    expect(tileColumns(100)).toBe(MAX_COLUMNS);
  });

  it('never returns zero columns, whatever it is asked', () => {
    // An empty grid still renders, and a zero here would divide by it.
    expect(tileColumns(0)).toBe(1);
  });
});
