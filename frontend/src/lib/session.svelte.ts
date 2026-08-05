/**
 * Application state, as Svelte 5 runes.
 *
 * One module-level store rather than props threaded through the tree: the
 * bridge pushes updates from outside the component graph, and the debug
 * panels need the same metrics and logs the conference view does.
 */

import { bridge } from './bridge';
import {
  capture,
  defaultAudioSettings,
  defaultVideoSettings,
  listDevices,
  type AudioSettings,
  type CaptureStats,
  type VideoSettings,
} from './capture';
import { playback, type PlaybackStats } from './playback';
import type {
  InviteMessage,
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

/**
 * How long a participant keeps their speaking indicator after their last
 * voice-active frame. Audio frames arrive every 20 ms, so this only has to
 * outlast ordinary jitter; the sender already applies its own release
 * hysteresis (see denoise.ts).
 */
const SPEAKING_TIMEOUT_MS = 400;

/**
 * The device and encoder selection, shared by the welcome screen and the
 * in-call device menu. It lives in the store because both need to read and
 * write the same values: joining by an invite link skips the welcome screen
 * entirely, so the in-call menu is the only place those choices can be made.
 */
export interface MediaSettings {
  cameraId: string;
  microphoneId: string;
  useVideo: boolean;
  useAudio: boolean;
  /** "WIDTHxHEIGHT", matching the welcome screen's options. */
  resolution: string;
  videoBitrate: number;
  audioBitrate: number;
  denoise: boolean;
}

/** One remote participant's tracks, keyed for the video grid. */
export interface RemoteView {
  id: string;
  nickname: string;
  videoHandle: number | null;
  audioHandle: number | null;
  /** True while they are speaking, for the tile's border. */
  speaking: boolean;
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
    echoCancellation: false, noiseSuppression: false, autoGainControl: false,
    denoiseActive: false,
  });
  playbackStats = $state<PlaybackStats[]>([]);

  /** Local preview stream, shown in the welcome screen and own tile. */
  previewStream = $state<MediaStream | null>(null);
  /**
   * What that stream is actually carrying.
   *
   * Mirrored as state because the stream object is now reused across a device
   * switch — capture adds and removes its tracks in place, so the local tile is
   * not rebound for a change that did not touch it. Nothing would observe a
   * track list read straight off the object.
   *
   * The camera is identified rather than counted, so the one change that does
   * need a rebind — a different camera behind the same stream — is visible as
   * one, while a microphone change stays invisible.
   */
  previewVideoId = $state('');
  previewAudio = $state(false);

  /** Whether the preview is carrying video at all. */
  get previewVideo(): boolean {
    return this.previewVideoId !== '';
  }

  media = $state<MediaSettings>({
    cameraId: '',
    microphoneId: '',
    useVideo: true,
    useAudio: true,
    resolution: '1280x720',
    videoBitrate: defaultVideoSettings.bitrate,
    audioBitrate: defaultAudioSettings.bitrate,
    denoise: defaultAudioSettings.denoise,
  });

  /** Cameras and microphones the browser is willing to name. */
  devices = $state<{ cameras: MediaDeviceInfo[]; microphones: MediaDeviceInfo[] }>({
    cameras: [],
    microphones: [],
  });

  /**
   * An invite link the backend received, waiting for the welcome screen to
   * act on it. Held in the store rather than delivered as an event because
   * the link can arrive before that screen has mounted.
   */
  pendingInvite = $state<InviteMessage | null>(null);

  /** True while the local microphone is picking up speech. */
  speaking = $state(false);
  /** Remote participant IDs currently speaking. */
  speakingPeers = $state<string[]>([]);

  /** Per-participant release timers for the speaking indicator. */
  #speakingTimers = new Map<string, ReturnType<typeof setTimeout>>();

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
        speaking: this.speakingPeers.includes(p.id),
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
        this.#dropRemoteState();
      }
    });

    bridge.onControl((msg) => {
      switch (msg.type) {
        case 'state': {
          const wasJoined = this.session.phase === 'joined';
          this.session = msg.state;
          // The session that produced those handles is gone: its decoders
          // decode nothing, and its participants are about to be
          // rediscovered under new handles. Capture keeps running, so the
          // call resumes the moment the relay is back.
          if (wasJoined && msg.state.phase === 'reconnecting') {
            this.#dropRemoteState();
          }
          break;
        }

        case 'requestKeyFrame':
          capture.forceKeyFrame();
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

        case 'invite':
          this.pendingInvite = msg.invite;
          break;

        case 'error':
          this.errors = appendCapped(this.errors, msg.error, 20);
          break;
      }
    });

    bridge.onMedia((frame) => playback.push(frame));

    // Voice activity, local and remote, drives the tiles' speaking border.
    capture.onVoice = (state) => {
      this.speaking = state.speaking;
    };
    playback.onVoice = (participant, speaking) => {
      this.#markSpeaking(participant, speaking);
    };

    this.#statsTimer = setInterval(() => {
      this.captureStats = capture.sampleStats();
      this.playbackStats = playback.sampleStats();
    }, STATS_INTERVAL_MS);
  }

  /** Forgets every remote track and participant, retiring their decoders. */
  #dropRemoteState(): void {
    playback.clear();
    this.tracks = [];
    this.participants = [];
    this.speakingPeers = [];
    this.#speakingTimers.forEach(clearTimeout);
    this.#speakingTimers.clear();
  }

  detach(): void {
    if (this.#statsTimer) clearInterval(this.#statsTimer);
    this.#statsTimer = null;
    this.#speakingTimers.forEach(clearTimeout);
    this.#speakingTimers.clear();
  }

  /**
   * Latches a remote participant as speaking and arms the release timer.
   *
   * Voice activity arrives per audio frame, so a participant is "still
   * speaking" until frames stop saying so — a timer rather than a false
   * edge, because a gap in delivery must not read as silence any more than
   * a pause between words should.
   */
  #markSpeaking(participant: string, speaking: boolean): void {
    const existing = this.#speakingTimers.get(participant);
    if (existing) clearTimeout(existing);

    if (!speaking) {
      this.#speakingTimers.delete(participant);
      this.speakingPeers = this.speakingPeers.filter((id) => id !== participant);
      return;
    }

    if (!this.speakingPeers.includes(participant)) {
      this.speakingPeers = [...this.speakingPeers, participant];
    }
    this.#speakingTimers.set(
      participant,
      setTimeout(() => {
        this.#speakingTimers.delete(participant);
        this.speakingPeers = this.speakingPeers.filter((id) => id !== participant);
      }, SPEAKING_TIMEOUT_MS),
    );
  }

  /** The current selection as the capture layer's video settings, or null. */
  get videoSettings(): VideoSettings | null {
    if (!this.media.useVideo) return null;
    const [width, height] = this.media.resolution.split('x').map(Number);
    return {
      deviceId: this.media.cameraId || undefined,
      width,
      height,
      framerate: defaultVideoSettings.framerate,
      bitrate: this.media.videoBitrate,
    };
  }

  /** The current selection as the capture layer's audio settings, or null. */
  get audioSettings(): AudioSettings | null {
    if (!this.media.useAudio) return null;
    return {
      deviceId: this.media.microphoneId || undefined,
      bitrate: this.media.audioBitrate,
      denoise: this.media.denoise,
    };
  }

  /** Opens the selected devices and shows a preview. */
  async openPreview(): Promise<void> {
    try {
      await capture.open(this.videoSettings, this.audioSettings);
    } finally {
      // In a finally because a partial failure still changed things: one kind
      // can open while the other is refused, and the preview should show what
      // is really there rather than what was asked for.
      this.#syncPreview();
    }
    await this.refreshDevices();
  }

  /** Republishes what capture is holding as observable state. */
  #syncPreview(): void {
    this.previewStream = capture.stream;
    this.previewVideoId = capture.stream?.getVideoTracks()[0]?.id ?? '';
    this.previewAudio = !!capture.stream?.getAudioTracks().length;
  }

  /**
   * Reloads the device list. Labels stay empty until capture permission has
   * been granted, so this is worth repeating after a stream opens.
   */
  async refreshDevices(): Promise<void> {
    const found = await listDevices();
    this.devices = found;
    if (!this.media.cameraId && found.cameras.length) {
      this.media.cameraId = found.cameras[0].deviceId;
    }
    if (!this.media.microphoneId && found.microphones.length) {
      this.media.microphoneId = found.microphones[0].deviceId;
    }
  }

  /**
   * Applies the current selection to a live call, switching devices without
   * leaving the room. Falls back to just reopening the preview when idle.
   */
  async applyMedia(): Promise<void> {
    if (this.session.phase !== 'joined') {
      await this.openPreview();
      return;
    }
    try {
      await capture.switchDevices(this.videoSettings, this.audioSettings);
    } finally {
      this.#syncPreview();
    }
    await this.refreshDevices();
  }

  /**
   * Joins the room, then starts publishing. The order matters: the backend
   * must have a session before the frontend declares its tracks, since
   * declaring one republishes the catalog.
   */
  async join(relay: string, room: string, nickname: string): Promise<void> {
    this.errors = [];
    // Resume playback audio from the join click: an AudioContext created
    // without a user gesture starts suspended and stays silent.
    await playback.resume();

    if (!capture.stream) {
      await this.openPreview();
    }

    bridge.send({ type: 'join', join: { relay, room, nickname } });
    await this.#awaitPhase('joined', 15000);
    await capture.start(this.videoSettings, this.audioSettings);
  }

  leave(): void {
    capture.stop();
    playback.clear();
    this.#syncPreview();
    this.tracks = [];
    this.participants = [];
    this.speaking = false;
    this.speakingPeers = [];
    this.#speakingTimers.forEach(clearTimeout);
    this.#speakingTimers.clear();
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
