<script lang="ts">
  /**
   * The backend log, streamed live from moq-go and the app's own packages.
   *
   * Raising the level here changes it in the backend process, which is how
   * you get moq-go's per-message DEBUG output — the SUBSCRIBE / PUBLISH /
   * subgroup traffic — without restarting a call.
   */
  import { store } from '../../lib/session.svelte';

  const LEVELS = ['DEBUG', 'INFO', 'WARN', 'ERROR'];

  /** Records at or above this rank are shown. */
  const RANK: Record<string, number> = { DEBUG: 0, INFO: 1, WARN: 2, ERROR: 3 };

  let filter = $state('');
  let minLevel = $state('DEBUG');
  let autoScroll = $state(true);
  let list = $state<HTMLDivElement | null>(null);

  const visible = $derived.by(() => {
    const needle = filter.trim().toLowerCase();
    const floor = RANK[minLevel] ?? 0;
    return store.logs.filter((entry) => {
      if ((RANK[entry.level] ?? 1) < floor) return false;
      if (!needle) return true;
      if (entry.msg.toLowerCase().includes(needle)) return true;
      return Object.entries(entry.attrs ?? {}).some(
        ([k, v]) => k.toLowerCase().includes(needle) || v.toLowerCase().includes(needle),
      );
    });
  });

  // Follow the tail as records arrive, unless the reader has turned that off.
  $effect(() => {
    void visible.length;
    if (autoScroll && list) list.scrollTop = list.scrollHeight;
  });

  function clockOf(ms: number): string {
    const d = new Date(ms);
    const pad = (n: number, w = 2) => String(n).padStart(w, '0');
    return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`;
  }
</script>

<div class="log">
  <div class="controls">
    <label class="inline-label" for="backend-level">Backend level</label>
    <select
      id="backend-level"
      value={store.logLevel}
      onchange={(e) => store.setLogLevel((e.currentTarget as HTMLSelectElement).value)}
    >
      {#each LEVELS as level (level)}
        <option value={level}>{level}</option>
      {/each}
    </select>

    <label class="inline-label" for="show-level">Show</label>
    <select id="show-level" bind:value={minLevel}>
      {#each LEVELS as level (level)}
        <option value={level}>{level}+</option>
      {/each}
    </select>

    <input class="filter" placeholder="filter…" bind:value={filter} spellcheck="false" />

    <label class="check">
      <input type="checkbox" bind:checked={autoScroll} /> follow
    </label>
    <label class="check">
      <input type="checkbox" bind:checked={store.logPaused} /> pause
    </label>
    <button class="ghost" onclick={() => store.clearLogs()}>clear</button>
    <span class="count">{visible.length}/{store.logs.length}</span>
  </div>

  <div class="list" bind:this={list}>
    {#each visible as entry, i (i)}
      <div class="row lvl-{entry.level.toLowerCase()}">
        <span class="time">{clockOf(entry.t)}</span>
        <span class="level">{entry.level}</span>
        <span class="msg">{entry.msg}</span>
        {#if entry.attrs}
          <span class="attrs">
            {#each Object.entries(entry.attrs) as [key, value] (key)}
              <span class="attr"><span class="key">{key}</span>={value}</span>
            {/each}
          </span>
        {/if}
      </div>
    {/each}
    {#if visible.length === 0}
      <p class="empty">No records match.</p>
    {/if}
  </div>
</div>

<style>
  .log {
    height: 100%;
    display: flex;
    flex-direction: column;
    min-height: 0;
  }

  .controls {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    border-bottom: 1px solid var(--border);
    flex-wrap: wrap;
  }

  .inline-label {
    margin: 0;
    font-size: 11px;
    color: var(--text-faint);
    white-space: nowrap;
  }

  .controls select {
    width: auto;
    padding: 3px 6px;
    font-size: 11px;
  }

  .filter {
    width: auto;
    flex: 1;
    min-width: 100px;
    padding: 3px 8px;
    font-size: 11px;
  }

  .check {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    margin: 0;
    font-size: 11px;
    color: var(--text-dim);
    white-space: nowrap;
  }

  .check input {
    width: auto;
  }

  .count {
    font-family: var(--mono);
    font-size: 10px;
    color: var(--text-faint);
    white-space: nowrap;
  }

  .list {
    flex: 1;
    overflow-y: auto;
    padding: 4px 0;
    background: var(--bg-sunken);
    font-family: var(--mono);
    font-size: 11px;
    line-height: 1.65;
  }

  .row {
    display: flex;
    gap: 8px;
    padding: 0 12px;
    white-space: pre-wrap;
    word-break: break-word;
  }

  .row:hover {
    background: var(--bg-raised);
  }

  .time {
    color: var(--text-faint);
    flex: none;
  }

  .level {
    flex: none;
    width: 42px;
    color: var(--text-faint);
  }

  /* Level colors are status, not series — each is paired with its own
     printed level name, so meaning never rests on hue alone. */
  .lvl-warn .level {
    color: var(--warn);
  }

  .lvl-error .level {
    color: var(--err);
  }

  .lvl-debug {
    color: var(--text-dim);
  }

  .msg {
    flex: none;
  }

  .attrs {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    color: var(--text-dim);
  }

  .attr .key {
    color: var(--text-faint);
  }

  .empty {
    padding: 16px 12px;
    color: var(--text-faint);
    font-family: var(--sans);
  }
</style>
