<script lang="ts">
  /**
   * Transport and media health as small multiples over a shared one-minute
   * window, plus the table view that makes every plotted number readable
   * without relying on color.
   */
  import { store } from '../../lib/session.svelte';
  import Plot from './Plot.svelte';

  /**
   * The three categorical slots that clear the all-pairs CVD gates on this
   * dark surface (validated: worst pair ΔE 9.4 deutan, 20.9 normal). Never
   * add a fourth here — yellow beside orange fails the floor.
   */
  const BLUE = '#3987e5';
  const ORANGE = '#d95926';

  let showTable = $state(false);

  const history = $derived(store.history);
  const pick = (fn: (m: (typeof history)[number]) => number) => history.map(fn);

  /**
   * Smoothed against the interval's worst sample, not against the minimum.
   * The minimum is the path's propagation floor: near-constant, so as a line
   * it carries no information. The peak is where the trouble shows — the gap
   * between the two lines is queueing delay, and a spike a smoothed average
   * would hide is exactly what makes a call stutter. The floor is still worth
   * knowing, so it stays as a number in the tile below.
   */
  const rtt = $derived([
    { label: 'smoothed', color: BLUE, values: pick((m) => m.rttMs) },
    { label: 'peak', color: ORANGE, values: pick((m) => m.peakRttMs) },
  ]);

  const loss = $derived([
    { label: 'loss', color: ORANGE, values: pick((m) => m.lossPercent) },
  ]);

  const transport = $derived([
    { label: 'sent', color: BLUE, values: pick((m) => m.sendKbps) },
    { label: 'received', color: ORANGE, values: pick((m) => m.receiveKbps) },
  ]);

  const moq = $derived([
    { label: 'published', color: BLUE, values: pick((m) => m.publishKbps) },
    { label: 'subscribed', color: ORANGE, values: pick((m) => m.subscribeKbps) },
  ]);

  const window = $derived([
    { label: 'congestion window', color: BLUE, values: pick((m) => m.cwnd / 1024) },
    { label: 'in flight', color: ORANGE, values: pick((m) => m.bytesInFlight / 1024) },
  ]);

  const objects = $derived([
    { label: 'out', color: BLUE, values: pick((m) => m.objectsOutPerSec) },
    { label: 'in', color: ORANGE, values: pick((m) => m.objectsInPerSec) },
  ]);

  const current = $derived(store.metrics);
</script>

<div class="panel">
  <div class="tiles">
    <div class="tile">
      <span class="label">Round trip</span>
      <span class="figure">
        {(current?.rttMs ?? 0).toFixed(1)}<em>ms · floor {(current?.minRttMs ?? 0).toFixed(1)}</em>
      </span>
    </div>
    <div class="tile">
      <span class="label">Packet loss</span>
      <span class="figure">{(current?.lossPercent ?? 0).toFixed(2)}<em>%</em></span>
    </div>
    <div class="tile">
      <span class="label">Publishing</span>
      <span class="figure">{Math.round(current?.publishKbps ?? 0)}<em>kbps</em></span>
    </div>
    <div class="tile">
      <span class="label">Subscribing</span>
      <span class="figure">{Math.round(current?.subscribeKbps ?? 0)}<em>kbps</em></span>
    </div>
    <div class="tile">
      <span class="label">Congestion</span>
      <span class="figure state">{current?.congestionState ?? '—'}</span>
    </div>
    <div class="tile">
      <span class="label">Packets lost</span>
      <span class="figure">{current?.packetsLost ?? 0}<em>of {current?.packetsSent ?? 0}</em></span>
    </div>
  </div>

  <div class="grid">
    <Plot title="Round-trip time" unit="ms" series={rtt} precision={1} />
    <Plot title="Packet loss" unit="%" series={loss} precision={2} />
    <Plot title="QUIC throughput" unit="kbps" series={transport} />
    <Plot title="MOQ object throughput" unit="kbps" series={moq} />
    <Plot title="Congestion window" unit="KiB" series={window} />
    <Plot title="Objects per second" unit="obj/s" series={objects} />
  </div>

  <div class="table-toggle">
    <button class="ghost" onclick={() => (showTable = !showTable)}>
      {showTable ? 'Hide' : 'Show'} table view
    </button>
  </div>

  {#if showTable}
    <table>
      <caption>Latest sample of every plotted measure</caption>
      <thead>
        <tr><th scope="col">Measure</th><th scope="col">Value</th><th scope="col">Unit</th></tr>
      </thead>
      <tbody>
        <tr><th scope="row">Smoothed RTT</th><td>{(current?.rttMs ?? 0).toFixed(2)}</td><td>ms</td></tr>
        <tr><th scope="row">Minimum RTT</th><td>{(current?.minRttMs ?? 0).toFixed(2)}</td><td>ms</td></tr>
        <tr><th scope="row">Latest RTT</th><td>{(current?.latestRttMs ?? 0).toFixed(2)}</td><td>ms</td></tr>
        <tr><th scope="row">Peak RTT (this interval)</th><td>{(current?.peakRttMs ?? 0).toFixed(2)}</td><td>ms</td></tr>
        <tr><th scope="row">Queueing delay (peak − floor)</th><td>{Math.max(0, (current?.peakRttMs ?? 0) - (current?.minRttMs ?? 0)).toFixed(2)}</td><td>ms</td></tr>
        <tr><th scope="row">Packet loss</th><td>{(current?.lossPercent ?? 0).toFixed(3)}</td><td>%</td></tr>
        <tr><th scope="row">Packets lost per second</th><td>{(current?.packetsLostPerSec ?? 0).toFixed(2)}</td><td>1/s</td></tr>
        <tr><th scope="row">QUIC sent</th><td>{Math.round(current?.sendKbps ?? 0)}</td><td>kbps</td></tr>
        <tr><th scope="row">QUIC received</th><td>{Math.round(current?.receiveKbps ?? 0)}</td><td>kbps</td></tr>
        <tr><th scope="row">MOQ published</th><td>{Math.round(current?.publishKbps ?? 0)}</td><td>kbps</td></tr>
        <tr><th scope="row">MOQ subscribed</th><td>{Math.round(current?.subscribeKbps ?? 0)}</td><td>kbps</td></tr>
        <tr><th scope="row">Congestion window</th><td>{Math.round((current?.cwnd ?? 0) / 1024)}</td><td>KiB</td></tr>
        <tr><th scope="row">Bytes in flight</th><td>{Math.round((current?.bytesInFlight ?? 0) / 1024)}</td><td>KiB</td></tr>
        <tr><th scope="row">Packets in flight</th><td>{current?.packetsInFlight ?? 0}</td><td>count</td></tr>
        <tr><th scope="row">Objects out</th><td>{(current?.objectsOutPerSec ?? 0).toFixed(1)}</td><td>obj/s</td></tr>
        <tr><th scope="row">Objects in</th><td>{(current?.objectsInPerSec ?? 0).toFixed(1)}</td><td>obj/s</td></tr>
        <tr><th scope="row">Groups out</th><td>{(current?.groupsOutPerSec ?? 0).toFixed(2)}</td><td>grp/s</td></tr>
        <tr><th scope="row">Frames dropped by the bridge</th><td>{current?.bridgeDropped ?? 0}</td><td>count</td></tr>
      </tbody>
    </table>
  {/if}
</div>

<style>
  .panel {
    padding: 12px;
    overflow-y: auto;
    height: 100%;
  }

  .tiles {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
    gap: 8px;
    margin-bottom: 12px;
  }

  .tile {
    background: var(--bg-sunken);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 7px 10px;
    display: flex;
    flex-direction: column;
    gap: 1px;
    min-width: 0;
  }

  .label {
    font-size: 10px;
    color: var(--text-faint);
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }

  .figure {
    font-size: 17px;
    font-weight: 600;
    color: var(--text);
  }

  .figure.state {
    font-size: 13px;
    font-weight: 500;
    color: var(--text-dim);
  }

  .figure em {
    font-style: normal;
    font-size: 10px;
    font-weight: 400;
    color: var(--text-faint);
    margin-left: 4px;
  }

  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
    gap: 8px;
  }

  .table-toggle {
    margin-top: 10px;
  }

  table {
    width: 100%;
    border-collapse: collapse;
    margin-top: 8px;
    font-size: 12px;
  }

  caption {
    text-align: left;
    font-size: 11px;
    color: var(--text-faint);
    padding-bottom: 6px;
  }

  th,
  td {
    text-align: left;
    padding: 4px 8px;
    border-bottom: 1px solid var(--border);
  }

  thead th {
    color: var(--text-faint);
    font-weight: 500;
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }

  tbody th {
    font-weight: 400;
    color: var(--text-dim);
  }

  td {
    font-family: var(--mono);
    font-variant-numeric: tabular-nums;
  }
</style>
