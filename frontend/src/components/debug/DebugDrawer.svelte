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

    {#if store.debugOpen}
      <button
        class="ghost minimize"
        onclick={() => (store.debugOpen = false)}
        aria-label="Minimize debug panel"
        title="Minimize debug panel"
      >
        <svg viewBox="0 0 14 14" aria-hidden="true">
          <line x1="3" y1="9" x2="11" y2="9" />
        </svg>
      </button>
    {:else}
      <button class="ghost" onclick={() => (store.debugOpen = true)}>Debug ▴</button>
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
