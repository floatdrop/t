<script lang="ts">
  /**
   * The in-call view: a tile per participant plus the local preview, and a
   * header carrying the room identity and the leave control.
   */
  import { store } from '../lib/session.svelte';
  import VideoTile from './VideoTile.svelte';

  const remotes = $derived(store.remotes);
  /** Column count grows with the participant count, capped so tiles stay large. */
  const columns = $derived(Math.min(3, Math.max(1, Math.ceil(Math.sqrt(remotes.length + 1)))));
</script>

<div class="conference">
  <header>
    <div class="identity">
      <span class="room">{store.session.room}</span>
      <span class="sep">·</span>
      <span class="relay mono">{store.session.relay}</span>
    </div>
    <div class="right">
      <span class="peers">
        {remotes.length === 0
          ? 'waiting for others to join'
          : `${remotes.length} other ${remotes.length === 1 ? 'participant' : 'participants'}`}
      </span>
      <button class="danger" onclick={() => store.leave()}>Leave</button>
    </div>
  </header>

  <div class="grid" style:grid-template-columns={`repeat(${columns}, minmax(0, 1fr))`}>
    <VideoTile
      nickname={store.session.nickname ?? 'me'}
      localStream={store.previewStream}
      hasAudio={!!store.previewStream?.getAudioTracks().length}
      speaking={store.speaking}
    />
    {#each remotes as remote (remote.id)}
      <VideoTile
        nickname={remote.nickname}
        videoHandle={remote.videoHandle}
        hasAudio={remote.audioHandle !== null}
        label={remote.id}
        speaking={remote.speaking}
      />
    {/each}
  </div>

  {#if !store.connected}
    <p class="banner">Backend disconnected — reconnecting…</p>
  {/if}
</div>

<style>
  .conference {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-height: 0;
  }

  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    /* Left padding clears the macOS traffic lights in the hidden-inset
       title bar. */
    padding: 10px 14px 10px 84px;
    border-bottom: 1px solid var(--border);
    flex: none;
  }

  .identity {
    display: flex;
    align-items: baseline;
    gap: 7px;
    min-width: 0;
  }

  .room {
    font-weight: 600;
  }

  .sep,
  .relay {
    color: var(--text-faint);
  }

  .right {
    display: flex;
    align-items: center;
    gap: 12px;
    flex: none;
  }

  .peers {
    font-size: 12px;
    color: var(--text-dim);
  }

  .grid {
    flex: 1;
    display: grid;
    gap: 10px;
    padding: 12px;
    overflow-y: auto;
    /* Centre the tiles when they fit, but fall back to top-aligned so an
       overflowing grid still scrolls from its first row. */
    align-content: safe center;
    min-height: 0;
  }

  .banner {
    margin: 0;
    padding: 6px 14px;
    background: color-mix(in srgb, var(--warn) 15%, transparent);
    color: var(--warn);
    font-size: 12px;
    flex: none;
  }
</style>
