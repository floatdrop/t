/**
 * Application state, as Svelte 5 runes.
 *
 * One module-level store rather than props threaded through the tree: the
 * bridge pushes updates from outside the component graph, and the debug
 * panels need the same metrics and logs the conference view does.
 */

import { untrack } from 'svelte';
import { bridge } from './bridge';
import {
  capture,
  defaultAudioSettings,
  defaultVideoSettings,
  listDevices,
  RESOLUTION_AUTO,
  screenVideoSettings,
  type AudioSettings,
  type CaptureStats,
  type VideoSettings,
  type VideoSource,
} from './capture';
import { autoVideoBitrate, autoVideoRung } from './layout';
import { playback, type PlaybackStats } from './playback';
import type {
  InviteMessage,
  UpdateInfo,
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
 * How long the window has to hold still before Auto re-measures it.
 *
 * A resize arrives per frame while a window is being dragged, and a
 * re-measurement that crosses a rung boundary rebuilds the whole video
 * pipeline. Auto only acts on the rung, not the pixel, so this only has to
 * outlast the drag itself.
 */
const RESIZE_SETTLE_MS = 300;

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
  /**
   * What useVideo publishes. A screen replaces the camera rather than joining
   * it, because a participant has one video track in the catalog.
   */
  videoSource: VideoSource;
  /**
   * "WIDTHxHEIGHT", one of the rungs in VIDEO_LADDER, or RESOLUTION_AUTO to
   * follow the grid — see the store's autoResolution.
   */
  resolution: string;
  videoBitrate: number;
  audioBitrate: number;
  denoise: boolean;
}

/** One remote participant's tracks, keyed for the video grid. */
export interface RemoteView {
  id: string;
  nickname: string;
  /** The build they are running, empty from a peer that does not publish one. */
  version: string;
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
  /**
   * Faults worth interrupting someone mid-call for, newest last.
   *
   * The debug log has every one of these already, and that was the whole
   * problem: a call whose microphone never started, or whose camera died on
   * its way into the encoder, looks entirely normal from the inside. The
   * person it happened to is the last to know, because their own tile is drawn
   * from the capture stream rather than the encode path. So the failures that
   * cost a participant their picture or their voice are said out loud here,
   * where the conference view can show them, rather than only in a panel
   * nobody has open.
   */
  errors = $state<string[]>([]);

  captureStats = $state<CaptureStats>({
    videoFps: 0, videoKbps: 0, encodeQueue: 0,
    audioFps: 0, audioKbps: 0, audioEncodeQueue: 0, keyFrames: 0, dropped: 0,
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
    videoSource: 'camera',
    // Every call starts by following the grid. A fixed size is a deliberate
    // override, and leave() puts this back so the next call starts from Auto
    // again rather than from whatever one room happened to need.
    resolution: RESOLUTION_AUTO,
    videoBitrate: defaultVideoSettings.bitrate,
    audioBitrate: defaultAudioSettings.bitrate,
    denoise: defaultAudioSettings.denoise,
  });

  /**
   * The window as Auto last measured it.
   *
   * Held as state rather than read from `window` where it is needed, because
   * the size Auto picks has to be recomputed when the window changes and
   * nothing observes a property read off the global. The pixel ratio travels
   * with it: it changes when the window is dragged to a different display,
   * which is also a resize.
   */
  viewportWidth = $state(window.innerWidth);
  pixelRatio = $state(window.devicePixelRatio || 1);

  #autoRung = $derived(
    autoVideoRung({
      // Our own tile is in the grid too, and on an empty call it is the only
      // one — which is why this is not simply the participant count.
      tiles: this.participants.length + 1,
      viewportWidth: this.viewportWidth,
      pixelRatio: this.pixelRatio,
      bitrate: this.media.videoBitrate,
    }),
  );

  /**
   * What Auto is asking for, as "WIDTHxHEIGHT".
   *
   * A string rather than the rung itself so that watchers see a change only
   * when the size really changed: a participant joining a call that is already
   * three across moves nothing, and re-encoding for it would cost every
   * subscriber a decoder reconfigure and a wait for the next keyframe.
   */
  autoResolution = $derived(`${this.#autoRung.width}x${this.#autoRung.height}`);

  /** How that size is written, for the picker to show alongside "Auto". */
  get autoLabel(): string {
    return this.#autoRung.label;
  }

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

  /**
   * A release newer than the one running, once the backend has found one.
   *
   * Null is the normal state and the normal outcome: the check runs once at
   * startup and says nothing at all unless there is something to offer.
   */
  update = $state<UpdateInfo | null>(null);

  /**
   * The build this app is, for the welcome screen and the debug panel.
   *
   * State rather than a read through to the bridge: the version arrives with
   * the endpoint descriptor, which is fetched asynchronously as the connection
   * opens, so a plain property read renders as empty once and never corrects
   * itself. Set from the status callback, which fires after the fetch.
   */
  version = $state('');

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
        version: p.version ?? '',
        videoHandle: video?.handle ?? null,
        audioHandle: audio?.handle ?? null,
        speaking: this.speakingPeers.includes(p.id),
      };
    });
  }

  /**
   * Everyone in the room with a version to report, us first.
   *
   * Ourselves included because the question this answers is "who is on what",
   * and an answer that omits the person asking is one subtraction away from
   * useless — comparing your own build against the room is the entire point.
   */
  get roster(): { id: string; nickname: string; version: string; self: boolean }[] {
    return [
      {
        id: this.session.id ?? '',
        nickname: this.session.nickname || 'me',
        version: this.version,
        self: true,
      },
      ...this.remotes.map((r) => ({
        id: r.id,
        nickname: r.nickname,
        version: r.version,
        self: false,
      })),
    ];
  }

  /** Wires the bridge into this store. Call once at startup. */
  attach(): void {
    bridge.onStatus((connected) => {
      this.connected = connected;
      // The descriptor has been fetched by the time the socket is open.
      if (connected && bridge.version) this.version = bridge.version;
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
          // Said out loud rather than dropped on the floor. A sink that fails
          // to build is a participant nobody can hear or see, and a bare `void`
          // leaves that as an unhandled rejection nobody is reading.
          void playback.add(track).catch((err) => {
            bridge.report('ERROR', 'could not start playback for a remote track', {
              participant: track.participant,
              kind: track.config.kind,
              err: String(err),
            });
            const who = track.nickname || track.participant;
            this.reportFault(`Cannot play ${track.config.kind} from ${who}.`);
          });
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

        case 'update':
          this.update = msg.update;
          break;

        case 'error':
          this.reportFault(msg.error);
          break;
      }
    });

    bridge.onMedia((frame) => playback.push(frame));

    // Voice activity, local and remote, drives the tiles' speaking border.
    capture.onVoice = (state) => {
      this.speaking = state.speaking;
    };
    // What capture cannot recover from. It reports these to the log as well,
    // but the log is not where someone in a call is looking.
    capture.onFailure = (detail) => {
      this.reportFault(detail);
    };
    // A screen share that ended outside the app leaves the setting claiming to
    // share something that is gone, so put it back where it was.
    capture.onVideoSourceLost = () => {
      if (this.media.videoSource !== 'screen') return;
      void this.stopScreenShare();
    };
    playback.onVoice = (participant, speaking) => {
      this.#markSpeaking(participant, speaking);
    };

    this.#statsTimer = setInterval(() => {
      this.captureStats = capture.sampleStats();
      this.playbackStats = playback.sampleStats();
    }, STATS_INTERVAL_MS);

    window.addEventListener('resize', this.#onResize);

    // Auto follows the call rather than being chosen once: people join, people
    // leave, the window is resized, and what we publish keeps up without anyone
    // opening a menu. An effect root because this store is not a component and
    // has no lifecycle of its own — detach() disposes it.
    this.#stopAuto = $effect.root(() => {
      $effect(() => {
        const wanted = this.autoResolution;
        if (this.media.resolution !== RESOLUTION_AUTO) return;
        // Only what Auto actually governs: a screen carries its own size, and a
        // camera that is off has no picture to resize. Both are picked up when
        // they come back, since the toggle applies the settings itself.
        if (!this.media.useVideo || this.media.videoSource !== 'camera') return;
        // Only on a live call. Off one there is nothing publishing to change,
        // and openPreview reads the same value when it next opens the camera.
        if (this.session.phase !== 'joined') return;
        if (wanted === this.#autoApplied) return;
        this.#autoApplied = wanted;

        // Untracked, because applyMedia reads nearly every media setting on its
        // way through and an effect that depended on all of them would rebuild
        // the video pipeline for a microphone change too.
        untrack(() => {
          void this.applyMedia().catch((err) => {
            bridge.report('WARN', 'could not apply the automatic resolution', {
              wanted,
              err: String(err),
            });
          });
        });
      });
    });
  }

  /** The last size Auto acted on, so a re-run that changes nothing does nothing. */
  #autoApplied = '';
  #stopAuto: (() => void) | null = null;
  #resizeTimer: ReturnType<typeof setTimeout> | null = null;

  /** Re-measures the window once it has stopped moving. */
  #onResize = (): void => {
    if (this.#resizeTimer) clearTimeout(this.#resizeTimer);
    this.#resizeTimer = setTimeout(() => {
      this.#resizeTimer = null;
      this.viewportWidth = window.innerWidth;
      this.pixelRatio = window.devicePixelRatio || 1;
    }, RESIZE_SETTLE_MS);
  };

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
    if (this.#resizeTimer) clearTimeout(this.#resizeTimer);
    this.#resizeTimer = null;
    window.removeEventListener('resize', this.#onResize);
    this.#stopAuto?.();
    this.#stopAuto = null;
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

    // A screen takes none of the camera's settings. Its size and rate are its
    // own (see screenVideoSettings), and it carries no device: a display is not
    // one, and putting the camera selection here would make picking a different
    // camera mid-share look like a change and re-open the screen picker.
    //
    // Auto is a camera setting and stops here. It sizes a picture to the tile it
    // will be shown in, which is the right question for a face and the wrong one
    // for a screen: a shared desktop is read, not watched, and is worth 1080p
    // whatever the grid is doing — often in the expanded tile, where the grid's
    // arithmetic does not apply at all. The effect that follows the grid skips a
    // share for the same reason.
    if (this.media.videoSource === 'screen') {
      return {
        source: 'screen',
        width: screenVideoSettings.width,
        height: screenVideoSettings.height,
        framerate: screenVideoSettings.framerate,
        bitrate: screenVideoSettings.bitrate,
      };
    }

    // Auto names no size of its own; it resolves to a rung, which is what the
    // capture layer is given. Nothing below this point knows the difference,
    // so a size that changed because someone joined rebuilds the pipeline by
    // exactly the same path as one that was picked by hand.
    const auto = this.media.resolution === RESOLUTION_AUTO;
    const picked = auto ? this.autoResolution : this.media.resolution;
    const [width, height] = picked.split('x').map(Number);
    return {
      source: 'camera',
      deviceId: this.media.cameraId || undefined,
      width,
      height,
      framerate: defaultVideoSettings.framerate,
      // Auto caps the budget to the size it settled on; a size chosen by hand
      // is an instruction and spends what was selected.
      bitrate: auto
        ? autoVideoBitrate(this.#autoRung, this.media.videoBitrate)
        : this.media.videoBitrate,
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

  /**
   * Whether the camera was publishing before a screen share began.
   *
   * Kept so that stopping a share returns things as they were. Coming back to a
   * live camera you had deliberately switched off would be a surprise of the
   * worst kind — the camera is not a control to turn on for someone.
   */
  #videoBeforeShare = false;

  /** True while the screen is what gets published. */
  get sharingScreen(): boolean {
    return this.media.useVideo && this.media.videoSource === 'screen';
  }

  /**
   * Starts publishing the screen in place of the camera.
   *
   * Must be called straight from a click: getDisplayMedia needs the transient
   * activation that gesture carries, and the picker never appears without it.
   */
  async startScreenShare(): Promise<void> {
    if (this.sharingScreen) return;
    this.#videoBeforeShare = this.media.useVideo;
    this.media.videoSource = 'screen';
    this.media.useVideo = true;
    try {
      await this.applyMedia();
    } catch (err) {
      // A cancelled picker rejects, and the settings must not be left claiming
      // to share a screen that was never granted.
      this.media.videoSource = 'camera';
      this.media.useVideo = this.#videoBeforeShare;
      await this.applyMedia().catch(() => {});
      throw err;
    }
  }

  /** Goes back to the camera, or to no video if that is where it started. */
  async stopScreenShare(): Promise<void> {
    this.media.videoSource = 'camera';
    this.media.useVideo = this.#videoBeforeShare;
    await this.applyMedia();
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

  /**
   * Releases whatever the preview opened.
   *
   * The welcome screen only shows a preview while its settings are expanded,
   * and holding the camera open behind a collapsed panel would leave the
   * recording light on for something nobody can see. join() reopens the devices
   * if they are needed.
   */
  closePreview(): void {
    capture.stop();
    this.#syncPreview();
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
   * Records a fault for the conference view to show.
   *
   * Repeats are collapsed to the one already at the end rather than stacked: a
   * decoder that fails on every frame would otherwise push the same sentence
   * twenty times and bury whatever else went wrong.
   */
  reportFault(detail: string): void {
    if (!detail || this.errors[this.errors.length - 1] === detail) return;
    this.errors = appendCapped(this.errors, detail, 20);
  }

  /** Clears the faults, once someone has read them. */
  dismissFaults(): void {
    this.errors = [];
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
    try {
      await capture.start(this.videoSettings, this.audioSettings);
    } catch (err) {
      // The phase is already "joined" by now, so the conference is on screen
      // and the welcome screen that would have shown this is gone. Without
      // saying so here, a capture that failed after the room was joined is a
      // call with an empty catalog and nothing anywhere to explain it.
      bridge.report('ERROR', 'capture failed to start after joining', { err: String(err) });
      this.reportFault('Could not start your camera or microphone. Others cannot see or hear you.');
      throw err;
    }
  }

  leave(): void {
    // A screen share belongs to the call and does not outlive it. Its track is
    // stopped along with everything else here, and leaving the source set to
    // 'screen' would have the welcome screen's own preview reach for
    // getDisplayMedia on mount — with no gesture behind it, which fails with
    // "must be called from a user gesture handler" and reads as the camera
    // having been refused.
    if (this.media.videoSource === 'screen') {
      this.media.videoSource = 'camera';
      this.media.useVideo = this.#videoBeforeShare;
    }

    // A fixed resolution is an override for the room it was chosen in — a call
    // that needed 360p because it had nine people in it should not hold the
    // next one to that. Every call is joined on Auto.
    this.media.resolution = RESOLUTION_AUTO;
    this.#autoApplied = '';

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

  /**
   * Opens the release page in the OS browser.
   *
   * Sent to the backend rather than opened here: this WebView has no notion of
   * a second window, so navigating to the page would replace the call with it.
   */
  openReleasePage(): void {
    if (!this.update) return;
    bridge.send({ type: 'openUrl', openUrl: this.update.url });
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
