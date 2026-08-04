<script lang="ts">
  /**
   * Per-track accounting: what the backend publishes and subscribes to on
   * the wire, next to what the WebView's own encoders and decoders are
   * doing. Reading them side by side is what localises a problem — a track
   * carrying bytes while its decoder sits at 0 fps means the trouble is in
   * the WebView, not the network.
   */
  import { store } from '../../lib/session.svelte';

  const tracks = $derived(store.metrics?.tracks ?? []);
  const outbound = $derived(tracks.filter((t) => t.label.startsWith('out/')));
  const inbound = $derived(tracks.filter((t) => !t.label.startsWith('out/')));
  const cap = $derived(store.captureStats);
</script>

<div class="panel">
  <section>
    <h3>Session</h3>
    <dl>
      <div><dt>Phase</dt><dd>{store.session.phase}</dd></div>
      <div><dt>Relay</dt><dd>{store.session.relay || '—'}</dd></div>
      <div><dt>Room</dt><dd>{store.session.room || '—'}</dd></div>
      <div><dt>Participant</dt><dd>{store.session.id || '—'}</dd></div>
      <div><dt>Nickname</dt><dd>{store.session.nickname || '—'}</dd></div>
      <div><dt>Remote peers</dt><dd>{store.participants.length}</dd></div>
    </dl>
    {#if store.session.detail}
      <p class="detail">{store.session.detail}</p>
    {/if}
  </section>

  <section>
    <h3>Local capture</h3>
    <dl>
      <div><dt>Video encode</dt><dd>{cap.videoFps.toFixed(1)} fps</dd></div>
      <div><dt>Video bitrate</dt><dd>{Math.round(cap.videoKbps)} kbps</dd></div>
      <div><dt>Encoder queue</dt><dd>{cap.encodeQueue}</dd></div>
      <div><dt>Audio encode</dt><dd>{cap.audioFps.toFixed(1)} fps</dd></div>
      <div><dt>Audio bitrate</dt><dd>{Math.round(cap.audioKbps)} kbps</dd></div>
      <div><dt>Keyframes</dt><dd>{cap.keyFrames}</dd></div>
      <div><dt>Frames dropped</dt><dd>{cap.dropped}</dd></div>
    </dl>
  </section>

  <section>
    <h3>Published tracks</h3>
    {#if outbound.length}
      <table>
        <thead>
          <tr>
            <th scope="col">Track</th><th scope="col">kbps</th>
            <th scope="col">Objects</th><th scope="col">Groups</th>
          </tr>
        </thead>
        <tbody>
          {#each outbound as t (t.label)}
            <tr>
              <th scope="row">{t.label.slice(4)}</th>
              <td>{Math.round(t.kbps)}</td>
              <td>{t.objects}</td>
              <td>{t.groups}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {:else}
      <p class="empty">Nothing published yet.</p>
    {/if}
  </section>

  <section>
    <h3>Subscribed tracks</h3>
    {#if inbound.length}
      <table>
        <thead>
          <tr>
            <th scope="col">Track</th><th scope="col">kbps</th>
            <th scope="col">Objects</th><th scope="col">Groups</th>
          </tr>
        </thead>
        <tbody>
          {#each inbound as t (t.label)}
            <tr>
              <th scope="row">{t.label.slice(3)}</th>
              <td>{Math.round(t.kbps)}</td>
              <td>{t.objects}</td>
              <td>{t.groups}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {:else}
      <p class="empty">No subscriptions.</p>
    {/if}
  </section>

  <section>
    <h3>Decoders</h3>
    {#if store.playbackStats.length}
      <table>
        <thead>
          <tr>
            <th scope="col">Participant</th><th scope="col">Kind</th>
            <th scope="col">fps</th><th scope="col">Queue</th>
            <th scope="col">Dropped</th><th scope="col">Buffered</th>
          </tr>
        </thead>
        <tbody>
          {#each store.playbackStats as s (s.handle)}
            <tr>
              <th scope="row">{s.participant}</th>
              <td>{s.kind}</td>
              <td>{s.fps.toFixed(1)}</td>
              <td>{s.decodeQueue}</td>
              <td>{s.dropped}</td>
              <td>
                {#if s.buffered !== undefined}
                  {(s.buffered / 48).toFixed(0)} ms
                  {#if s.underruns}<span class="warn"> · {s.underruns} underruns</span>{/if}
                {:else}
                  —
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {:else}
      <p class="empty">No active decoders.</p>
    {/if}
  </section>

  {#if store.errors.length}
    <section>
      <h3>Errors</h3>
      <ul class="errors">
        {#each store.errors as err, i (i)}
          <li>{err}</li>
        {/each}
      </ul>
    </section>
  {/if}
</div>

<style>
  .panel {
    padding: 12px;
    overflow-y: auto;
    height: 100%;
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(300px, 1fr));
    gap: 12px;
    align-content: start;
  }

  section {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 10px 12px;
    min-width: 0;
  }

  h3 {
    margin: 0 0 8px;
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    color: var(--text-faint);
    font-weight: 600;
  }

  dl {
    margin: 0;
    display: grid;
    gap: 3px;
  }

  dl > div {
    display: flex;
    justify-content: space-between;
    gap: 10px;
    font-size: 12px;
  }

  dt {
    color: var(--text-dim);
  }

  dd {
    margin: 0;
    font-family: var(--mono);
    font-variant-numeric: tabular-nums;
    text-align: right;
    word-break: break-all;
  }

  table {
    width: 100%;
    border-collapse: collapse;
    font-size: 12px;
  }

  th,
  td {
    text-align: right;
    padding: 3px 5px;
    border-bottom: 1px solid var(--border);
  }

  thead th {
    color: var(--text-faint);
    font-weight: 500;
    font-size: 10px;
    text-transform: uppercase;
  }

  tbody th {
    text-align: left;
    font-weight: 400;
    color: var(--text-dim);
    word-break: break-all;
  }

  td {
    font-family: var(--mono);
    font-variant-numeric: tabular-nums;
  }

  .empty,
  .detail {
    margin: 0;
    font-size: 12px;
    color: var(--text-faint);
  }

  .detail {
    margin-top: 6px;
    color: var(--err);
  }

  .warn {
    color: var(--warn);
  }

  .errors {
    margin: 0;
    padding-left: 16px;
    font-size: 12px;
    color: var(--err);
  }
</style>
