/**
 * Shared metrics for the lucide icons used in the chrome.
 *
 * lucide takes the size per instance, and the context that would set it once
 * for the whole tree is internal to the package — not on its exports map — so
 * the value has to be passed at every call. It lives here so those calls agree
 * with each other rather than each carrying its own literal.
 */

/** Icon size beside a single line of UI text, in px. */
export const ICON_SIZE = 15;
