<script lang="ts">
  /**
   * One participant's tile. A remote participant's video is painted into a
   * canvas by the playback decoder; the local participant gets a <video>
   * element bound straight to the capture stream, which avoids decoding
   * our own feed just to show it back.
   */
  import { playback } from '../lib/playback';

  interface Props {
    nickname: string;
    /** Handle of the remote video track, or null for the local tile. */
    videoHandle?: number | null;
    hasAudio?: boolean;
    /** Set for the local tile; renders the capture stream directly. */
    localStream?: MediaStream | null;
    label?: string;
    /** Draws the voice-activity border. */
    speaking?: boolean;
  }

  let {
    nickname,
    videoHandle = null,
    hasAudio = false,
    localStream = null,
    label = '',
    speaking = false,
  }: Props = $props();

  let canvas = $state<HTMLCanvasElement | null>(null);
  let video = $state<HTMLVideoElement | null>(null);

  // Bind the canvas to its decoder whenever either changes, and release it
  // on teardown so the decoder stops holding a detached element.
  $effect(() => {
    if (videoHandle === null) return;
    const handle = videoHandle;
    playback.attachCanvas(handle, canvas);
    return () => playback.attachCanvas(handle, null);
  });

  $effect(() => {
    if (video && localStream) video.srcObject = localStream;
  });

  const isLocal = $derived(localStream !== null);
  const hasVideo = $derived(isLocal ? !!localStream?.getVideoTracks().length : videoHandle !== null);
</script>

<div class="tile" class:local={isLocal} class:speaking>
  {#if isLocal && hasVideo}
    <!-- svelte-ignore a11y_media_has_caption -->
    <video bind:this={video} autoplay muted playsinline></video>
  {:else if videoHandle !== null}
    <canvas bind:this={canvas}></canvas>
  {:else}
    <div class="placeholder">
      <div class="initial">{nickname.slice(0, 1).toUpperCase()}</div>
      <span>camera off</span>
    </div>
  {/if}

  <div class="overlay">
    <span class="name">{nickname}{isLocal ? ' (you)' : ''}</span>
    <span class="badges">
      {#if speaking}<span class="badge speaking-badge" title="speaking">●</span>{/if}
      {#if hasAudio}<span class="badge" title="publishing audio">🔊</span>{/if}
      {#if label}<span class="badge mono">{label}</span>{/if}
    </span>
  </div>
</div>

<style>
  .tile {
    position: relative;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    overflow: hidden;
    aspect-ratio: 16 / 9;
    min-width: 0;
    transition: box-shadow 120ms ease, border-color 120ms ease;
  }

  @media (prefers-reduced-motion: reduce) {
    .tile {
      transition: none;
    }
  }

  .tile.local {
    border-color: var(--accent-dim);
  }

  /* Voice activity. The ring is drawn as an inset box-shadow rather than a
     border so turning it on never changes the tile's box size and cannot
     reflow the grid mid-call. */
  .tile.speaking {
    border-color: var(--ok);
    box-shadow:
      inset 0 0 0 2px var(--ok),
      0 0 12px color-mix(in srgb, var(--ok) 35%, transparent);
  }

  .speaking-badge {
    color: var(--ok);
    font-size: 9px;
  }

  canvas,
  video {
    width: 100%;
    height: 100%;
    object-fit: cover;
    display: block;
  }

  /* Mirror only our own feed — remote participants must not be flipped. */
  .tile.local video {
    transform: scaleX(-1);
  }

  .placeholder {
    height: 100%;
    display: grid;
    place-content: center;
    justify-items: center;
    gap: 8px;
    color: var(--text-faint);
    font-size: 12px;
  }

  .initial {
    width: 52px;
    height: 52px;
    border-radius: 50%;
    background: var(--panel);
    display: grid;
    place-items: center;
    font-size: 20px;
    font-weight: 600;
    color: var(--text-dim);
  }

  .overlay {
    position: absolute;
    inset: auto 0 0 0;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 6px 9px;
    background: linear-gradient(transparent, rgba(0, 0, 0, 0.72));
    font-size: 12px;
  }

  .name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .badges {
    display: inline-flex;
    gap: 5px;
    flex: none;
  }

  .badge {
    font-size: 11px;
    color: var(--text-dim);
  }
</style>
