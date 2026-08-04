/**
 * Application state, as Svelte 5 runes.
 *
 * One module-level store rather than props threaded through the tree: the
 * bridge pushes updates from outside the component graph, and the debug
 * panels need the same metrics and logs the conference view does.
 */

import { bridge } from './bridge';
import { capture, type AudioSettings, type CaptureStats, type VideoSettings } from './capture';
import { playback, type PlaybackStats } from './playback';
import type {
  LogEntry,
  Metrics,
  Participant,
  RemoteTrack,
  SessionState,
} from './protocol';

/** How many samples the plots retain — 240 at 250 ms is a minute. */
export const HISTORY_LENGTH = 240;

/** How many log records the panel keeps before discarding the oldest. */
const LOG_LIMIT = 1500;

/** How often the frontend samples its own encode/decode counters. */
const STATS_INTERVAL_MS = 250;

/** One remote participant's tracks, keyed for the video grid. */
export interface RemoteView {
  id: string;
  nickname: string;
  videoHandle: number | null;
  audioHandle: number | null;
}

class Store {
  session = $state<SessionState>({ phase: 'idle' });
  connected = $state(false);
  participants = $state<Participant[]>([]);
  tracks = $state<RemoteTrack[]>([]);
  logs = $state<LogEntry[]>([]);
  metrics = $state<Metrics | null>(null);
  history = $state<Metrics[]>([]);
  errors = $state<string[]>([]);

  captureStats = $state<CaptureStats>({
    videoFps: 0, videoKbps: 0, encodeQueue: 0,
    audioFps: 0, audioKbps: 0, keyFrames: 0, dropped: 0,
  });
  playbackStats = $state<PlaybackStats[]>([]);

  /** Local preview stream, shown in the welcome screen and own tile. */
  previewStream = $state<MediaStream | null>(null);

  logLevel = $state('INFO');
  logPaused = $state(false);
  /** `?debug=1` opens the drawer at start; Cmd+D toggles it thereafter. */
  debugOpen = $state(new URLSearchParams(location.search).get('debug') === '1');

  #statsTimer: ReturnType<typeof setInterval> | null = null;

  /**
   * Merges participants and tracks into what the grid renders. A
   * participant with no video still gets a tile — they are in the call.
   */
  get remotes(): RemoteView[] {
    return this.participants.map((p) => {
      const video = this.tracks.find((t) => t.participant === p.id && t.config.kind === 'video');
      const audio = this.tracks.find((t) => t.participant === p.id && t.config.kind === 'audio');
      return {
        id: p.id,
        nickname: p.nickname || video?.nickname || audio?.nickname || p.id,
        videoHandle: video?.handle ?? null,
        audioHandle: audio?.handle ?? null,
      };
    });
  }

  /** Wires the bridge into this store. Call once at startup. */
  attach(): void {
    bridge.onStatus((connected) => {
      this.connected = connected;
      if (!connected) {
        // The backend is gone: its session and every decoder that fed
        // from it are void.
        this.tracks = [];
        this.participants = [];
        playback.clear();
      }
    });

    bridge.onControl((msg) => {
      switch (msg.type) {
        case 'state':
          this.session = msg.state;
          break;

        case 'participants':
          this.participants = msg.participants ?? [];
          break;

        case 'remoteTrack': {
          const track = msg.track;
          this.tracks = [...this.tracks.filter((t) => t.handle !== track.handle), track];
          void playback.add(track);
          break;
        }

        case 'trackGone':
          playback.remove(msg.trackGone.handle);
          this.tracks = this.tracks.filter((t) => t.handle !== msg.trackGone.handle);
          break;

        case 'log':
          if (this.logPaused) break;
          this.logs = appendCapped(this.logs, msg.log, LOG_LIMIT);
          break;

        case 'metrics':
          this.metrics = msg.metrics;
          this.history = appendCapped(this.history, msg.metrics, HISTORY_LENGTH);
          break;

        case 'error':
          this.errors = appendCapped(this.errors, msg.error, 20);
          break;
      }
    });

    bridge.onMedia((frame) => playback.push(frame));

    this.#statsTimer = setInterval(() => {
      this.captureStats = capture.sampleStats();
      this.playbackStats = playback.sampleStats();
    }, STATS_INTERVAL_MS);
  }

  detach(): void {
    if (this.#statsTimer) clearInterval(this.#statsTimer);
    this.#statsTimer = null;
  }

  /** Opens the selected devices and shows a preview. */
  async openPreview(video: VideoSettings | null, audio: AudioSettings | null): Promise<void> {
    const stream = await capture.open(video, audio);
    this.previewStream = stream;
  }

  /**
   * Joins the room, then starts publishing. The order matters: the backend
   * must have a session before the frontend declares its tracks, since
   * declaring one republishes the catalog.
   */
  async join(
    relay: string,
    room: string,
    nickname: string,
    video: VideoSettings | null,
    audio: AudioSettings | null,
  ): Promise<void> {
    this.errors = [];
    // Resume playback audio from the join click: an AudioContext created
    // without a user gesture starts suspended and stays silent.
    await playback.resume();

    if (!capture.stream) {
      await this.openPreview(video, audio);
    }

    bridge.send({ type: 'join', join: { relay, room, nickname } });
    await this.#awaitPhase('joined', 15000);
    await capture.start(video, audio);
  }

  leave(): void {
    capture.stop();
    playback.clear();
    this.previewStream = null;
    this.tracks = [];
    this.participants = [];
    bridge.send({ type: 'leave' });
  }

  setLogLevel(level: string): void {
    this.logLevel = level;
    bridge.send({ type: 'logLevel', logLevel: level });
  }

  clearLogs(): void {
    this.logs = [];
  }

  /**
   * Resolves once the backend reports the wanted phase, or rejects on a
   * failed join. Without this the capture pipeline could start publishing
   * into a session that does not exist yet.
   */
  #awaitPhase(phase: SessionState['phase'], timeoutMs: number): Promise<void> {
    if (this.session.phase === phase) return Promise.resolve();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        stop();
        reject(new Error('timed out waiting for the relay'));
      }, timeoutMs);
      const off = bridge.onControl((msg) => {
        if (msg.type !== 'state') return;
        if (msg.state.phase === phase) {
          stop();
          resolve();
        } else if (msg.state.phase === 'failed') {
          stop();
          reject(new Error(msg.state.detail || 'could not join the room'));
        }
      });
      function stop() {
        clearTimeout(timer);
        off();
      }
    });
  }
}

function appendCapped<T>(list: T[], item: T, limit: number): T[] {
  const next = list.length >= limit ? list.slice(list.length - limit + 1) : list.slice();
  next.push(item);
  return next;
}

export const store = new Store();
