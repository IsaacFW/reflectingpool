// Every request to the server goes through api(). It adds the CSRF token,
// says which index the page's entry IDs came from, and notices when the
// session has ended or a scan has replaced the index.
import { signal } from './lib.js';

/**
 * @typedef {{user: string, csrf: string, read_only: boolean, version: string, roots: string[]}} Session
 * @typedef {{id: number, parent: number, name: string, path?: string, kind: string, flags: number,
 *   size: number, disk: number, mtime: number, btime: number, atime: number, nlink: number, share: number,
 *   counted: boolean, ext: string, type: string, state: number, covered: boolean, files: number, dirs: number,
 *   max_mtime: number, max_btime: number, max_atime: number}} Entry
 * @typedef {{path: string, kind: string, note?: string, display_name?: string, prefixes?: string[],
 *   owner?: string, review_after?: string, skipped?: boolean, orphaned?: boolean, updated?: string}} Annotation
 */

/** The session: undefined until the server has been asked, null when signed out. */
export const session = signal(/** @type {Session | null | undefined} */ (undefined));

/** The index in use. Entry IDs mean nothing outside the index that issued them, so anything showing entries reloads when this changes. */
export const indexId = signal('');

/** Entry flags and annotation states, as the index stores them. */
export const FLAG = { mount: 1, excluded: 2, error: 4 };
export const STATE = { note: 1, prefix: 2, skipped: 4, displayName: 8 };

export class ApiError extends Error {
  /**
   * @param {number} status
   * @param {string} code a stable word to act on; `message` is for a person
   * @param {string} message
   * @param {number} [retryAfter] seconds, when the server said how long to wait
   */
  constructor(status, code, message, retryAfter) {
    super(message);
    this.status = status;
    this.code = code;
    this.retryAfter = retryAfter || 0;
  }
}

/**
 * @param {string} method
 * @param {string} path
 * @param {unknown} [body] sent as JSON
 * @param {{signal?: AbortSignal}} [opts]
 * @returns {Promise<any>}
 */
export async function api(method, path, body, opts = {}) {
  /** @type {Record<string, string>} */
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const s = session.value;
  if (method !== 'GET' && s) headers['X-CSRF-Token'] = s.csrf;
  if (indexId.value) headers['X-RP-Index'] = indexId.value;

  let res;
  try {
    res = await fetch(path, {
      method, headers, signal: opts.signal, credentials: 'same-origin',
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (e) {
    if (e instanceof DOMException && e.name === 'AbortError') throw e;
    throw new ApiError(0, 'network', 'The server could not be reached.');
  }
  const current = res.headers.get('X-RP-Index');
  if (current && current !== indexId.value) indexId.value = current;

  const text = await res.text();
  /** @type {any} */
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { data = null; }
  }
  if (!res.ok) {
    if (res.status === 401 && path !== '/api/login') session.value = null;
    throw new ApiError(res.status, (data && data.code) || 'error',
      (data && data.error) || `The server answered ${res.status}.`, Number(res.headers.get('Retry-After')) || 0);
  }
  return data;
}

/** The address of a file's contents, for an image, a player or a frame. */
export function contentURL(/** @type {number} */ id) {
  return `/api/entries/${id}/content`;
}

/** Whether an error only says that a scan replaced the index. Whatever was being loaded is loaded again, so there is nothing to report. */
export function superseded(/** @type {unknown} */ e) {
  return (e instanceof ApiError && e.code === 'index_changed') || (e instanceof DOMException && e.name === 'AbortError');
}

/** The shares nothing can be recorded in, by entry ID, with their names. A pool mapped read-only puts every share here. */
export const lockedShares = signal(/** @type {Map<number, string>} */ (new Map()));

/** Asks which shares can be written to. Called when the index changes, since share IDs change with it. */
export function loadShareAccess() {
  return api('GET', '/api/shares').then((r) => {
    lockedShares.value = new Map(r.shares.filter((/** @type {{writable: boolean}} */ s) => !s.writable).map((/** @type {{id: number, name: string}} */ s) => [s.id, s.name]));
  }).catch(() => {});
}
