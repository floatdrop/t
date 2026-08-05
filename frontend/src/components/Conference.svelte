<script lang="ts">
  /**
   * The in-call view: a tile per participant plus the local preview, and a
   * header carrying the room identity and the leave control.
   */
  import Check from '@lucide/svelte/icons/check';
  import Link from '@lucide/svelte/icons/link';
  import PhoneOff from '@lucide/svelte/icons/phone-off';
  import { bridge } from '../lib/bridge';
  import { ICON_SIZE } from '../lib/icons';
  import { buildInviteLink, copyText } from '../lib/invite';
  import { store } from '../lib/session.svelte';
  import DeviceMenu from './DeviceMenu.svelte';
  import VideoTile from './VideoTile.svelte';

  /** How long the button confirms a copy before returning to its label. */
  const COPIED_FEEDBACK_MS = 1600;

  /**
   * A link is only shareable if it carries both halves — the relay address is
   * what makes the room reachable at all, so a link without it is useless.
   */
  const canInvite = $derived(!!store.session.relay && !!store.session.room);

  let copied = $state(false);
  let copyFailed = $state(false);
  let copyTimer: ReturnType<typeof setTimeout> | undefined;

  async function copyInvite(): Promise<void> {
    const link = buildInviteLink({
      relay: store.session.relay ?? '',
      room: store.session.room ?? '',
    });
    const ok = await copyText(link);
    copied = ok;
    copyFailed = !ok;
    if (!ok) {
      bridge.report('WARN', 'could not copy the invite link to the clipboard');
    }
    clearTimeout(copyTimer);
    copyTimer = setTimeout(() => {
      copied = false;
      copyFailed = false;
    }, COPIED_FEEDBACK_MS);
  }

  const remotes = $derived(store.remotes);

  /**
   * An invite that arrives during a call is offered rather than obeyed:
   * yanking someone out of a conversation they are already in would be worse
   * than making them click once.
   */
  const invite = $derived(store.pendingInvite);

  /**
   * Health of the link to the relay, as one of three states.
   *
   * Green is not merely "joined": a session can be up and still unusable, so
   * sustained packet loss reads as degraded rather than healthy. Red is
   * reserved for having no working session at all — that is the state a
   * participant needs to recognise instantly.
   */
  const LOSS_DEGRADED_PERCENT = 2;

  const health = $derived.by(() => {
    if (!store.connected) {
      return { level: 'down', label: 'Backend disconnected' };
    }
    switch (store.session.phase) {
      case 'joined':
        break;
      case 'reconnecting':
        return { level: 'degraded', label: 'Reconnecting to the relay' };
      case 'failed':
        return { level: 'down', label: store.session.detail || 'Connection failed' };
      default:
        return { level: 'down', label: 'Not connected' };
    }
    const loss = store.metrics?.lossPercent ?? 0;
    if (loss >= LOSS_DEGRADED_PERCENT) {
      return { level: 'degraded', label: `Connected · ${loss.toFixed(1)}% packet loss` };
    }
    const rtt = store.metrics?.rttMs;
    return {
      level: 'ok',
      label: rtt ? `Connected · ${rtt.toFixed(0)} ms round trip` : 'Connected',
    };
  });

  function acceptInvite(): void {
    // Leaving returns to the welcome screen, which picks the invite up from
    // the store and joins — the same path a link-launched app takes.
    store.leave();
  }
  /**
   * Which tile has the window to itself, if any.
   *
   * Held here rather than in the tiles because it is a property of the layout:
   * only one of them can be expanded, and the grid is what has to give way.
   */
  const SELF = '\u0000self';
  let expandedId = $state<string | null>(null);

  /**
   * The expansion, dropped if whoever it belonged to has left. Reading through
   * this rather than the raw id means a departure cannot leave the view stuck on
   * a participant who is no longer there.
   */
  const expanded = $derived(
    expandedId !== null && (expandedId === SELF || remotes.some((r) => r.id === expandedId))
      ? expandedId
      : null,
  );

  // Escape gets out, the way it does from anything that took over the window.
  $effect(() => {
    if (expanded === null) return;
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key === 'Escape') expandedId = null;
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  });

  /** Column count grows with the participant count, capped so tiles stay large. */
  const columns = $derived(
    expanded !== null
      ? 1
      : Math.min(3, Math.max(1, Math.ceil(Math.sqrt(remotes.length + 1)))),
  );
</script>

<div class="conference">
  <header>
    <div class="identity">
      <span class="room">{store.session.room}</span>
      <span class="sep">·</span>
      <span class="relay mono">{store.session.relay}</span>
      <span
        class="health {health.level}"
        title={health.label}
        role="img"
        aria-label={health.label}
      ></span>
    </div>
    <div class="right">
      <span class="peers">
        {remotes.length === 0
          ? 'waiting for others to join'
          : `${remotes.length} other ${remotes.length === 1 ? 'participant' : 'participants'}`}
      </span>
      <DeviceMenu />
      <button
        onclick={copyInvite}
        disabled={!canInvite}
        title={canInvite
          ? `Copy an invite link for room ${store.session.room} on ${store.session.relay}`
          : 'No relay and room to invite to yet'}
      >
        <!-- The tick was a ✓ in the text; it is the icon's job now, so the
             label just says what happened. -->
        {#if copied}
          <Check size={ICON_SIZE} />Copied
        {:else if copyFailed}
          <Link size={ICON_SIZE} />Copy failed
        {:else}
          <Link size={ICON_SIZE} />Copy invite
        {/if}
      </button>
      <button class="danger" onclick={() => store.leave()}>
        <PhoneOff size={ICON_SIZE} />
        Leave
      </button>
    </div>
  </header>

  <div
    class="grid"
    class:solo={expanded !== null}
    style:grid-template-columns={`repeat(${columns}, minmax(0, 1fr))`}
  >
    {#if expanded === null || expanded === SELF}
      <VideoTile
        nickname={store.session.nickname ?? 'me'}
        localStream={store.previewStream}
        localVideoId={store.previewVideoId}
        localSource={store.media.videoSource}
        hasAudio={store.previewAudio}
        speaking={store.speaking}
        expanded={expanded === SELF}
        onExpand={(want) => (expandedId = want ? SELF : null)}
      />
    {/if}
    {#each remotes as remote (remote.id)}
      {#if expanded === null || expanded === remote.id}
        <VideoTile
          nickname={remote.nickname}
          videoHandle={remote.videoHandle}
          hasAudio={remote.audioHandle !== null}
          label={remote.id}
          speaking={remote.speaking}
          expanded={expanded === remote.id}
          onExpand={(want) => (expandedId = want ? remote.id : null)}
        />
      {/if}
    {/each}
  </div>

  {#if invite}
    <div class="invite-banner">
      <span>
        Invite to room <strong>{invite.room}</strong> on
        <span class="mono">{invite.relay}</span>
      </span>
      <span class="invite-actions">
        <button onclick={acceptInvite}>Leave and join</button>
        <button class="ghost" onclick={() => (store.pendingInvite = null)}>Dismiss</button>
      </span>
    </div>
  {/if}

  {#if store.session.phase === 'reconnecting'}
    <p class="banner">
      Lost the relay — reconnecting…{store.session.detail
        ? ` (${store.session.detail})`
        : ''}
    </p>
  {:else if !store.connected}
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

  /* Relay health. Kept beside the address because that is what it describes,
     and given a title so the colour is never the only thing carrying it. */
  .health {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    flex: none;
    align-self: center;
    background: var(--text-faint);
    transition: background 200ms ease, box-shadow 200ms ease;
  }

  .health.ok {
    background: var(--ok);
    box-shadow: 0 0 6px color-mix(in srgb, var(--ok) 55%, transparent);
  }

  .health.degraded {
    background: var(--warn);
    box-shadow: 0 0 6px color-mix(in srgb, var(--warn) 55%, transparent);
  }

  .health.down {
    background: var(--err);
    box-shadow: 0 0 6px color-mix(in srgb, var(--err) 55%, transparent);
  }

  @media (prefers-reduced-motion: reduce) {
    .health {
      transition: none;
    }
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

  /* One tile, given the whole area: the row has to be allowed to stretch, which
     centred content would not do, and nothing should scroll since there is
     nothing below the fold. */
  .grid.solo {
    align-content: stretch;
    overflow: hidden;
  }

  .invite-banner {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    flex-wrap: wrap;
    padding: 8px 14px;
    background: color-mix(in srgb, var(--accent) 14%, transparent);
    border-top: 1px solid color-mix(in srgb, var(--accent) 40%, transparent);
    font-size: 12px;
    flex: none;
  }

  .invite-actions {
    display: inline-flex;
    gap: 6px;
    flex: none;
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
