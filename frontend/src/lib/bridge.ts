/**
 * WebSocket client for the Go backend.
 *
 * The backend serves its endpoint (URL plus a per-run token) at /__bridge,
 * so this module discovers where to connect rather than hardcoding a port.
 * It reconnects on its own: the WebView can be reloaded during development
 * without restarting the app.
 */

import {
  decodeFrame,
  encodeFrame,
  type ClientMessage,
  type ClientReport,
  type Endpoint,
  type MediaFrame,
  type ServerMessage,
} from './protocol';

/** Delay before retrying a dropped or refused connection. */
const RECONNECT_DELAY_MS = 1000;

type ControlListener = (msg: ServerMessage) => void;
type MediaListener = (frame: MediaFrame) => void;
type StatusListener = (connected: boolean) => void;

export class Bridge {
  #ws: WebSocket | null = null;
  #endpoint: Endpoint | null = null;
  #closed = false;
  #retry: ReturnType<typeof setTimeout> | null = null;

  #control = new Set<ControlListener>();
  #media = new Set<MediaListener>();
  #status = new Set<StatusListener>();

  /** True while the socket is open. */
  connected = false;

  /**
   * The build this app was packaged as, from the endpoint descriptor.
   *
   * Empty until the descriptor has been fetched, which is the first thing the
   * connection does. Kept here rather than in the store because it belongs to
   * the binary rather than to any session — it does not change, and it is the
   * same answer whether or not a room was ever joined.
   */
  version = '';
  os = '';

  /** Opens the connection and keeps it open until close(). */
  async start(): Promise<void> {
    this.#closed = false;
    await this.#connect();
  }

  close(): void {
    this.#closed = true;
    if (this.#retry) clearTimeout(this.#retry);
    this.#retry = null;
    this.#ws?.close();
    this.#ws = null;
  }

  onControl(fn: ControlListener): () => void {
    this.#control.add(fn);
    return () => this.#control.delete(fn);
  }

  onMedia(fn: MediaListener): () => void {
    this.#media.add(fn);
    return () => this.#media.delete(fn);
  }

  onStatus(fn: StatusListener): () => void {
    this.#status.add(fn);
    return () => this.#status.delete(fn);
  }

  /** Sends a JSON control message. Silently drops when disconnected. */
  /** Sends a control message. Reports whether the socket took it. */
  send(msg: ClientMessage): boolean {
    if (this.#ws?.readyState !== WebSocket.OPEN) return false;
    this.#ws.send(JSON.stringify(msg));
    return true;
  }

  /**
   * Reports a WebView-side event into the backend log, so the debug panel
   * shows both halves of the app in one stream. Also echoes to the
   * devtools console, which is where it lands if the socket is down.
   */
  report(level: ClientReport['level'], msg: string, attrs?: Record<string, string>): void {
    const sink = level === 'ERROR' ? console.error : level === 'WARN' ? console.warn : console.log;
    sink(`[${level}] ${msg}`, attrs ?? '');
    this.send({ type: 'report', report: { level, msg, attrs } });
  }

  /**
   * Sends one encoded media frame.
   *
   * Returns false when the socket is closed or its send buffer is already
   * deep, which the caller treats as backpressure: dropping a live frame
   * beats growing an unbounded queue of stale ones. The threshold is
   * generous because loopback drains at hundreds of MB/s — reaching it
   * means something is genuinely wrong.
   */
  sendFrame(frame: MediaFrame): boolean {
    const ws = this.#ws;
    if (ws?.readyState !== WebSocket.OPEN) return false;
    if (ws.bufferedAmount > 4 * 1024 * 1024) return false;
    ws.send(encodeFrame(frame));
    return true;
  }

  async #connect(): Promise<void> {
    if (this.#closed) return;
    try {
      if (!this.#endpoint) {
        const res = await fetch('/__bridge', { cache: 'no-store' });
        if (!res.ok) throw new Error(`bridge endpoint: HTTP ${res.status}`);
        this.#endpoint = (await res.json()) as Endpoint;
        // Published as soon as it is known, which is before any session
        // exists — the welcome screen shows the version without waiting for
        // a room.
        this.version = this.#endpoint.version ?? '';
        this.os = this.#endpoint.os ?? '';
      }
      const url = `${this.#endpoint.url}?token=${encodeURIComponent(this.#endpoint.token)}`;
      const ws = new WebSocket(url);
      ws.binaryType = 'arraybuffer';
      this.#ws = ws;

      ws.onopen = () => {
        this.connected = true;
        this.#status.forEach((fn) => fn(true));
      };
      ws.onmessage = (ev) => this.#dispatch(ev.data);
      ws.onclose = () => {
        this.connected = false;
        this.#status.forEach((fn) => fn(false));
        this.#scheduleRetry();
      };
      ws.onerror = () => {
        // An error is always followed by close, which handles the retry.
        ws.close();
      };
    } catch (err) {
      console.error('bridge connect failed', err);
      // A stale endpoint (the app restarted on a new port) has to be
      // re-fetched, so forget it before retrying.
      this.#endpoint = null;
      this.#scheduleRetry();
    }
  }

  #scheduleRetry(): void {
    if (this.#closed || this.#retry) return;
    this.#retry = setTimeout(() => {
      this.#retry = null;
      void this.#connect();
    }, RECONNECT_DELAY_MS);
  }

  #dispatch(data: unknown): void {
    if (typeof data === 'string') {
      let msg: ServerMessage;
      try {
        msg = JSON.parse(data) as ServerMessage;
      } catch (err) {
        console.error('bridge: bad control message', err);
        return;
      }
      this.#control.forEach((fn) => fn(msg));
      return;
    }
    if (data instanceof ArrayBuffer) {
      const frame = decodeFrame(data);
      if (!frame) {
        console.warn('bridge: undecodable media frame');
        return;
      }
      this.#media.forEach((fn) => fn(frame));
    }
  }
}

export const bridge = new Bridge();
