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
  import { GRID_GAP, GRID_PADDING, tileColumns } from '../lib/layout';
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

  /**
   * Round trip above which the link reads as degraded, whatever else is going
   * well.
   *
   * Loss was the only thing this dot watched, and a session can be lossless and
   * still be a poor call: latency is what makes people talk over each other,
   * and nothing about the picture or the counters gives it away. G.114 puts the
   * limit of comfortable conversation at 150 ms one way, which is 300 ms of
   * round trip; this sits a little under that so the dot turns before the call
   * becomes the thing being discussed rather than after. A cellular uplink
   * clears it easily.
   */
  const RTT_DEGRADED_MS = 250;

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
    const rtt = store.metrics?.rttMs ?? 0;
    if (rtt >= RTT_DEGRADED_MS) {
      return { level: 'degraded', label: `Connected · ${rtt.toFixed(0)} ms round trip` };
    }
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

  /**
   * Column count grows with the participant count, capped so tiles stay large.
   *
   * The arithmetic lives in layout.ts because Auto resolution needs the same
   * answer: how wide a tile ends up is what decides how big a picture is worth
   * sending. Two copies of it would drift, and the drift would be invisible —
   * a slightly wrong idea of the tile size only ever shows up as a stream
   * that is quietly the wrong size.
   */
  const columns = $derived(expanded !== null ? 1 : tileColumns(remotes.length + 1));

  /**
   * How far outside the grid a tile still counts as worth receiving.
   *
   * A whole viewport in each direction, which is more than it sounds and less
   * than it needs to be. Subscribing is not instantaneous: a fresh SUBSCRIBE
   * starts at the live edge and the first thing a decoder can use is the next
   * keyframe, so a tile subscribed the moment it appears shows nothing for up
   * to the publisher's keyframe interval — two seconds. There is no way to ask
   * a remote publisher for one sooner; nobody has a channel to ask on. So the
   * margin has to be wide enough that the wait happens off screen, and a
   * viewport of scrolling is about the least that reliably is.
   */
  const INTEREST_MARGIN = '100%';

  /**
   * How long to let the view settle before telling the backend. A scroll fires
   * the observer many times on the way past, and the answer at the end is the
   * only one worth acting on — the same reasoning as the resize settle.
   */
  const INTEREST_SETTLE_MS = 250;

  /**
   * How often visibility is worked out from the layout instead of waited for.
   *
   * Everything below rides on IntersectionObserver callbacks, and a callback
   * chain that stops is this WebView's signature failure — the presentation
   * loop, the capture tap and the worklet ports have all done it, which is why
   * each of them has a watchdog. This one has the worst possible resting
   * state: if the callbacks stop while nothing is on screen, every peer's
   * video stays unsubscribed, no counter moves, no line is logged, and the
   * empty set is faithfully replayed across reconnects. A tile plainly on
   * screen showing nothing, for the rest of the call.
   *
   * So the answer is recomputed from the geometry on a slow timer, which needs
   * no callback to arrive. Slow because it is a floor, not the mechanism.
   */
  const INTEREST_SWEEP_MS = 10_000;

  let gridEl = $state<HTMLDivElement | undefined>();

  /**
   * Which tiles exist, as a value that only changes when that does.
   *
   * The observer below must not be torn down for anything else, and `remotes`
   * is rebuilt whenever *anyone speaks* — it carries a speaking flag, so it
   * changes identity several times a second on a live call. An effect watching
   * it re-ran that often, and each re-run cancelled the pending report before
   * its settle elapsed, so after the first one no interest was ever sent
   * again: every tile in the room stayed unsubscribed while the observer
   * looked like it was working.
   */
  const tileKey = $derived(
    remotes
      .map((r) => r.id)
      .sort()
      .join(','),
  );

  /**
   * Reports which tiles are on screen, so the backend can stop subscribing to
   * the video of people nobody can see.
   *
   * The grid scrolls — that is the whole reason this exists. A nine-person
   * call draws nine tiles and shows perhaps six, and the other three are a
   * megabit each of pictures being decoded into a scroll region nobody is
   * looking at. Discarding them here would still have paid for them on the
   * wire; not asking is the only saving available.
   *
   * Re-observes whenever the set of tiles changes, which is why remotes and
   * expanded are read: solo view renders exactly one tile, and everyone else
   * stops being visible in the most literal sense.
   */
  $effect(() => {
    void tileKey;
    void expanded;
    const root = gridEl;
    if (!root) return;

    const visible = new Set<string>();
    let settle: ReturnType<typeof setTimeout> | undefined;

    /**
     * Whose video to ask for: what is on screen, except in solo view.
     *
     * Expanding renders exactly one tile, so on visibility alone every other
     * participant would be dropped — and collapsing would rebuild all of them
     * at once, each a SUBSCRIBE, a new handle, a new decoder and a backfilled
     * group that its live stream now waits behind. In a nine-way call that is
     * eight of those arriving together, for a view change that Escape undoes.
     * The others are one keypress away; they stay subscribed.
     */
    const wantedTiles = (): string[] =>
      expanded !== null ? remotes.map((r) => r.id) : [...visible];

    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          const id = (entry.target as HTMLElement).dataset.participant;
          if (!id) continue;
          if (entry.isIntersecting) visible.add(id);
          else visible.delete(id);
        }
        clearTimeout(settle);
        // Reported, not decided: which tiles are on screen is all this can
        // see. How big they are drawn and whether the link can carry the full
        // picture are both the store's, which is what lets either change the
        // answer without anybody scrolling.
        settle = setTimeout(() => store.setVisibleTiles(wantedTiles()), INTEREST_SETTLE_MS);
      },
      { root, rootMargin: INTEREST_MARGIN, threshold: 0 },
    );

    for (const tile of root.querySelectorAll('[data-participant]')) {
      observer.observe(tile);
    }

    // The floor. Measures the same band the observer watches — the grid's own
    // box grown by INTEREST_MARGIN — so a sweep agrees with the callbacks
    // rather than fighting them.
    const sweep = setInterval(() => {
      const box = root.getBoundingClientRect();
      const margin = box.height;
      const seen: string[] = [];
      for (const tile of root.querySelectorAll<HTMLElement>('[data-participant]')) {
        const id = tile.dataset.participant;
        if (!id) continue;
        const r = tile.getBoundingClientRect();
        if (r.bottom >= box.top - margin && r.top <= box.bottom + margin) seen.push(id);
      }
      visible.clear();
      for (const id of seen) visible.add(id);
      store.setVisibleTiles(wantedTiles());
    }, INTEREST_SWEEP_MS);

    return () => {
      clearTimeout(settle);
      clearInterval(sweep);
      observer.disconnect();
    };
  });
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

  <!-- Padding and gap come from the same constants, since the width they leave
       for a tile is exactly what Auto resolution measures against. -->
  <div
    bind:this={gridEl}
    class="grid"
    class:solo={expanded !== null}
    style:grid-template-columns={`repeat(${columns}, minmax(0, 1fr))`}
    style:gap={`${GRID_GAP}px`}
    style:padding={`${GRID_PADDING}px`}
  >
    {#if expanded === null || expanded === SELF}
      <VideoTile
        nickname={store.session.nickname ?? 'me'}
        version={store.version}
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
          version={remote.version}
          videoHandle={remote.videoHandle}
          videoLevel={remote.videoLevel}
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

  {#if store.errors.length > 0}
    <div class="fault-banner" role="alert">
      <span>
        {store.errors[store.errors.length - 1]}
        {#if store.errors.length > 1}
          <em>and {store.errors.length - 1} more</em>
        {/if}
      </span>
      <button class="ghost" onclick={() => store.dismissFaults()}>Dismiss</button>
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

  /* Gap and padding are set inline from layout.ts — see the markup. */
  .grid {
    flex: 1;
    display: grid;
    overflow-y: auto;
    /* Centre the tiles when they fit, but fall back to top-aligned so an
       overflowing grid still scrolls from its first row. */
    align-content: safe center;
    min-height: 0;
    /* Each row is as tall as the tile it holds, and nothing else.
       Left to `auto`, WebKit sizes these rows by sharing out the grid's own
       height instead — and a tile's height comes from its aspect-ratio, which
       that arithmetic does not consult. With three participants and the debug
       drawer open, the rows came out 93px against tiles 594px tall, so the
       second row was drawn straight over the first: the third tile covered
       half of the first. Measured in this WebView; min-content is what makes
       the row consult the tile. */
    grid-auto-rows: min-content;
  }

  /* One tile, given the whole area: the row has to be allowed to stretch, which
     centred content would not do, and nothing should scroll since there is
     nothing below the fold. */
  .grid.solo {
    align-content: stretch;
    overflow: hidden;
    /* The one case where the row must not follow the tile: an expanded tile
       drops its aspect-ratio and takes the height it is given, so the row has
       to be the one stretching to the window. */
    grid-auto-rows: auto;
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

  /* Red rather than the reconnecting banner's amber: reconnecting is the app
     handling something, while these are the things it cannot handle and has
     stopped trying. Dismissible for the same reason — nothing here clears on
     its own, so it would otherwise sit there for the rest of the call. */
  .fault-banner {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 6px 14px;
    background: color-mix(in srgb, var(--err) 16%, transparent);
    border-top: 1px solid color-mix(in srgb, var(--err) 45%, transparent);
    color: var(--err);
    font-size: 12px;
    flex: none;
  }

  .fault-banner em {
    color: color-mix(in srgb, var(--err) 70%, var(--text-dim));
    font-style: normal;
  }
</style>
