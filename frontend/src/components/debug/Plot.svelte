<script lang="ts">
  /**
   * One live time-series plot, drawn on a canvas.
   *
   * Canvas rather than SVG because these update four times a second for as
   * long as a call lasts: re-rendering a few hundred SVG nodes at that rate
   * churns the DOM for no benefit, while a canvas repaint is a fixed cost.
   *
   * Series colors come from the validated categorical order (blue, orange,
   * aqua — the three slots that clear the all-pairs CVD gates on this dark
   * surface). Every label, value and tick is drawn in a text token, never
   * in a series color; identity comes from the swatch beside the text.
   */

  interface Series {
    label: string;
    /** One of the CATEGORICAL slots below. */
    color: string;
    values: number[];
  }

  interface Props {
    title: string;
    unit: string;
    series: Series[];
    /** Forces the y-axis top, for scales with a meaningful ceiling. */
    max?: number | null;
    /** Decimal places in labels and the tooltip. */
    precision?: number;
    height?: number;
  }

  let { title, unit, series, max = null, precision = 0, height = 92 }: Props = $props();

  /** Ink tokens — text never wears a series color. */
  const TEXT_SECONDARY = '#98a1b3';
  const TEXT_MUTED = '#646d80';
  const SURFACE = '#080a0e';
  const GRID = '#1c2130';

  let canvas = $state<HTMLCanvasElement | null>(null);
  let wrapper = $state<HTMLDivElement | null>(null);
  let hoverIndex = $state<number | null>(null);
  let hoverX = $state(0);

  const sampleCount = $derived(Math.max(...series.map((s) => s.values.length), 0));

  /** Latest value per series, for the direct end-labels. */
  const latest = $derived(
    series.map((s) => (s.values.length ? s.values[s.values.length - 1] : 0)),
  );

  function format(v: number): string {
    if (!Number.isFinite(v)) return '—';
    if (Math.abs(v) >= 10_000) return `${(v / 1000).toFixed(1)}k`;
    return v.toFixed(precision);
  }

  /** Rounds a scale top up to 1, 2 or 5 × a power of ten. */
  function niceTop(value: number): number {
    if (value <= 0) return 1;
    const magnitude = 10 ** Math.floor(Math.log10(value));
    const scaled = value / magnitude;
    const step = scaled <= 1 ? 1 : scaled <= 2 ? 2 : scaled <= 5 ? 5 : 10;
    return step * magnitude;
  }

  const top = $derived.by(() => {
    if (max !== null) return max;
    let peak = 0;
    for (const s of series) for (const v of s.values) if (Number.isFinite(v) && v > peak) peak = v;
    return niceTop(peak * 1.15);
  });

  // Repaint whenever the data, the scale, or the hover position changes.
  $effect(() => {
    // Referenced so the effect re-runs on new samples.
    void series;
    void top;
    void hoverIndex;
    draw();
  });

  $effect(() => {
    if (!wrapper) return;
    const observer = new ResizeObserver(() => draw());
    observer.observe(wrapper);
    return () => observer.disconnect();
  });

  function draw(): void {
    const el = canvas;
    if (!el || !wrapper) return;

    const dpr = window.devicePixelRatio || 1;
    const width = wrapper.clientWidth;
    if (width <= 0) return;
    if (el.width !== Math.round(width * dpr) || el.height !== Math.round(height * dpr)) {
      el.width = Math.round(width * dpr);
      el.height = Math.round(height * dpr);
    }
    const ctx = el.getContext('2d');
    if (!ctx) return;

    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, width, height);

    // Leave room on the right for the end marker and its ring.
    const padRight = 7;
    const padBottom = 14;
    const plotW = width - padRight;
    const plotH = height - padBottom;

    // Gridlines: hairline, solid, one step off the surface, recessive.
    ctx.strokeStyle = GRID;
    ctx.lineWidth = 1;
    ctx.setLineDash([]);
    for (const frac of [0, 0.5, 1]) {
      const y = Math.round(plotH * frac) + 0.5;
      ctx.beginPath();
      ctx.moveTo(0, y);
      ctx.lineTo(plotW, y);
      ctx.stroke();
    }

    // Axis ticks carry the values the end-labels don't.
    ctx.fillStyle = TEXT_MUTED;
    ctx.font = '10px ui-monospace, SFMono-Regular, Menlo, monospace';
    ctx.textAlign = 'left';
    ctx.textBaseline = 'top';
    ctx.fillText(format(top), 2, 2);
    ctx.textBaseline = 'bottom';
    ctx.fillText('0', 2, plotH - 2);

    if (sampleCount < 2) {
      ctx.fillStyle = TEXT_MUTED;
      ctx.textAlign = 'center';
      ctx.textBaseline = 'middle';
      ctx.fillText('waiting for samples…', plotW / 2, plotH / 2);
      return;
    }

    const xAt = (i: number) => (i / (sampleCount - 1)) * plotW;
    const yAt = (v: number) => {
      const clamped = Number.isFinite(v) ? Math.max(0, Math.min(v, top)) : 0;
      return plotH - (clamped / top) * plotH;
    };

    for (const s of series) {
      if (s.values.length < 2) continue;
      // Align a short series to the right edge so all series share "now".
      const offset = sampleCount - s.values.length;

      // Area wash at ~10% — a hint of volume, never a saturated block.
      ctx.beginPath();
      ctx.moveTo(xAt(offset), plotH);
      s.values.forEach((v, i) => ctx.lineTo(xAt(offset + i), yAt(v)));
      ctx.lineTo(xAt(sampleCount - 1), plotH);
      ctx.closePath();
      ctx.globalAlpha = 0.1;
      ctx.fillStyle = s.color;
      ctx.fill();
      ctx.globalAlpha = 1;

      ctx.beginPath();
      s.values.forEach((v, i) => {
        const x = xAt(offset + i);
        const y = yAt(v);
        if (i === 0) ctx.moveTo(x, y);
        else ctx.lineTo(x, y);
      });
      ctx.strokeStyle = s.color;
      ctx.lineWidth = 2;
      ctx.lineJoin = 'round';
      ctx.lineCap = 'round';
      ctx.stroke();

      // End marker: r=4 (8px), with a 2px surface ring so overlapping
      // series stay legible where they cross.
      const lastX = xAt(sampleCount - 1);
      const lastY = yAt(s.values[s.values.length - 1]);
      ctx.beginPath();
      ctx.arc(lastX, lastY, 4, 0, Math.PI * 2);
      ctx.fillStyle = s.color;
      ctx.fill();
      ctx.lineWidth = 2;
      ctx.strokeStyle = SURFACE;
      ctx.stroke();
    }

    if (hoverIndex !== null && hoverIndex >= 0 && hoverIndex < sampleCount) {
      const x = Math.round(xAt(hoverIndex)) + 0.5;
      ctx.strokeStyle = TEXT_SECONDARY;
      ctx.lineWidth = 1;
      ctx.globalAlpha = 0.5;
      ctx.beginPath();
      ctx.moveTo(x, 0);
      ctx.lineTo(x, plotH);
      ctx.stroke();
      ctx.globalAlpha = 1;

      for (const s of series) {
        const offset = sampleCount - s.values.length;
        const i = hoverIndex - offset;
        if (i < 0 || i >= s.values.length) continue;
        ctx.beginPath();
        ctx.arc(xAt(hoverIndex), yAt(s.values[i]), 4, 0, Math.PI * 2);
        ctx.fillStyle = s.color;
        ctx.fill();
        ctx.lineWidth = 2;
        ctx.strokeStyle = SURFACE;
        ctx.stroke();
      }
    }
  }

  function onMove(ev: MouseEvent): void {
    if (!wrapper || sampleCount < 2) return;
    const rect = wrapper.getBoundingClientRect();
    const plotW = rect.width - 7;
    const ratio = Math.max(0, Math.min(1, (ev.clientX - rect.left) / plotW));
    hoverIndex = Math.round(ratio * (sampleCount - 1));
    hoverX = ev.clientX - rect.left;
  }

  /** Seconds ago for the hovered sample, at the 250 ms metrics cadence. */
  const hoverAgo = $derived(
    hoverIndex === null ? '' : `${(((sampleCount - 1 - hoverIndex) * 250) / 1000).toFixed(1)}s ago`,
  );

  const hoverValues = $derived(
    hoverIndex === null
      ? []
      : series.map((s) => {
          const i = hoverIndex! - (sampleCount - s.values.length);
          return {
            label: s.label,
            color: s.color,
            value: i >= 0 && i < s.values.length ? s.values[i] : NaN,
          };
        }),
  );
</script>

<figure class="plot">
  <figcaption>
    <span class="title">{title}</span>
    <span class="values">
      {#each series as s, i (s.label)}
        <span class="value">
          <!-- The swatch carries identity; the number stays in ink. -->
          <span class="swatch" style:background={s.color}></span>
          <span class="num">{format(latest[i])}</span>
        </span>
      {/each}
      <span class="unit">{unit}</span>
    </span>
  </figcaption>

  <div
    class="canvas-wrap"
    bind:this={wrapper}
    style:height={`${height}px`}
    role="img"
    aria-label={`${title} in ${unit}: ${series.map((s, i) => `${s.label} ${format(latest[i])}`).join(', ')}`}
    onmousemove={onMove}
    onmouseleave={() => (hoverIndex = null)}
  >
    <canvas bind:this={canvas}></canvas>

    {#if hoverIndex !== null}
      <div
        class="tooltip"
        style:left={`${hoverX}px`}
        style:transform={hoverX > (wrapper?.clientWidth ?? 0) / 2
          ? 'translateX(calc(-100% - 10px))'
          : 'translateX(10px)'}
      >
        <div class="tip-when">{hoverAgo}</div>
        {#each hoverValues as row (row.label)}
          <div class="tip-row">
            <span class="swatch" style:background={row.color}></span>
            <span class="tip-label">{row.label}</span>
            <span class="tip-value">{format(row.value)}</span>
          </div>
        {/each}
      </div>
    {/if}
  </div>

  {#if series.length > 1}
    <!-- Two or more series always get a legend: identity is never left to
         color-matching alone. -->
    <div class="legend">
      {#each series as s (s.label)}
        <span class="key">
          <span class="swatch" style:background={s.color}></span>
          {s.label}
        </span>
      {/each}
    </div>
  {/if}
</figure>

<style>
  .plot {
    margin: 0;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 8px 10px 6px;
    min-width: 0;
  }

  figcaption {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 8px;
    margin-bottom: 4px;
  }

  .title {
    font-size: 11px;
    color: var(--text-dim);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .values {
    display: inline-flex;
    align-items: baseline;
    gap: 8px;
    flex: none;
  }

  .value {
    display: inline-flex;
    align-items: baseline;
    gap: 4px;
  }

  .num {
    font-family: var(--mono);
    font-size: 13px;
    font-variant-numeric: tabular-nums;
    color: var(--text);
  }

  .unit {
    font-size: 10px;
    color: var(--text-faint);
  }

  .canvas-wrap {
    position: relative;
  }

  canvas {
    display: block;
    width: 100%;
    height: 100%;
  }

  .swatch {
    width: 8px;
    height: 8px;
    border-radius: 2px;
    display: inline-block;
    flex: none;
    align-self: center;
  }

  .legend {
    display: flex;
    flex-wrap: wrap;
    gap: 10px;
    margin-top: 4px;
  }

  .key {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-size: 10px;
    color: var(--text-dim);
  }

  .tooltip {
    position: absolute;
    top: 0;
    pointer-events: none;
    background: var(--panel);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
    padding: 5px 7px;
    font-size: 11px;
    white-space: nowrap;
    z-index: 5;
    box-shadow: 0 4px 14px rgba(0, 0, 0, 0.5);
  }

  .tip-when {
    color: var(--text-faint);
    font-size: 10px;
    margin-bottom: 3px;
  }

  .tip-row {
    display: flex;
    align-items: center;
    gap: 6px;
  }

  .tip-label {
    color: var(--text-dim);
  }

  .tip-value {
    font-family: var(--mono);
    font-variant-numeric: tabular-nums;
    color: var(--text);
    margin-left: auto;
  }
</style>
