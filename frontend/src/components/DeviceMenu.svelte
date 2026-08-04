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
  let panel = $state<HTMLDivElement | null>(null);

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
      if (panel && !panel.contains(ev.target as Node)) open = false;
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

<div class="wrap" bind:this={panel}>
  <button onclick={toggle} aria-expanded={open} title="Camera and microphone">
    Devices {open ? '▾' : '▴'}
  </button>

  {#if open}
    <div class="panel" role="group" aria-label="Camera and microphone">
      <div class="field">
        <label for="call-cam">
          <input
            type="checkbox"
            class="inline"
            bind:checked={store.media.useVideo}
            onchange={apply}
          /> Camera
        </label>
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
        <label for="call-mic">
          <input
            type="checkbox"
            class="inline"
            bind:checked={store.media.useAudio}
            onchange={apply}
          /> Microphone
        </label>
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

<style>
  .wrap {
    position: relative;
    flex: none;
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

  input.inline {
    width: auto;
    margin-right: 4px;
    vertical-align: -1px;
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
