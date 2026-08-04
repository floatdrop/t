/**
 * Random nicknames, so the welcome screen arrives pre-filled and nobody
 * has to invent a name to join a test call.
 */

const ADJECTIVES = [
  'brisk', 'calm', 'clever', 'crimson', 'dapper', 'eager', 'fluent', 'gentle',
  'humble', 'jolly', 'keen', 'lucid', 'mellow', 'nimble', 'orbital', 'plucky',
  'quiet', 'rapid', 'silver', 'tidal', 'upbeat', 'vivid', 'witty', 'zesty',
];

const NOUNS = [
  'otter', 'falcon', 'heron', 'ibex', 'jackal', 'kestrel', 'lemur', 'marten',
  'newt', 'osprey', 'puffin', 'quokka', 'raven', 'shrike', 'tapir', 'urchin',
  'vixen', 'walrus', 'yak', 'zebu', 'badger', 'cormorant', 'dingo', 'egret',
];

function pick<T>(list: readonly T[]): T {
  const idx = crypto.getRandomValues(new Uint32Array(1))[0] % list.length;
  return list[idx];
}

/** Returns a nickname like "vivid-kestrel-417". */
export function randomNickname(): string {
  const suffix = crypto.getRandomValues(new Uint32Array(1))[0] % 900 + 100;
  return `${pick(ADJECTIVES)}-${pick(NOUNS)}-${suffix}`;
}

/** Returns a short room identifier like "k4m9x2". */
export function randomRoom(): string {
  const alphabet = 'abcdefghijkmnpqrstuvwxyz23456789';
  const raw = crypto.getRandomValues(new Uint8Array(6));
  return Array.from(raw, (b) => alphabet[b % alphabet.length]).join('');
}
