<script lang="ts">
  /**
   * Camera and microphone selection during a call.
   *
   * Needed because the welcome screen is not always visited: joining by an
   * invite link goes straight into the room, and until this existed there was
   * no way to change devices at all once you were there.
   *
   * Switching rebuilds only the local capture pipeline — the MOQ publications
   * stay open — so a swap costs a keyframe, not a rejoin.
   */
  import { store } from '../lib/session.svelte';

  let open = $state(false);
  let busy = $state(false);
  let error = $state('');
  let root = $state<HTMLDivElement | null>(null);

  const cameras = $derived(store.devices.cameras);
  const microphones = $derived(store.devices.microphones);

  /** Applies the current selection, keeping the call up. */
  async function apply(): Promise<void> {
    busy = true;
    error = '';
    try {
      await store.applyMedia();
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }

  /**
   * Flips a capture track and republishes. Kept out of the panel because
   * muting is the one thing you reach for mid-sentence — it should not cost a
   * popover.
   */
  async function toggleTrack(kind: 'audio' | 'video'): Promise<void> {
    if (kind === 'audio') store.media.useAudio = !store.media.useAudio;
    else store.media.useVideo = !store.media.useVideo;
    await apply();
  }

  function toggle(): void {
    open = !open;
    // Device labels can change while a call runs — something plugged in, a
    // headset connected — so the list is refreshed each time it is shown.
    if (open) void store.refreshDevices();
  }

  // Close on an outside click or Escape, the way a popover is expected to.
  $effect(() => {
    if (!open) return;
    const onPointerDown = (ev: PointerEvent) => {
      if (root && !root.contains(ev.target as Node)) open = false;
    };
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key === 'Escape') open = false;
    };
    // Deferred to the next frame so the click that opened the panel does not
    // immediately close it again.
    const timer = setTimeout(() => {
      window.addEventListener('pointerdown', onPointerDown);
      window.addEventListener('keydown', onKey);
    }, 0);
    return () => {
      clearTimeout(timer);
      window.removeEventListener('pointerdown', onPointerDown);
      window.removeEventListener('keydown', onKey);
    };
  });
</script>

<div class="controls" bind:this={root}>
  {#if error && !open}
    <span class="note err" role="status">{error}</span>
  {/if}

  <button
    class="track"
    class:off={!store.media.useAudio}
    aria-pressed={store.media.useAudio}
    disabled={busy}
    onclick={() => toggleTrack('audio')}
    title={store.media.useAudio ? 'Turn the microphone off' : 'Turn the microphone on'}
  >
    Mic {store.media.useAudio ? 'on' : 'off'}
  </button>

  <button
    class="track"
    class:off={!store.media.useVideo}
    aria-pressed={store.media.useVideo}
    disabled={busy}
    onclick={() => toggleTrack('video')}
    title={store.media.useVideo ? 'Turn the camera off' : 'Turn the camera on'}
  >
    Cam {store.media.useVideo ? 'on' : 'off'}
  </button>

  <div class="wrap">
    <button onclick={toggle} aria-expanded={open} title="Camera and microphone">
      Devices {open ? '▾' : '▴'}
    </button>

    {#if open}
      <div class="panel" role="group" aria-label="Camera and microphone">
        <div class="field">
          <label for="call-cam">Camera</label>
          <select
            id="call-cam"
            bind:value={store.media.cameraId}
            onchange={apply}
            disabled={!store.media.useVideo || busy}
          >
            {#each cameras as cam (cam.deviceId)}
              <option value={cam.deviceId}>{cam.label || 'Camera'}</option>
            {/each}
          </select>
        </div>

        <div class="field">
          <label for="call-mic">Microphone</label>
          <select
            id="call-mic"
            bind:value={store.media.microphoneId}
            onchange={apply}
            disabled={!store.media.useAudio || busy}
          >
            {#each microphones as mic (mic.deviceId)}
              <option value={mic.deviceId}>{mic.label || 'Microphone'}</option>
            {/each}
          </select>
        </div>

        <div class="field">
          <label for="call-res">Resolution</label>
          <select
            id="call-res"
            bind:value={store.media.resolution}
            onchange={apply}
            disabled={!store.media.useVideo || busy}
          >
            <option value="640x360">640 × 360</option>
            <option value="854x480">854 × 480</option>
            <option value="1280x720">1280 × 720</option>
            <option value="1920x1080">1920 × 1080</option>
          </select>
        </div>

        <label class="toggle">
          <input
            type="checkbox"
            bind:checked={store.media.denoise}
            onchange={apply}
            disabled={!store.media.useAudio || busy}
          />
          Noise suppression
        </label>

        {#if busy}
          <p class="note">Switching…</p>
        {:else if error}
          <p class="note err">{error}</p>
        {/if}
      </div>
    {/if}
  </div>
</div>

<style>
  .controls {
    display: flex;
    align-items: center;
    gap: 6px;
    flex: none;
  }

  .wrap {
    position: relative;
    flex: none;
  }

  /* Mic and camera read as pressed-in when live, and go quiet — not alarming —
     when off, since being muted is a normal state rather than an error. */
  .track {
    padding: 7px 10px;
    font-size: 12px;
    border-color: var(--accent);
    background: var(--accent-dim);
  }

  .track.off {
    border-color: var(--border-strong);
    background: var(--bg-raised);
    color: var(--text-faint);
  }

  .panel {
    position: absolute;
    top: calc(100% + 6px);
    right: 0;
    z-index: 20;
    width: 260px;
    display: flex;
    flex-direction: column;
    gap: 10px;
    padding: 12px;
    background: var(--panel);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    box-shadow: 0 10px 30px rgba(0, 0, 0, 0.55);
  }

  .field {
    min-width: 0;
  }

  .toggle {
    display: flex;
    align-items: center;
    gap: 6px;
    margin: 0;
    font-size: 12px;
    color: var(--text);
  }

  .toggle input {
    width: auto;
  }

  .note {
    margin: 0;
    font-size: 11px;
    color: var(--text-faint);
  }

  .note.err {
    color: var(--err);
  }
</style>
