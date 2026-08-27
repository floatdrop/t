import { describe, expect, test } from 'vitest';

import { buildInviteLink, normalizeRelay, parseInviteLink } from './invite';

/**
 * The mirror of TestParseInviteURL in invite_test.go. The Go parser and this
 * one are hand-written twins, so every case that pins the dialect there is
 * pinned here too — nothing else keeps them speaking the same language.
 */
describe('parseInviteLink', () => {
  test.each([
    ['bare host:port is the authority', 't://localhost:4433/linktest', 'localhost:4433', 'linktest'],
    ['remote relay', 't://relay.example.com:443/standup', 'relay.example.com:443', 'standup'],
    [
      'https relay overrides the authority',
      't://relay.example.com/r1?relay=https%3A%2F%2Frelay.example.com%2Flive',
      'https://relay.example.com/live',
      'r1',
    ],
    [
      'moqt relay overrides the authority',
      't://relay.example.com:4433/r2?relay=moqt%3A%2F%2Frelay.example.com%3A4433%2Fapp',
      'moqt://relay.example.com:4433/app',
      'r2',
    ],
    // The reported failure: an https relay's authority has no port to read,
    // because URL drops the default :443 when buildInviteLink derives it.
    [
      'port-less authority is an https relay',
      't://moq.tel.yandex.net/standup',
      'https://moq.tel.yandex.net/',
      'standup',
    ],
    [
      'port-less IPv6 authority keeps its brackets',
      't://[::1]/standup',
      'https://[::1]/',
      'standup',
    ],
    ['room is percent-decoded', 't://localhost:4433/room%20one', 'localhost:4433', 'room one'],
  ])('%s', (_name, url, relay, room) => {
    expect(parseInviteLink(url)).toEqual({ relay, room });
  });

  test.each([
    ['no room', 't://localhost:4433'],
    ['no room, trailing slash only', 't://localhost:4433/'],
    ['no relay', 't:///room'],
    ['wrong scheme', 'https://localhost:4433/room'],
    ['not a URL at all', 'localhost:4433'],
  ])('%s → null', (_name, url) => {
    expect(parseInviteLink(url)).toBeNull();
  });
});

describe('buildInviteLink round-trip', () => {
  test.each([
    ['bare host:port', 'localhost:4433'],
    ['https on the default port', 'https://moq.tel.yandex.net/'],
    ['https with a path', 'https://relay.example.com/live'],
    ['moqt with a port and a path', 'moqt://relay.example.com:4433/app'],
  ])('%s survives the link', (_name, relay) => {
    expect(parseInviteLink(buildInviteLink({ relay, room: 'standup' }))).toEqual({
      relay,
      room: 'standup',
    });
  });

  /**
   * The failure this test file was written for: a chat client that stops
   * linkifying an unknown scheme at the "?" delivers the authority alone. That
   * is lossy for a relay with a path, but the public relay — an https host on
   * the default port — has to survive it, because it is what every invite
   * sent to a fresh install carries.
   */
  test('a truncated link still resolves the default relay', () => {
    const relay = 'https://moq.tel.yandex.net/';
    const truncated = buildInviteLink({ relay, room: 'standup' }).split('?')[0];
    expect(truncated).toBe('t://moq.tel.yandex.net/standup');
    expect(parseInviteLink(truncated)).toEqual({ relay, room: 'standup' });
  });
});

/**
 * The repair path for installs that already stored a broken relay. A port-less
 * hostname could never be dialled, so nothing working is being rewritten.
 */
describe('normalizeRelay', () => {
  test.each([
    ['a port-less hostname becomes an https relay', 'moq.tel.yandex.net', 'https://moq.tel.yandex.net/'],
    ['a host:port is left alone', 'localhost:4433', 'localhost:4433'],
    ['an https URL is left alone', 'https://moq.tel.yandex.net/', 'https://moq.tel.yandex.net/'],
    ['an https URL with a path is left alone', 'https://relay.example.com/live', 'https://relay.example.com/live'],
    ['a moqt URL is left alone', 'moqt://relay.example.com:4433/app', 'moqt://relay.example.com:4433/app'],
    ['surrounding space is trimmed', '  moq.tel.yandex.net  ', 'https://moq.tel.yandex.net/'],
    ['empty stays empty', '', ''],
  ])('%s', (_name, stored, want) => {
    expect(normalizeRelay(stored)).toBe(want);
  });

  test('is idempotent', () => {
    expect(normalizeRelay(normalizeRelay('moq.tel.yandex.net'))).toBe('https://moq.tel.yandex.net/');
  });
});
