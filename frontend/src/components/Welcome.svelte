<script lang="ts">
  /**
   * The welcome screen: relay address, room, nickname, and camera and
   * microphone selection with a live preview.
   *
   * Device labels are blank until capture permission has been granted, so
   * this screen opens a stream as soon as it mounts — the preview doubles
   * as the permission prompt and as the check that the devices work
   * before anyone joins a call.
   */
  import { onMount } from 'svelte';
  import { defaultAudioSettings, defaultVideoSettings, listDevices } from '../lib/capture';
  import { randomNickname, randomRoom } from '../lib/nickname';
  import { store } from '../lib/session.svelte';

  const RELAY_KEY = 'tlmst.relay';
  const NICK_KEY = 'tlmst.nickname';

  /**
   * Query parameters prefill the form, so a room can be shared as a link
   * (and the app can be launched straight into one — see the -room flag in
   * main.go). `join=1` submits without waiting for a click.
   */
  const params = new URLSearchParams(location.search);

  let relay = $state(params.get('relay') ?? localStorage.getItem(RELAY_KEY) ?? 'localhost:4433');
  let room = $state(params.get('room') ?? '');
  let nickname = $state(
    params.get('nickname') ?? localStorage.getItem(NICK_KEY) ?? randomNickname(),
  );

  let cameras = $state<MediaDeviceInfo[]>([]);
  let microphones = $state<MediaDeviceInfo[]>([]);
  let cameraId = $state('');
  let microphoneId = $state('');
  let useVideo = $state(true);
  let useAudio = $state(true);

  let resolution = $state('1280x720');
  let videoBitrate = $state(defaultVideoSettings.bitrate);
  let audioBitrate = $state(defaultAudioSettings.bitrate);
  let denoise = $state(defaultAudioSettings.denoise);

  let previewEl = $state<HTMLVideoElement | null>(null);
  let permissionError = $state('');
  let joining = $state(false);
  let joinError = $state('');

  const connecting = $derived(store.session.phase === 'connecting');

  onMount(() => {
    if (!room) room = randomRoom();
    void (async () => {
      await refreshDevices();
      if (params.get('join') === '1') {
        // Wait for the backend socket: join() needs it to send anything.
        await waitForBackend(10000);
        await join();
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

  async function refreshDevices(): Promise<void> {
    try {
      await store.openPreview(videoSettings(), audioSettings());
      permissionError = '';
    } catch (err) {
      permissionError = err instanceof Error ? err.message : String(err);
    }
    // Enumerate after opening: labels stay empty until permission lands.
    const devices = await listDevices();
    cameras = devices.cameras;
    microphones = devices.microphones;
    if (!cameraId && cameras.length) cameraId = cameras[0].deviceId;
    if (!microphoneId && microphones.length) microphoneId = microphones[0].deviceId;
  }

  function videoSettings() {
    if (!useVideo) return null;
    const [width, height] = resolution.split('x').map(Number);
    return {
      deviceId: cameraId || undefined,
      width,
      height,
      framerate: defaultVideoSettings.framerate,
      bitrate: videoBitrate,
    };
  }

  function audioSettings() {
    if (!useAudio) return null;
    return { deviceId: microphoneId || undefined, bitrate: audioBitrate, denoise };
  }

  /** Reopens the devices when a selection changes, so preview follows it. */
  async function reopen(): Promise<void> {
    try {
      await store.openPreview(videoSettings(), audioSettings());
      permissionError = '';
    } catch (err) {
      permissionError = err instanceof Error ? err.message : String(err);
    }
  }

  async function join(): Promise<void> {
    if (!relay.trim() || !room.trim()) return;
    joining = true;
    joinError = '';
    localStorage.setItem(RELAY_KEY, relay);
    localStorage.setItem(NICK_KEY, nickname);
    try {
      await store.join(relay, room, nickname, videoSettings(), audioSettings());
    } catch (err) {
      joinError = err instanceof Error ? err.message : String(err);
    } finally {
      joining = false;
    }
  }
</script>

<div class="welcome">
  <div class="card">
    <header>
      <h1>tlmst</h1>
      <p>Teleconferencing over Media over QUIC</p>
    </header>

    <div class="preview">
      {#if store.previewStream && useVideo}
        <!-- svelte-ignore a11y_media_has_caption -->
        <video bind:this={previewEl} autoplay muted playsinline></video>
      {:else}
        <div class="preview-empty">
          {#if permissionError}
            <span class="err">{permissionError}</span>
          {:else if !useVideo}
            <span>Camera off</span>
          {:else}
            <span>Starting camera…</span>
          {/if}
        </div>
      {/if}
    </div>

    <div class="fields">
      <div class="field">
        <label for="relay">Relay address</label>
        <input id="relay" bind:value={relay} placeholder="localhost:4433" spellcheck="false" />
        <p class="hint">host:port, moqt://…, or https://… for WebTransport</p>
      </div>

      <div class="row">
        <div class="field">
          <label for="room">Room</label>
          <div class="with-button">
            <input id="room" bind:value={room} spellcheck="false" />
            <button class="ghost" onclick={() => (room = randomRoom())} title="New room">↻</button>
          </div>
        </div>
        <div class="field">
          <label for="nick">Nickname</label>
          <div class="with-button">
            <input id="nick" bind:value={nickname} spellcheck="false" />
            <button class="ghost" onclick={() => (nickname = randomNickname())} title="New nickname">↻</button>
          </div>
        </div>
      </div>

      <div class="row">
        <div class="field">
          <label for="cam">
            <input
              type="checkbox"
              class="inline"
              bind:checked={useVideo}
              onchange={reopen}
            /> Camera
          </label>
          <select id="cam" bind:value={cameraId} onchange={reopen} disabled={!useVideo}>
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
              bind:checked={useAudio}
              onchange={reopen}
            /> Microphone
          </label>
          <select id="mic" bind:value={microphoneId} onchange={reopen} disabled={!useAudio}>
            {#each microphones as mic (mic.deviceId)}
              <option value={mic.deviceId}>{mic.label || 'Microphone'}</option>
            {/each}
          </select>
        </div>
      </div>

      <div class="row">
        <div class="field">
          <label for="res">Resolution</label>
          <select id="res" bind:value={resolution} onchange={reopen} disabled={!useVideo}>
            <option value="640x360">640 × 360</option>
            <option value="854x480">854 × 480</option>
            <option value="1280x720">1280 × 720</option>
            <option value="1920x1080">1920 × 1080</option>
          </select>
        </div>
        <div class="field">
          <label for="vbr">Video bitrate</label>
          <select id="vbr" bind:value={videoBitrate} disabled={!useVideo}>
            <option value={500_000}>500 kbps</option>
            <option value={1_000_000}>1 Mbps</option>
            <option value={1_500_000}>1.5 Mbps</option>
            <option value={3_000_000}>3 Mbps</option>
          </select>
        </div>
        <div class="field">
          <label for="abr">Audio bitrate</label>
          <select id="abr" bind:value={audioBitrate} disabled={!useAudio}>
            <option value={16_000}>16 kbps</option>
            <option value={32_000}>32 kbps</option>
            <option value={64_000}>64 kbps</option>
          </select>
        </div>
      </div>

      <label class="toggle">
        <input type="checkbox" bind:checked={denoise} disabled={!useAudio} />
        <span>
          Noise suppression
          <em>
            RNNoise, on top of the platform's own echo cancellation and
            suppression
          </em>
        </span>
      </label>
    </div>

    {#if joinError}
      <p class="error-box">{joinError}</p>
    {/if}

    <footer>
      <span class="status">
        <span class="dot" class:on={store.connected}></span>
        {store.connected ? 'backend connected' : 'waiting for backend…'}
      </span>
      <button
        class="primary"
        onclick={join}
        disabled={joining || connecting || !store.connected || (!useVideo && !useAudio)}
      >
        {joining || connecting ? 'Connecting…' : 'Join room'}
      </button>
    </footer>
  </div>
</div>

<style>
  .welcome {
    flex: 1;
    display: grid;
    place-items: center;
    padding: 24px;
    overflow-y: auto;
  }

  .card {
    width: 100%;
    max-width: 640px;
    background: var(--bg-raised);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 18px 20px;
  }

  header {
    margin-bottom: 12px;
  }

  h1 {
    margin: 0;
    font-size: 22px;
    letter-spacing: -0.01em;
  }

  header p {
    margin: 2px 0 0;
    color: var(--text-dim);
    font-size: 13px;
  }

  .preview {
    aspect-ratio: 16 / 9;
    /* Cap the height so the card as a whole clears the default window at
       any width: the preview is the one element that would otherwise grow
       with it and push the form into a scroll. */
    max-height: 232px;
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    overflow: hidden;
    margin-bottom: 12px;
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

  .fields {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .row {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
    gap: 12px;
  }

  .field {
    min-width: 0;
  }

  .with-button {
    display: flex;
    gap: 4px;
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
    margin: 14px 0 0;
    padding: 8px 10px;
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--err) 12%, transparent);
    border: 1px solid color-mix(in srgb, var(--err) 40%, transparent);
    color: var(--err);
    font-size: 12px;
  }

  footer {
    margin-top: 14px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .status {
    display: inline-flex;
    align-items: center;
    gap: 7px;
    font-size: 12px;
    color: var(--text-dim);
  }

  .dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--text-faint);
  }

  .dot.on {
    background: var(--ok);
  }
</style>
