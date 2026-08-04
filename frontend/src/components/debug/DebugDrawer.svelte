<script lang="ts">
  /**
   * Resizable bottom drawer holding the three debug panels. Kept mounted
   * while collapsed so the plots' one-minute history and the log tail
   * survive closing and reopening it.
   */
  import { store } from '../../lib/session.svelte';
  import LogPanel from './LogPanel.svelte';
  import MetricsPanel from './MetricsPanel.svelte';
  import TracksPanel from './TracksPanel.svelte';

  type Tab = 'metrics' | 'logs' | 'tracks';

  /**
   * `?debugTab=` picks the tab the drawer opens on, so a particular view can
   * be reproduced from the command line (see -debug-tab in main.go).
   */
  const REQUESTED: Record<string, Tab> = {
    transport: 'metrics',
    metrics: 'metrics',
    tracks: 'tracks',
    codecs: 'tracks',
    logs: 'logs',
  };
  const requested = new URLSearchParams(location.search).get('debugTab') ?? '';

  let tab = $state<Tab>(REQUESTED[requested] ?? 'metrics');
  let height = $state(320);
  let dragging = $state(false);

  /**
   * A tab reads as selected only while the drawer is open. Collapsed, there
   * is nothing on screen for it to be selected *of*, and highlighting one
   * would suggest the panel below is already showing it.
   */
  function isActive(candidate: Tab): boolean {
    return store.debugOpen && tab === candidate;
  }

  /** Selecting a tab is a request to see it, so it also opens the drawer. */
  function select(next: Tab): void {
    tab = next;
    store.debugOpen = true;
  }

  function startDrag(ev: PointerEvent): void {
    dragging = true;
    const startY = ev.clientY;
    const startHeight = height;
    const target = ev.currentTarget as HTMLElement;
    target.setPointerCapture(ev.pointerId);

    const move = (e: PointerEvent) => {
      // Dragging up grows the drawer, so the delta is inverted.
      height = Math.max(140, Math.min(window.innerHeight - 160, startHeight - (e.clientY - startY)));
    };
    const up = () => {
      dragging = false;
      target.releasePointerCapture(ev.pointerId);
      target.removeEventListener('pointermove', move);
      target.removeEventListener('pointerup', up);
    };
    target.addEventListener('pointermove', move);
    target.addEventListener('pointerup', up);
  }

  const errorCount = $derived(
    store.logs.filter((l) => l.level === 'ERROR').length + store.errors.length,
  );
</script>

<section class="drawer" class:open={store.debugOpen} style:height={store.debugOpen ? `${height}px` : 'auto'}>
  {#if store.debugOpen}
    <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
    <div
      class="grip"
      class:dragging
      onpointerdown={startDrag}
      role="separator"
      aria-orientation="horizontal"
      aria-label="Resize debug panel"
    ></div>
  {/if}

  <header>
    <nav>
      <button class="tab" class:active={isActive('metrics')} onclick={() => select('metrics')}>
        Transport
      </button>
      <button class="tab" class:active={isActive('tracks')} onclick={() => select('tracks')}>
        Tracks &amp; codecs
      </button>
      <button class="tab" class:active={isActive('logs')} onclick={() => select('logs')}>
        Logs
        {#if errorCount}<span class="badge">{errorCount}</span>{/if}
      </button>
    </nav>

    <!-- Only a close affordance is needed: the tabs themselves open the
         drawer, so a separate "Debug" button would be a second control for
         something the tab row already does. -->
    {#if store.debugOpen}
      <button
        class="ghost close"
        onclick={() => (store.debugOpen = false)}
        aria-label="Close debug panel"
        title="Close debug panel"
      >
        <svg viewBox="0 0 14 14" aria-hidden="true">
          <line x1="4" y1="4" x2="10" y2="10" />
          <line x1="10" y1="4" x2="4" y2="10" />
        </svg>
      </button>
    {/if}
  </header>

  {#if store.debugOpen}
    <div class="body">
      {#if tab === 'metrics'}
        <MetricsPanel />
      {:else if tab === 'tracks'}
        <TracksPanel />
      {:else}
        <LogPanel />
      {/if}
    </div>
  {/if}
</section>

<style>
  .drawer {
    flex: none;
    display: flex;
    flex-direction: column;
    background: var(--bg-raised);
    border-top: 1px solid var(--border);
    min-height: 0;
  }

  .grip {
    height: 5px;
    margin-top: -3px;
    cursor: ns-resize;
    background: transparent;
    flex: none;
  }

  .grip:hover,
  .grip.dragging {
    background: var(--accent);
  }

  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 4px 10px;
    flex: none;
  }

  nav {
    display: flex;
    gap: 2px;
  }

  .tab {
    background: transparent;
    border: none;
    border-bottom: 2px solid transparent;
    border-radius: 0;
    padding: 6px 10px;
    font-size: 12px;
    color: var(--text-dim);
  }

  .tab:hover {
    background: transparent;
    border-color: var(--border-strong);
    color: var(--text);
  }

  .tab.active {
    color: var(--text);
    border-bottom-color: var(--accent);
  }

  /* Square so the icon sits centred, and sized to stay a comfortable hit
     target even though the glyph itself is a pair of thin lines. */
  .close {
    display: grid;
    place-items: center;
    width: 26px;
    height: 26px;
    padding: 0;
    flex: none;
  }

  .close svg {
    width: 14px;
    height: 14px;
    stroke: currentColor;
    stroke-width: 1.5;
    stroke-linecap: round;
  }

  .badge {
    display: inline-block;
    margin-left: 5px;
    padding: 0 5px;
    border-radius: 8px;
    background: color-mix(in srgb, var(--err) 25%, transparent);
    color: var(--err);
    font-size: 10px;
    font-variant-numeric: tabular-nums;
  }

  .body {
    flex: 1;
    min-height: 0;
    border-top: 1px solid var(--border);
    overflow: hidden;
  }
</style>
