// A review queue as the page describes it: what to go through, how it is
// grouped, and in what order. It lives in the address, so a review can be
// bookmarked and survives a reload, and it names shares and folders by name
// and path because entry IDs change with every scan.
import { api } from './api.js';

/**
 * @typedef {object} QueueDef
 * @property {'file' | 'dir' | ''} kind files, folders, or both
 * @property {string[]} shares names; none means every share
 * @property {string[]} types file types; none means all
 * @property {string} under a folder's path: only what is inside it
 * @property {number} min smallest size in bytes
 * @property {number} older only items not changed since this time (Unix seconds)
 * @property {string} q part of a name
 * @property {string[]} groups share, type, ext, folder, age: outermost first
 * @property {'size' | 'age' | 'newest' | 'name'} order
 * @property {boolean} covered leave out what is inside described folders
 */

/** @type {QueueDef} */
export const DEFAULT = { kind: 'file', shares: [], types: [], under: '', min: 0, older: 0, q: '', groups: ['share', 'type'], order: 'size', covered: true };

export const GROUP_KEYS = [['share', 'Share'], ['type', 'File type'], ['ext', 'Extension'], ['folder', 'Folder'], ['age', 'Age']];
export const ORDERS = [['size', 'Largest first'], ['age', 'Oldest first'], ['newest', 'Newest first'], ['name', 'By name']];

/** @type {{name: string, def: Partial<QueueDef>}[]} */
export const PRESETS = [
  { name: 'Largest files by type, share by share', def: { kind: 'file', groups: ['share', 'type'], order: 'size' } },
  { name: 'Folders first, largest first', def: { kind: 'dir', groups: ['share'], order: 'size' } },
  { name: 'Biggest files anywhere', def: { kind: 'file', groups: [], order: 'size' } },
  { name: 'Oldest first, share by share', def: { kind: 'file', groups: ['share'], order: 'age' } },
  { name: 'One folder at a time', def: { kind: 'file', groups: ['folder'], order: 'size' } },
];

const list = (/** @type {string | undefined} */ s) => (s ? s.split(',').filter(Boolean) : []);

/** @returns {QueueDef} */
export function defFromParams(/** @type {Record<string, string>} */ p) {
  const kind = p.kind === 'dir' ? 'dir' : p.kind === 'any' ? '' : 'file';
  const order = /** @type {QueueDef['order']} */ (ORDERS.some(([id]) => id === p.order) ? p.order : 'size');
  return {
    kind, shares: list(p.shares), types: list(p.types), under: p.under || '', min: Number(p.min) || 0,
    older: Number(p.older) || 0, q: p.q || '',
    groups: p.groups === undefined ? DEFAULT.groups : list(p.groups).filter((g) => GROUP_KEYS.some(([id]) => id === g)),
    order, covered: p.covered !== '0',
  };
}

/** @returns {Record<string, string | undefined>} */
export function paramsFromDef(/** @type {QueueDef} */ d) {
  return {
    kind: d.kind === 'dir' ? 'dir' : d.kind === '' ? 'any' : undefined,
    shares: d.shares.join(',') || undefined, types: d.types.join(',') || undefined,
    under: d.under || undefined, min: d.min ? String(d.min) : undefined, older: d.older ? String(d.older) : undefined,
    q: d.q || undefined, groups: d.groups.join(',') || 'none', order: d.order === 'size' ? undefined : d.order,
    covered: d.covered ? undefined : '0',
  };
}

/** The queue in a sentence: "Files in media, by Share then File type, largest first". */
export function sentence(/** @type {QueueDef} */ d) {
  const what = d.kind === 'dir' ? 'Folders' : d.kind === '' ? 'Files and folders' : 'Files';
  const where = d.under ? `inside ${d.under}` : d.shares.length ? `in ${d.shares.join(', ')}` : 'in all shares';
  const label = (/** @type {string} */ id) => (GROUP_KEYS.find(([k]) => k === id) || ['', id])[1];
  const by = d.groups.length ? `by ${d.groups.map(label).join(' then ')}` : 'as one list';
  const order = (ORDERS.find(([id]) => id === d.order) || ORDERS[0])[1].toLowerCase();
  return `${what} ${where}, ${by}, ${order}`;
}

/**
 * Turns a definition into what POST /api/queue takes: names and paths become
 * the entry IDs of the index in use.
 * @returns {Promise<Record<string, unknown>>}
 */
export async function resolve(/** @type {QueueDef} */ d) {
  /** @type {Record<string, unknown>} */
  const filter = { kind: d.kind };
  if (d.shares.length) {
    const { shares } = await api('GET', '/api/shares');
    filter.shares = d.shares.map((name) => {
      const s = shares.find((/** @type {{name: string}} */ x) => x.name === name);
      if (!s) throw new Error(`There is no share called ${name}.`);
      return s.id;
    });
  }
  if (d.under) filter.under = (await api('GET', '/api/entries/lookup?path=' + encodeURIComponent(d.under))).entry.id;
  if (d.types.length) filter.types = d.types;
  if (d.min) filter.min_size = d.min;
  if (d.older) filter.modified_before = d.older;
  if (d.q) filter.name = d.q;
  return {
    filter, groups: d.groups, exclude_covered: d.covered,
    order: d.order === 'newest' ? 'age' : d.order, desc: d.order === 'size' || d.order === 'newest',
  };
}

// The last queue used is remembered in this browser, for "continue review".
export function lastParams() {
  try {
    return /** @type {Record<string, string> | null} */ (JSON.parse(localStorage.getItem('rp-last-queue') || 'null'));
  } catch {
    return null;
  }
}

export function rememberParams(/** @type {Record<string, string>} */ p) {
  try {
    localStorage.setItem('rp-last-queue', JSON.stringify(p));
  } catch { /* not kept */ }
}

/** A group's name as shown: "media, video". */
export function groupLabel(/** @type {{values: {key: string, label: string}[]}} */ g) {
  if (!g.values.length) return 'Everything';
  return g.values.map((v) => (v.key === 'ext' && !v.label ? 'no extension' : v.label)).join(', ');
}

/**
 * A group in words, so that nobody has to work out what "media, video"
 * stands for: "video files in media".
 * @param {{values: {key: string, label: string}[]}} g
 * @param {QueueDef['kind']} kind
 */
export function groupPhrase(g, kind) {
  /** @type {Record<string, string>} */
  const by = Object.fromEntries(g.values.map((v) => [v.key, v.label]));
  const noun = kind === 'dir' ? 'folders' : kind === '' ? 'items' : 'files';
  let what = noun;
  if (by.type !== undefined) what = by.type === 'folder' ? 'folders' : `${by.type} ${noun}`;
  else if (by.ext !== undefined) what = by.ext ? `.${by.ext} ${noun}` : `${noun} with no extension`;
  if (by.age !== undefined) what += `, ${by.age.toLowerCase()} old,`;
  if (by.folder !== undefined) what += ` in ${by.folder}`;
  else if (by.share !== undefined) what += ` in ${by.share}`;
  return what.replace(/,$/, '');
}
