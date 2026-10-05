// State shared by the whole page: where the user is, how they like sizes
// shown, and the one-line message at the bottom of the screen.
import { signal } from './lib.js';

/** The screens. Those without `built` show a note about what will be there. */
export const SCREENS = [
  { id: 'overview', label: 'Overview', built: true },
  { id: 'space', label: 'Space', built: true },
  { id: 'find', label: 'Find', built: true },
  { id: 'review', label: 'Review', built: true },
  { id: 'annotations', label: 'Annotations', built: true },
  { id: 'storage', label: 'Storage', built: true },
  { id: 'settings', label: 'Settings', built: true },
];

/**
 * @typedef {{size: 'disk' | 'apparent', units: 'decimal' | 'binary', theme: 'system' | 'light' | 'dark', map: 'map' | 'icicle' | 'off'}} Prefs
 * @typedef {{screen: string, params: Record<string, string>}} Route
 */

// Preferences live in this browser until the server has somewhere to keep
// them. Storage can be unavailable (a private window); the defaults then
// simply apply each time.
/** @type {Prefs} */
const defaults = { size: 'disk', units: 'decimal', theme: 'system', map: 'map' };

function loadPrefs() {
  try {
    return { ...defaults, ...JSON.parse(localStorage.getItem('rp-prefs') || '{}') };
  } catch {
    return { ...defaults };
  }
}

export const prefs = signal(loadPrefs());

/**
 * @template {keyof Prefs} K
 * @param {K} key
 * @param {Prefs[K]} value
 */
export function setPref(key, value) {
  prefs.value = { ...prefs.value, [key]: value };
  try {
    localStorage.setItem('rp-prefs', JSON.stringify(prefs.value));
  } catch { /* not kept; see above */ }
  applyTheme();
}

export function applyTheme() {
  const theme = prefs.value.theme;
  if (theme === 'system') delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = theme;
}

/** @returns {Route} */
function readLocation() {
  const screen = location.pathname.replace(/^\/+/, '').split('/')[0] || 'overview';
  /** @type {Record<string, string>} */
  const params = {};
  new URLSearchParams(location.search).forEach((v, k) => { params[k] = v; });
  return { screen, params };
}

export const route = signal(readLocation());

/**
 * Goes to a screen. The address carries paths, never entry IDs, so that it
 * still means the same thing after the next scan.
 * @param {string} screen
 * @param {Record<string, string | undefined>} [params]
 * @param {boolean} [replace] correct the current address instead of adding to history
 */
export function go(screen, params = {}, replace = false) {
  const url = href(screen, params);
  if (replace) history.replaceState(null, '', url);
  else history.pushState(null, '', url);
  route.value = readLocation();
}

/** The address of a screen, as go() uses it and for a link's href. */
export function href(/** @type {string} */ screen, /** @type {Record<string, string | undefined>} */ params = {}) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v) q.set(k, v);
  const query = q.toString();
  return '/' + screen + (query ? '?' + query : '');
}

window.addEventListener('popstate', () => { route.value = readLocation(); });

/** The message at the bottom of the screen, announced to screen readers. */
export const toast = signal('');
let toastTimer = 0;

export function say(/** @type {string} */ text) {
  toast.value = text;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { toast.value = ''; }, 6000);
}
