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
      <div><dt>Version</dt><dd>{store.version || '—'}</dd></div>
      <div><dt>Remote peers</dt><dd>{store.participants.length}</dd></div>
    </dl>
    {#if store.session.detail}
      <p class="detail">{store.session.detail}</p>
    {/if}
  </section>

  <!-- Everyone's build, side by side, us included. This is the view that
       answers "is it just me?" — a room where one person is three releases
       behind explains a great deal that is otherwise mystifying, and it
       cannot be seen one tooltip at a time. Our own row is what the others
       are being compared against, so it is first and always present. -->
  <section>
    <h3>Participants</h3>
    <table>
      <thead>
        <tr>
          <th scope="col">Participant</th>
          <th scope="col">ID</th>
          <th scope="col" class="num">Version</th>
        </tr>
      </thead>
      <tbody>
        <!-- Our own row keys on a name no participant can have: ids are hex,
             and ours is empty until a room is joined. -->
        {#each store.roster as peer (peer.self ? 'self' : peer.id)}
          <tr class:self={peer.self}>
            <td>{peer.nickname}{peer.self ? ' (you)' : ''}</td>
            <td class="mono">{peer.id || '—'}</td>
            <!-- An em dash rather than a guess: a peer that publishes no
                 version is on a build from before this existed, which is
                 itself the answer. -->
            <td class="num mono">{peer.version || '—'}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </section>

  <section>
    <h3>Local capture</h3>
    <dl>
      <div><dt>Video encode</dt><dd>{cap.videoFps.toFixed(1)} fps</dd></div>
      <div><dt>Video bitrate</dt><dd>{Math.round(cap.videoKbps)} kbps</dd></div>
      <div><dt>Encoder queue</dt><dd>{cap.encodeQueue}</dd></div>
      <div><dt>Audio encode</dt><dd>{cap.audioFps.toFixed(1)} fps</dd></div>
      <div><dt>Audio bitrate</dt><dd>{Math.round(cap.audioKbps)} kbps</dd></div>
      <div><dt>Audio encoder queue</dt><dd>{cap.audioEncodeQueue}</dd></div>
      <div><dt>Keyframes</dt><dd>{cap.keyFrames}</dd></div>
      <div><dt>Frames dropped</dt><dd>{cap.dropped}</dd></div>
    </dl>
  </section>

  <section>
    <h3>Audio processing</h3>
    <dl>
      <div>
        <dt>Echo cancellation</dt>
        <dd class:on={cap.echoCancellation}>
          {cap.echoCancellation ? 'platform' : 'off'}
        </dd>
      </div>
      <div>
        <dt>Platform noise suppression</dt>
        <dd class:on={cap.noiseSuppression}>{cap.noiseSuppression ? 'on' : 'off'}</dd>
      </div>
      <div>
        <dt>Auto gain control</dt>
        <dd class:on={cap.autoGainControl}>{cap.autoGainControl ? 'on' : 'off'}</dd>
      </div>
      <div>
        <dt>Local denoiser</dt>
        <dd class:on={cap.denoiseActive}>{cap.denoiseActive ? 'rnnoise' : 'off'}</dd>
      </div>
      <div>
        <dt>Voice activity</dt>
        <dd class:on={store.speaking}>{store.speaking ? 'speaking' : 'silent'}</dd>
      </div>
    </dl>
    <p class="note">
      Echo cancellation is the platform's: only it can see what the speakers
      are playing. The local denoiser runs after it.
    </p>
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
            <!-- How fast this track is falling behind the clock that produced
                 it. Rising means a queue on the way here is filling: more is
                 being sent than is getting through. Audio carries it, because
                 it is the track produced on a fixed cadence. -->
            <th scope="col">Drift</th>
            <!-- How far behind the live edge this track has slipped since it
                 was subscribed. The backend rebuilds a subscription that gets
                 too far behind, so this returning to zero is that working. -->
            <th scope="col">Behind</th>
          </tr>
        </thead>
        <tbody>
          {#each inbound as t (t.label)}
            <tr>
              <th scope="row">{t.label.slice(3)}</th>
              <td>{Math.round(t.kbps)}</td>
              <td>{t.objects}</td>
              <td>{t.groups}</td>
              <td>
                {#if t.skewMillisPerSec !== undefined}
                  <!-- Above the publisher's own clock, not just above two
                       machines drifting apart. capture.ts steers its audio
                       epoch back by up to 1 ms per second while recovering
                       from a stall, and documents a stall on every launch — so
                       1 ms/s of apparent drift is a healthy publisher
                       correcting itself, and warning there painted this cell
                       yellow for minutes after every call started. -->
                  <span class={t.skewMillisPerSec > 2 ? 'warn' : ''}>
                    {t.skewMillisPerSec > 0 ? '+' : ''}{t.skewMillisPerSec.toFixed(1)} ms/s
                  </span>
                {:else}
                  —
                {/if}
              </td>
              <td>
                {#if t.lagMillis !== undefined}
                  <span class={t.lagMillis > 500 ? 'warn' : ''}>
                    {t.lagMillis > 0 ? '+' : ''}{t.lagMillis.toFixed(0)} ms
                  </span>
                {:else}
                  —
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {:else}
      <p class="empty">No subscriptions.</p>
    {/if}
  </section>

  <section class="wide">
    <h3>Decoders</h3>
    {#if store.playbackStats.length}
      <!-- Eleven columns will not fit a narrow drawer, so the table scrolls
           inside its own box rather than spilling out of the panel.

           Every width is declared and the layout is fixed, so a column cannot
           resize under its own contents. These numbers change four times a
           second, and a table that reflows on each of them is one nobody can
           read a trend off: the eye tracks a column that is holding still, not
           one that jumps a few pixels whenever a value gains a digit. -->
      <div class="scroll-x">
        <table class="decoders">
          <colgroup>
            <col style="width: 96px" /><col style="width: 54px" />
            <col style="width: 110px" /><col style="width: 96px" />
            <col style="width: 58px" /><col style="width: 58px" />
            <col style="width: 58px" /><col style="width: 68px" />
            <col style="width: 80px" /><col style="width: 82px" />
            <col style="width: 78px" />
          </colgroup>
          <thead>
            <tr>
              <th scope="col">Participant</th><th scope="col">Kind</th>
              <th scope="col">Codec</th><th scope="col">Resolution</th>
              <th scope="col">fps</th><th scope="col">Queue</th>
              <!-- Frames decoded and waiting for their turn against the audio
                   clock. Its own column beside the decode queue, which is the
                   other depth: the two answer different questions — one is the
                   decoder falling behind, the other is presentation holding
                   frames back on purpose. -->
              <th scope="col">Held</th>
              <th scope="col">Dropped</th>
              <!-- One per resolution the publisher has sent. A number that
                   climbs with the frame count means something is resizing the
                   canvas per frame, which clears it every time. -->
              <th scope="col">Resizes</th><th scope="col">Buffered</th>
              <th scope="col">Underruns</th>
              <!-- How far the picture led the sound at the last presented
                   frame. Nothing corrects from this, but without it a sync
                   regression is invisible. -->
              <th scope="col">A/V</th>
            </tr>
          </thead>
          <tbody>
            {#each store.playbackStats as s (s.handle)}
              <tr>
                <th scope="row">{s.participant}</th>
                <td>{s.kind}</td>
                <td class="codec">{s.codec}</td>
                <td>
                  {#if s.kind === 'video'}
                    {s.width && s.height ? `${s.width}×${s.height}` : '—'}
                  {:else}
                    —
                  {/if}
                </td>
                <td>{s.fps.toFixed(1)}</td>
                <td>{s.decodeQueue}</td>
                <td>{s.queued ?? '—'}</td>
                <td>{s.dropped}</td>
                <td>{s.resizes ?? '—'}</td>
                <td>
                  {#if s.buffered !== undefined}
                    {(s.buffered / 48).toFixed(0)} ms
                  {:else}
                    —
                  {/if}
                </td>
                <td class={s.underruns ? 'warn' : ''}>
                  {s.underruns ?? '—'}
                </td>
                <td>
                  {#if s.avOffsetMs !== undefined}
                    <span class={Math.abs(s.avOffsetMs) > 80 ? 'warn' : ''}>
                      {s.avOffsetMs > 0 ? '+' : ''}{s.avOffsetMs.toFixed(0)} ms
                    </span>
                  {:else if s.kind === 'video'}
                    <!-- No audio from this participant, so the frame is
                         presented as soon as it decodes. -->
                    free
                  {:else}
                    —
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
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

  /* A section that needs the drawer's full width rather than one grid cell. */
  .wide {
    grid-column: 1 / -1;
  }

  /* Our own row in the roster: the one everything else is compared against,
     so it is marked rather than left to be found by reading the names. */
  tr.self td {
    color: var(--text);
    font-weight: 500;
  }

  .scroll-x {
    overflow-x: auto;
    margin: 0 -4px;
    padding: 0 4px;
  }

  table {
    width: 100%;
    border-collapse: collapse;
    font-size: 12px;
  }

  .scroll-x table {
    /* Let the columns size to their content and the box scroll, instead of
       squeezing text until it wraps out of the cell. */
    width: auto;
    min-width: 100%;
    white-space: nowrap;
  }

  /* The decoders table is the exception: its columns come from the colgroup
     and stay there, because everything in it is a live number. Sizing to
     content would mean the whole row shifting sideways whenever a value
     crosses ten, or a participant's fps gains a digit — which is precisely
     when someone is staring at it. */
  .scroll-x table.decoders {
    table-layout: fixed;
    width: max-content;
  }

  .decoders th,
  .decoders td {
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .codec {
    text-align: left;
  }

  dd.on {
    color: var(--ok);
  }

  .note {
    margin: 8px 0 0;
    font-size: 11px;
    color: var(--text-faint);
    line-height: 1.45;
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
