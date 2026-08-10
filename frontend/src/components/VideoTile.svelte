<script lang="ts">
  /**
   * One participant's tile. A remote participant's video is painted into a
   * canvas by the playback decoder; the local participant gets a <video>
   * element bound straight to the capture stream, which avoids decoding
   * our own feed just to show it back.
   */
  import Maximize from '@lucide/svelte/icons/maximize';
  import Minimize from '@lucide/svelte/icons/minimize';
  import type { VideoSource } from '../lib/capture';
  import { ICON_SIZE } from '../lib/icons';
  import { playback } from '../lib/playback';

  interface Props {
    nickname: string;
    /** Handle of the remote video track, or null for the local tile. */
    videoHandle?: number | null;
    hasAudio?: boolean;
    /** Set for the local tile; renders the capture stream directly. */
    localStream?: MediaStream | null;
    /**
     * Identity of the local stream's video track, or '' when it carries none.
     *
     * Passed rather than read off localStream, whose track list changes in
     * place when a device is switched: the stream object stays the same, so a
     * camera swap has nothing else to announce it.
     */
    localVideoId?: string;
    /**
     * What the local video is showing. A screen must not be treated like a
     * face: flipping it makes text read backwards, and cropping it hides the
     * edges of whatever is being shared.
     */
    localSource?: VideoSource;
    label?: string;
    /**
     * How much of this participant's video we are taking. Anything but "full"
     * is the backend having reduced it to fit the connection, which the tile
     * says out loud — otherwise it is indistinguishable from a camera that is
     * off, and the picture quietly getting worse for no visible reason is the
     * thing people report as a bug about the wrong subsystem.
     */
    videoLevel?: string;
    /**
     * The build this participant is running. Shown on hover rather than as a
     * badge: it matters on the day two people see different things and nowhere
     * else, and a version on every tile is a permanent tax for that day.
     */
    version?: string;
    /** Draws the voice-activity border. */
    speaking?: boolean;
    /** True when this tile has the window to itself. */
    expanded?: boolean;
    /** Set to offer the expand control; called with the wanted state. */
    onExpand?: (expanded: boolean) => void;
  }

  let {
    nickname,
    videoHandle = null,
    hasAudio = false,
    localStream = null,
    localVideoId = '',
    localSource = 'camera',
    label = '',
    videoLevel = 'full',
    version = '',
    speaking = false,
    expanded = false,
    onExpand,
  }: Props = $props();

  let canvas = $state<HTMLCanvasElement | null>(null);
  let video = $state<HTMLVideoElement | null>(null);
  let root = $state<HTMLDivElement | null>(null);

  /**
   * Takes the tile to the whole screen, not merely the whole window.
   *
   * The control used to do the latter and was labelled as the former: it set a
   * flag the grid reads, so the tile filled the window and the menu bar, the
   * title bar and everything behind them stayed exactly where they were.
   *
   * The in-window expansion is still what happens first, and is still what is
   * left if the request is refused — WebKit gates element fullscreen and can
   * simply say no, which is not a reason to leave the button doing nothing. So
   * the flag is set either way and the screen is asked for on top of it.
   */
  async function toggleExpand(): Promise<void> {
    if (expanded) {
      onExpand?.(false);
      if (document.fullscreenElement) {
        await document.exitFullscreen().catch(() => {});
      }
      return;
    }
    onExpand?.(true);
    try {
      await root?.requestFullscreen();
    } catch {
      // Refused, or unsupported. The tile still has the window.
    }
  }

  /**
   * Leaving fullscreen by any other route — Escape, the green button, a swipe —
   * has to put the grid back, or the tile stays expanded inside a window that
   * is no longer full and nothing looks like it is in a state anyone chose.
   */
  $effect(() => {
    const onChange = () => {
      if (!document.fullscreenElement && expanded) onExpand?.(false);
    };
    document.addEventListener('fullscreenchange', onChange);
    return () => document.removeEventListener('fullscreenchange', onChange);
  });

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
  const hasVideo = $derived(isLocal ? localVideoId !== '' : videoHandle !== null);
</script>

<!-- data-participant is what the grid's visibility observer keys on, so a tile
     scrolled out of view can stop being paid for. Absent on the local tile,
     which is drawn from the camera and was never subscribed to. -->
<div
  bind:this={root}
  class="tile"
  data-participant={label || undefined}
  class:local={isLocal}
  class:speaking
  class:screen={localSource === 'screen'}
  class:expanded
>
  {#if isLocal && hasVideo}
    <!-- Keyed on the camera, so switching one gives a fresh element to bind:
         the stream it is attached to is reused across the swap, and re-setting
         srcObject to the object it already holds is not guaranteed to restart
         anything. A microphone change leaves the key alone, and with it the
         picture. -->
    {#key localVideoId}
      <!-- svelte-ignore a11y_media_has_caption -->
      <video bind:this={video} autoplay muted playsinline></video>
    {/key}
  {:else if videoHandle !== null}
    <canvas bind:this={canvas}></canvas>
  {:else}
    <div class="placeholder">
      <div class="initial">{nickname.slice(0, 1).toUpperCase()}</div>
      <span>camera off</span>
    </div>
  {/if}

  {#if onExpand}
    <!-- Kept out of the overlay so it stays in the corner opposite the name,
         and shown on hover or while expanded — a control that got you into this
         view has to be visible enough to get you back out. -->
    <button
      class="expand"
      aria-label={expanded ? 'Restore the grid' : 'Fill the screen'}
      aria-pressed={expanded}
      title={expanded ? 'Restore the grid (Esc)' : 'Fill the screen'}
      onclick={toggleExpand}
    >
      {#if expanded}
        <Minimize size={ICON_SIZE} />
      {:else}
        <Maximize size={ICON_SIZE} />
      {/if}
    </button>
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

  /* A shared screen is not a self-view. Unflipped so text reads the right way
     round, and contained rather than cropped so the edges of what is being
     shared are actually in shot. */
  .tile.local.screen video {
    transform: none;
    object-fit: contain;
  }

  /* Expanded, the tile stops being a thumbnail in a grid: it takes the height
     it is given rather than holding 16/9, and nothing is cropped, since seeing
     all of what someone is showing is the entire point of expanding it. */
  .tile.expanded {
    aspect-ratio: auto;
    height: 100%;
  }

  /* Fullscreen gives the element the screen but not the shape: without this it
     keeps the grid's rounded corners and its own background, so a black bar of
     tile sits inside a black screen and the picture is still letterboxed by a
     border nobody can see the point of. */
  .tile:fullscreen {
    width: 100vw;
    height: 100vh;
    border-radius: 0;
    border: none;
    aspect-ratio: auto;
  }

  .tile:fullscreen :is(video, canvas) {
    object-fit: contain;
  }

  .tile.expanded :is(video, canvas) {
    object-fit: contain;
  }

  /* Top-right, opposite the name. Faint until the tile is hovered so a wall of
     tiles is not a wall of buttons. */
  .expand {
    position: absolute;
    top: 6px;
    right: 6px;
    z-index: 1;
    padding: 5px;
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--bg-sunken) 70%, transparent);
    border-color: transparent;
    color: var(--text-dim);
    opacity: 0;
    transition: opacity 120ms ease, color 120ms ease, border-color 120ms ease;
  }

  .tile:hover .expand,
  .expand:focus-visible,
  .tile.expanded .expand {
    opacity: 1;
  }

  .expand:hover:not(:disabled) {
    color: var(--text);
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

  /* The one badge that is not neutral information: video this client had to
     give up on entirely, which otherwise reads as a camera that is off. */
</style>
