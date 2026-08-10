/**
 * Invite links — the shareable form of "join me in this room".
 *
 * A link looks like `t://localhost:4433/room-id`: the relay is the
 * authority and the room the path. `t` is registered as a URL scheme (see
 * CFBundleURLTypes in build/darwin/Info.plist), so these are clickable — macOS
 * launches or focuses the app and hands it the URL. The welcome screen also
 * accepts one pasted into its relay or room field, which covers the case where
 * a chat client will not linkify an unknown scheme.
 *
 * A link, rather than two values to copy separately, because a relay address
 * and a room have to travel together to be worth anything.
 */

const SCHEME = 't:';

export interface Invite {
  relay: string;
  room: string;
}

/**
 * Builds the link that Copy invite puts on the clipboard.
 *
 * The relay is the authority and the room the path — `t://host:port/room`
 * — so the link reads as an address rather than a bag of parameters.
 *
 * A relay that is more than a bare `host:port` (a `moqt://` or `https://` URL,
 * possibly with a path) cannot be expressed by an authority alone. Those keep
 * the readable authority for display and carry the exact value in a `relay`
 * query parameter, which the parser prefers when present.
 */
export function buildInviteLink({ relay, room }: Invite): string {
  const path = encodeURIComponent(room);
  if (isBareHostPort(relay)) {
    return `${SCHEME}//${relay}/${path}`;
  }
  const params = new URLSearchParams({ relay });
  return `${SCHEME}//${relayAuthority(relay)}/${path}?${params.toString()}`;
}

/** True for a relay written as `host:port`, with no scheme and no path. */
function isBareHostPort(relay: string): boolean {
  return !relay.includes('://') && !/[/?#\s]/.test(relay);
}

/** The host:port of a relay URL, for the readable part of the link. */
function relayAuthority(relay: string): string {
  try {
    return new URL(relay).host || relay;
  } catch {
    return relay;
  }
}

/**
 * Extracts an invite from pasted text, or null when it holds none.
 *
 * Tolerant on purpose: text arrives from chat clients that wrap, truncate or
 * decorate what they were given, so the link is located within the paste
 * rather than required to be the whole of it.
 */
export function parseInviteLink(text: string): Invite | null {
  // Word-boundary anchored: a one-letter scheme would otherwise match the tail
  // of any word ending in "t" that happens to precede "://".
  const match = text.match(/\bt:\/\/[^\s<>"']+/i);
  if (!match) return null;
  try {
    const url = new URL(match[0]);
    // An explicit relay parameter wins: it is only ever present when the
    // authority alone could not express the relay.
    const relay = (url.searchParams.get('relay') || url.host).trim();
    const room = decodeURIComponent(url.pathname).replace(/^\/+/, '').trim();
    if (!relay || !room) return null;
    return { relay, room };
  } catch {
    return null;
  }
}

/**
 * Copies text to the clipboard, reporting whether it worked.
 *
 * The async Clipboard API is tried first and a hidden-textarea
 * `execCommand('copy')` is the fallback: the modern call needs a permission
 * the embedded WebView does not always grant, and a copy button that silently
 * does nothing is worse than a deprecated code path.
 */
export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Fall through to the legacy path.
  }

  const scratch = document.createElement('textarea');
  scratch.value = text;
  // Off-screen rather than hidden: a display:none element cannot be selected.
  scratch.setAttribute('readonly', '');
  scratch.style.position = 'fixed';
  scratch.style.top = '-1000px';
  scratch.style.opacity = '0';
  document.body.appendChild(scratch);
  try {
    scratch.select();
    return document.execCommand('copy');
  } catch {
    return false;
  } finally {
    scratch.remove();
  }
}
