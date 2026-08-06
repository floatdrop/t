<script lang="ts">
  /**
   * The welcome screen: a mark, a room, a name, and a way in.
   *
   * Nothing here touches the camera or microphone on its own. Opening the app
   * used to prompt for both immediately, in order to fill in device labels and
   * show a preview — which meant the very first thing a new user saw was two
   * OS permission dialogs, before they had expressed any interest in a call.
   * The preview now lives behind the settings disclosure, so the prompt arrives
   * either when someone goes looking for their devices or when they join, both
   * of which are moments they asked for it.
   */
  import Settings2 from '@lucide/svelte/icons/settings-2';
  import { onMount } from 'svelte';
  import { ICON_SIZE } from '../lib/icons';
  import { parseInviteLink } from '../lib/invite';
  import { randomNickname, randomRoom } from '../lib/nickname';
  import { store } from '../lib/session.svelte';
  import Logo from './Logo.svelte';

  const RELAY_KEY = 'tlmst.relay';
  const NICK_KEY = 'tlmst.nickname';

  /** Where calls go unless told otherwise. */
  const DEFAULT_RELAY = 'https://t.tel.yandex.net/';

  /**
   * Query parameters prefill the form, so a room can be shared as a link
   * (and the app can be launched straight into one — see the -room flag in
   * main.go). `join=1` submits without waiting for a click.
   */
  const params = new URLSearchParams(location.search);

  let relay = $state(params.get('relay') ?? localStorage.getItem(RELAY_KEY) ?? DEFAULT_RELAY);
  let room = $state(params.get('room') ?? '');
  /**
   * Whoever the system says is using the machine, unless something more
   * deliberate has said otherwise: an explicit -nickname wins, then a name kept
   * from a previous run, then the OS account (the `user` param, set by
   * systemUserName in main.go), and only then something made up.
   */
  let nickname = $state(
    params.get('nickname') ??
      localStorage.getItem(NICK_KEY) ??
      params.get('user') ??
      randomNickname(),
  );

  // Device and encoder choices live in the store, shared with the in-call
  // device menu — see MediaSettings in session.svelte.ts.
  const cameras = $derived(store.devices.cameras);
  const microphones = $derived(store.devices.microphones);

  let settingsOpen = $state(false);
  let previewEl = $state<HTMLVideoElement | null>(null);
  let permissionError = $state('');
  let joining = $state(false);
  let joinError = $state('');

  const connecting = $derived(store.session.phase === 'connecting');
  const noDevices = $derived(!store.media.useVideo && !store.media.useAudio);

  onMount(() => {
    if (!room) room = randomRoom();
    if (params.get('join') !== '1') return;
    void (async () => {
      // Consumed, the way a delivered invite is: it says what to do on
      // launch, not every time this screen appears. This screen is also where
      // Leave returns to, and an unconsumed flag walked straight back into the
      // room — leaving no way out of a call started with it.
      params.delete('join');
      const rest = params.toString();
      history.replaceState(null, '', rest ? `${location.pathname}?${rest}` : location.pathname);
      // Wait for the backend socket: join() needs it to send anything.
      await waitForBackend(10000);
      await join();
    })();
  });

  /**
   * Acts on an invite link the OS delivered: fill the form, then join.
   *
   * Joining without a further click is the point of a clickable link — the
   * user already expressed intent by opening it. An invite that lands while a
   * call is in progress is handled by Conference, not here.
   */
  $effect(() => {
    const invite = store.pendingInvite;
    if (!invite) return;
    store.pendingInvite = null;
    relay = invite.relay;
    room = invite.room;
    invitePasted = true;
    void (async () => {
      try {
        await waitForBackend(10000);
        await join();
      } catch (err) {
        joinError = err instanceof Error ? err.message : String(err);
      }
    })();
  });

  /** Resolves once the bridge is connected, or rejects on timeout. */
  function waitForBackend(timeoutMs: number): Promise<void> {
    return new Promise((resolve, reject) => {
      const deadline = Date.now() + timeoutMs;
      const tick = () => {
        if (store.connected) return resolve();
        if (Date.now() > deadline) return reject(new Error('backend did not connect'));
        setTimeout(tick, 100);
      };
      tick();
    });
  }

  // Keep the preview element bound to whatever stream capture currently
  // holds, including after a device change reopens it.
  $effect(() => {
    if (previewEl && store.previewStream) {
      previewEl.srcObject = store.previewStream;
    }
  });

  /** Opens the devices, which is also what populates their labels. */
  async function refreshDevices(): Promise<void> {
    try {
      await store.openPreview();
      permissionError = '';
    } catch (err) {
      permissionError = err instanceof Error ? err.message : String(err);
    }
  }

  /** Reopens the devices when a selection changes, so preview follows it. */
  const reopen = refreshDevices;

  /**
   * Shows or hides the settings, opening the devices on the way in and
   * releasing them on the way out.
   *
   * Expanding this is the request for device access — it is where the labels
   * and the preview are, and neither exists without it.
   */
  async function toggleSettings(): Promise<void> {
    settingsOpen = !settingsOpen;
    if (settingsOpen) await refreshDevices();
    else store.closePreview();
  }

  /**
   * Fills relay and room from a pasted invite link.
   *
   * Bound to both fields because either is a plausible place to paste a link
   * that carries both values, and neither would accept it as a literal value.
   *
   * The settings are deliberately left closed, even though the relay inside
   * them just changed: expanding them is what opens the camera, and a paste is
   * no reason to prompt anyone for it. The hint under the room says what
   * happened instead.
   */
  function onPaste(event: ClipboardEvent): void {
    const text = event.clipboardData?.getData('text') ?? '';
    const invite = parseInviteLink(text);
    if (!invite) return;
    event.preventDefault();
    relay = invite.relay;
    room = invite.room;
    invitePasted = true;
    setTimeout(() => (invitePasted = false), 2400);
  }

  let invitePasted = $state(false);

  async function join(): Promise<void> {
    if (!relay.trim() || !room.trim()) return;
    joining = true;
    joinError = '';
    localStorage.setItem(RELAY_KEY, relay);
    localStorage.setItem(NICK_KEY, nickname);
    try {
      await store.join(relay, room, nickname);
    } catch (err) {
      joinError = err instanceof Error ? err.message : String(err);
    } finally {
      joining = false;
    }
  }

  /**
   * What the button says, which is also the only place the reasons it cannot be
   * pressed are visible. The backend indicator moved to the debug drawer, so
   * without this a missing backend would leave a dead button and no
   * explanation.
   */
  const action = $derived.by(() => {
    if (joining || connecting) return 'Connecting…';
    if (!store.connected) return 'Waiting for the backend…';
    if (noDevices) return 'Turn on a camera or microphone';
    return 'Join room';
  });
</script>

<div class="welcome">
  <form
    class="panel"
    class:configuring={settingsOpen}
    onsubmit={(ev) => {
      ev.preventDefault();
      void join();
    }}
  >
    <div class="hero">
      <Logo size={72} />
      <h1>tlmst</h1>
      <p>Teleconferencing over Media over QUIC</p>
    </div>

    <div class="field">
      <label for="room">Room</label>
      <div class="with-button">
        <input id="room" bind:value={room} onpaste={onPaste} spellcheck="false" />
        <button
          type="button"
          class="ghost"
          onclick={() => (room = randomRoom())}
          title="New room"
        >↻</button>
      </div>
      {#if invitePasted}
        <p class="hint ok-hint">Invite link applied — relay and room filled in.</p>
      {/if}
    </div>

    <div class="field">
      <label for="nick">Your name</label>
      <div class="with-button">
        <input id="nick" bind:value={nickname} spellcheck="false" />
        <button
          type="button"
          class="ghost"
          onclick={() => (nickname = randomNickname())}
          title="New nickname"
        >↻</button>
      </div>
    </div>

    {#if joinError}
      <p class="error-box">{joinError}</p>
    {/if}

    <button
      type="submit"
      class="primary join"
      disabled={joining || connecting || !store.connected || noDevices}
    >
      {action}
    </button>

    <button
      type="button"
      class="ghost disclosure"
      onclick={toggleSettings}
      aria-expanded={settingsOpen}
      aria-controls="welcome-settings"
    >
      <Settings2 size={ICON_SIZE} />
      Settings
      <span class="caret" aria-hidden="true">{settingsOpen ? '▴' : '▾'}</span>
    </button>

    {#if settingsOpen}
      <div class="settings" id="welcome-settings">
        <div class="preview">
          {#if store.previewVideo}
            <!-- Keyed on the camera for the reason VideoTile is: the stream is
                 reused across a device switch, so only a fresh element is certain
                 to show the new one. -->
            {#key store.previewVideoId}
              <!-- svelte-ignore a11y_media_has_caption -->
              <video bind:this={previewEl} autoplay muted playsinline></video>
            {/key}
          {:else}
            <div class="preview-empty">
              {#if permissionError}
                <span class="err">{permissionError}</span>
              {:else if !store.media.useVideo}
                <span>Camera off</span>
              {:else}
                <span>Starting camera…</span>
              {/if}
            </div>
          {/if}
        </div>

        <div class="field">
          <label for="relay">Relay address</label>
          <input
            id="relay"
            bind:value={relay}
            onpaste={onPaste}
            placeholder={DEFAULT_RELAY}
            spellcheck="false"
          />
          <p class="hint">
            host:port, moqt://…, or https://… for WebTransport — or paste an invite link
          </p>
        </div>

        <div class="row">
          <div class="field">
            <label for="cam">
              <input
                type="checkbox"
                class="inline"
                bind:checked={store.media.useVideo}
                onchange={reopen}
              /> Camera
            </label>
            <select
              id="cam"
              bind:value={store.media.cameraId}
              onchange={reopen}
              disabled={!store.media.useVideo}
            >
              {#each cameras as cam (cam.deviceId)}
                <option value={cam.deviceId}>{cam.label || 'Camera'}</option>
              {/each}
            </select>
          </div>
          <div class="field">
            <label for="mic">
              <input
                type="checkbox"
                class="inline"
                bind:checked={store.media.useAudio}
                onchange={reopen}
              /> Microphone
            </label>
            <select
              id="mic"
              bind:value={store.media.microphoneId}
              onchange={reopen}
              disabled={!store.media.useAudio}
            >
              {#each microphones as mic (mic.deviceId)}
                <option value={mic.deviceId}>{mic.label || 'Microphone'}</option>
              {/each}
            </select>
          </div>
        </div>

        <div class="row">
          <div class="field">
            <label for="res">Resolution</label>
            <select
              id="res"
              bind:value={store.media.resolution}
              onchange={reopen}
              disabled={!store.media.useVideo}
            >
              <option value="640x360">640 × 360</option>
              <option value="854x480">854 × 480</option>
              <option value="1280x720">1280 × 720</option>
              <option value="1920x1080">1920 × 1080</option>
            </select>
          </div>
          <div class="field">
            <label for="vbr">Video bitrate</label>
            <select id="vbr" bind:value={store.media.videoBitrate} disabled={!store.media.useVideo}>
              <option value={500_000}>500 kbps</option>
              <option value={1_000_000}>1 Mbps</option>
              <option value={1_500_000}>1.5 Mbps</option>
              <option value={3_000_000}>3 Mbps</option>
            </select>
          </div>
          <div class="field">
            <label for="abr">Audio bitrate</label>
            <select id="abr" bind:value={store.media.audioBitrate} disabled={!store.media.useAudio}>
              <option value={16_000}>16 kbps</option>
              <option value={32_000}>32 kbps</option>
              <option value={64_000}>64 kbps</option>
            </select>
          </div>
        </div>

        <label class="toggle">
          <input
            type="checkbox"
            bind:checked={store.media.denoise}
            disabled={!store.media.useAudio}
          />
          <span>
            Noise suppression
            <em>
              RNNoise, on top of the platform's own echo cancellation and
              suppression
            </em>
          </span>
        </label>
      </div>
    {/if}
  </form>
</div>

<style>
  .welcome {
    flex: 1;
    display: grid;
    place-items: center;
    padding: 18px;
    overflow-y: auto;
  }

  /* No card around any of this. The screen has one thing to say, and a border
     drawn around it only competes with the mark for attention. */
  .panel {
    width: 100%;
    max-width: 360px;
    display: flex;
    flex-direction: column;
    gap: 12px;
    /* Narrow while closed, because the screen has one thing to say. Wider once
       the settings are out, or eight fields stacked in a 360px column run off
       the bottom of the window. */
    transition: max-width 160ms ease;
  }

  .panel.configuring {
    max-width: 460px;
  }

  .hero {
    display: flex;
    flex-direction: column;
    align-items: center;
    text-align: center;
    gap: 8px;
    margin-bottom: 4px;
  }

  h1 {
    margin: 0;
    font-size: 22px;
    letter-spacing: -0.01em;
  }

  .hero p {
    margin: -6px 0 0;
    color: var(--text-dim);
    font-size: 13px;
  }

  .field {
    min-width: 0;
  }

  .row {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(130px, 1fr));
    gap: 12px;
  }

  .with-button {
    display: flex;
    gap: 4px;
  }

  .join {
    margin-top: 2px;
  }

  /* Quiet by design: it is the way to the things most people never need to
     change, and should not compete with the button next to it. */
  .disclosure {
    align-self: center;
    font-size: 12px;
    color: var(--text-dim);
  }

  .caret {
    font-size: 10px;
    line-height: 1;
  }

  .settings {
    display: flex;
    flex-direction: column;
    gap: 9px;
    padding-top: 12px;
    border-top: 1px solid var(--border);
  }

  .preview {
    aspect-ratio: 16 / 9;
    /* Capped so the expanded panel still clears the default window — this is a
       check that the camera works, not a stage to perform on. Capping the
       height of a 16/9 box narrows it, so it is centred too: left-aligned and
       narrower than every field below it just reads as broken. */
    max-height: 140px;
    align-self: center;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .preview video {
    width: 100%;
    height: 100%;
    object-fit: cover;
    /* Mirror the local preview: people expect to see themselves as in a
       mirror, not as others see them. */
    transform: scaleX(-1);
  }

  .preview-empty {
    height: 100%;
    display: grid;
    place-items: center;
    color: var(--text-faint);
    font-size: 13px;
    text-align: center;
    padding: 12px;
  }

  .hint {
    margin: 4px 0 0;
    font-size: 11px;
    color: var(--text-faint);
  }

  input.inline {
    width: auto;
    margin-right: 4px;
    vertical-align: -1px;
  }

  .err {
    color: var(--err);
  }

  .ok-hint {
    color: var(--ok);
  }

  .toggle {
    display: flex;
    align-items: flex-start;
    gap: 7px;
    margin: 0;
    font-size: 12px;
    color: var(--text);
  }

  .toggle input {
    width: auto;
    margin-top: 2px;
    flex: none;
  }

  .toggle em {
    display: block;
    font-style: normal;
    font-size: 11px;
    color: var(--text-faint);
  }

  .error-box {
    margin: 0;
    padding: 8px 10px;
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--err) 12%, transparent);
    border: 1px solid color-mix(in srgb, var(--err) 40%, transparent);
    color: var(--err);
    font-size: 12px;
  }
</style>
