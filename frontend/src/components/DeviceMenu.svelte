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
  // Imported one icon at a time rather than from the barrel, so only the seven
  // that are used reach the bundle.
  import Mic from '@lucide/svelte/icons/mic';
  import MicOff from '@lucide/svelte/icons/mic-off';
  import ScreenShare from '@lucide/svelte/icons/screen-share';
  import ScreenShareOff from '@lucide/svelte/icons/screen-share-off';
  import Settings2 from '@lucide/svelte/icons/settings-2';
  import Video from '@lucide/svelte/icons/video';
  import VideoOff from '@lucide/svelte/icons/video-off';
  import { rungValue, VIDEO_LADDER } from '../lib/capture';
  import { ICON_SIZE } from '../lib/icons';
  import { store } from '../lib/session.svelte';

  let open = $state(false);
  let busy = $state(false);
  let error = $state('');
  let root = $state<HTMLDivElement | null>(null);

  /** The camera is only what anyone sees when it is not standing behind a share. */
  const cameraLive = $derived(store.media.useVideo && store.media.videoSource === 'camera');

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
   *
   * The camera case has three states to reconcile, not two: pressing it while
   * the screen is being shared means "show me instead", which is a switch back
   * rather than a turn-off.
   */
  async function toggleTrack(kind: 'audio' | 'video'): Promise<void> {
    if (kind === 'audio') {
      store.media.useAudio = !store.media.useAudio;
    } else if (store.sharingScreen) {
      store.media.videoSource = 'camera';
      store.media.useVideo = true;
    } else {
      store.media.useVideo = !store.media.useVideo;
    }
    await apply();
  }

  /**
   * Starts or stops sharing the screen.
   *
   * Deliberately not routed through apply(): startScreenShare has to reach
   * getDisplayMedia while this click's activation is still valid, so it applies
   * the settings itself and this only handles the reporting around it.
   */
  async function toggleScreen(): Promise<void> {
    busy = true;
    error = '';
    try {
      if (store.sharingScreen) await store.stopScreenShare();
      else await store.startScreenShare();
    } catch (err) {
      // Cancelling the picker rejects with NotAllowedError, which is a choice
      // rather than a fault, so it is not worth shouting about.
      const name = err instanceof DOMException ? err.name : '';
      if (name !== 'NotAllowedError' && name !== 'AbortError') {
        error = err instanceof Error ? err.message : String(err);
      }
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
    <span class="note err inline" role="status" title={error}>{error}</span>
  {/if}

  <!-- Named for the device, not the action, because aria-pressed already
       carries the state: "Microphone, pressed" is what a toggle should
       announce, where "Turn the microphone off, pressed" contradicts itself.
       The action stays in the tooltip, which is where a sighted user looks. -->
  <button
    class="track"
    class:off={!store.media.useAudio}
    aria-label="Microphone"
    aria-pressed={store.media.useAudio}
    disabled={busy}
    onclick={() => toggleTrack('audio')}
    title={store.media.useAudio ? 'Turn the microphone off' : 'Turn the microphone on'}
  >
    {#if store.media.useAudio}
      <Mic size={ICON_SIZE} />
    {:else}
      <MicOff size={ICON_SIZE} />
    {/if}
  </button>

  <!-- Reads as off while the screen is being shared, because the camera is not
       what anyone is seeing. -->
  <button
    class="track"
    class:off={!cameraLive}
    aria-label="Camera"
    aria-pressed={cameraLive}
    disabled={busy}
    onclick={() => toggleTrack('video')}
    title={cameraLive
      ? 'Turn the camera off'
      : store.sharingScreen
        ? 'Show the camera instead of the screen'
        : 'Turn the camera on'}
  >
    {#if cameraLive}
      <Video size={ICON_SIZE} />
    {:else}
      <VideoOff size={ICON_SIZE} />
    {/if}
  </button>

  <button
    class="track"
    class:off={!store.sharingScreen}
    aria-label="Share screen"
    aria-pressed={store.sharingScreen}
    disabled={busy}
    onclick={toggleScreen}
    title={store.sharingScreen ? 'Stop sharing the screen' : 'Share the screen'}
  >
    {#if store.sharingScreen}
      <ScreenShare size={ICON_SIZE} />
    {:else}
      <ScreenShareOff size={ICON_SIZE} />
    {/if}
  </button>

  <div class="wrap">
    <button
      class="devices"
      onclick={toggle}
      aria-label="Devices"
      aria-expanded={open}
      title="Camera and microphone"
    >
      <Settings2 size={ICON_SIZE} />
      <!-- The panel drops below, so closed points down at where it will
           appear and open points back up at the button. -->
      <span class="caret" aria-hidden="true">{open ? '▴' : '▾'}</span>
    </button>

    {#if open}
      <div class="panel" role="group" aria-label="Camera and microphone">
        <div class="field">
          <label for="call-cam">Camera</label>
          <!-- Disabled while sharing rather than left to do nothing: the video
               is not coming from a camera, and picking one here is deliberately
               not a way to change that. Same for the resolution below — a
               screen share carries its own, see screenVideoSettings. -->
          <select
            id="call-cam"
            bind:value={store.media.cameraId}
            onchange={apply}
            disabled={!cameraLive || busy}
            title={store.sharingScreen ? 'The screen is being shared' : ''}
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
            disabled={!cameraLive || busy}
          >
            {#each VIDEO_LADDER as rung (rung.label)}
              <option value={rungValue(rung)}>{rung.label}</option>
            {/each}
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
     when off, since being muted is a normal state rather than an error. The
     slashed-through icon says which state it is; the colour only agrees with
     it. */
  .track {
    padding: 7px;
    border-color: var(--accent);
    background: var(--accent-dim);
  }

  /* As tall as a button carrying one line of text, so the icon-only controls
     sit level with Copy invite and Leave beside them rather than 6px short.
     Expressed in em so it follows the font rather than a measured pixel. */
  .track,
  .devices {
    min-height: calc(1.5em + 16px);
  }

  .track.off {
    border-color: var(--border-strong);
    background: var(--bg-raised);
    color: var(--text-faint);
  }

  .devices {
    padding: 7px 8px;
    gap: 3px;
  }

  .caret {
    font-size: 10px;
    line-height: 1;
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

  /* Capped in the header, where the row does not shrink and a long message
     from getUserMedia would otherwise push the controls off the edge. The
     tooltip keeps the whole of it. */
  .note.inline {
    max-width: 22ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
