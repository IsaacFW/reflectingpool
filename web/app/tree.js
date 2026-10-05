// The folder tree: a panel beside the list in Space for getting around
// without going down one folder at a time. It holds folders only, loads a
// level when it is opened, and keeps the way to the current folder open.
import { api, indexId, superseded } from './api.js';
import { bytes, count } from './format.js';
import { html, useEffect, useRef, useState } from './lib.js';

/** @typedef {import('./api.js').Entry} Entry */

const LEVEL = 200; // folders fetched for one level, largest first

/** The folders above a path and the path itself, outermost first, starting at its scan root. */
function wayTo(/** @type {string} */ path, /** @type {string[]} */ roots) {
  const root = roots.find((r) => path === r || path.startsWith(r === '/' ? '/' : r + '/'));
  if (!root) return [];
  const out = [root];
  let at = root === '/' ? '' : root;
  for (const name of path.slice(root.length).split('/').filter(Boolean)) {
    at += '/' + name;
    out.push(at);
  }
  return out;
}

/**
 * @param {{current: string, roots: string[], mode: 'disk' | 'apparent', onGo: (path: string) => void, onClose: () => void}} props
 */
export function FolderTree({ current, roots, mode, onGo, onClose }) {
  const index = indexId.value;
  // What has been fetched: for a folder's path, the folders inside it. The
  // key '' holds the scan roots.
  const [levels, setLevels] = useState(/** @type {Map<string, {index: string, rows: Entry[], total: number}>} */ (new Map()));
  const [open, setOpen] = useState(/** @type {Set<string>} */ (new Set()));
  const asked = useRef(/** @type {Set<string>} */ (new Set()));
  const sizeOf = (/** @type {Entry} */ e) => (mode === 'disk' ? e.disk : e.size);
  const sort = mode === 'disk' ? 'disk' : 'size';

  const level = (/** @type {string} */ path) => {
    const l = levels.get(path);
    return l && l.index === index ? l : undefined;
  };

  /** Fetches the folders inside one folder. `id` 0 is the top. */
  function load(/** @type {string} */ path, /** @type {number} */ id) {
    const key = index + '|' + path;
    if (asked.current.has(key)) return;
    asked.current.add(key);
    api('GET', `/api/tree?kind=dir&sort=${sort}&desc=1&limit=${LEVEL}` + (id ? `&id=${id}` : ''))
      .then((r) => {
        const rows = r.children.filter((/** @type {Entry} */ c) => c.name !== '.reflection'); // ours, not somewhere to go
        setLevels((m) => new Map(m).set(path, { index, rows, total: r.total - (r.children.length - rows.length) }));
      })
      .catch((e) => { if (!superseded(e)) asked.current.delete(key); });
  }

  // The top, and again whenever a scan replaces the index or the sizes change.
  useEffect(() => {
    asked.current = new Set();
    setLevels(new Map());
    load('', 0);
  }, [index, mode]);

  // Keep the way to the current folder open, a level at a time: each level
  // can only be asked for once the one above has said what its ID is.
  const way = wayTo(current, roots);
  useEffect(() => {
    if (!way.length) return;
    setOpen((o) => {
      const next = new Set(o);
      for (const p of way) next.add(p);
      return next.size === o.size ? o : next;
    });
  }, [current]);
  useEffect(() => {
    let parent = '';
    for (const p of way) {
      const l = level(parent);
      if (!l) return;
      const row = l.rows.find((r) => (parent ? (parent === '/' ? '' : parent) + '/' + r.name : r.name) === p);
      if (!row) return; // further down this level than was fetched
      if (!level(p)) load(p, row.id);
      parent = p;
    }
  }, [current, levels]);

  const toggle = (/** @type {string} */ path, /** @type {Entry} */ row) => {
    const next = new Set(open);
    if (next.has(path)) next.delete(path);
    else {
      next.add(path);
      load(path, row.id);
    }
    setOpen(next);
  };

  /** One level of the tree, drawn inside its parent. */
  const branch = (/** @type {string} */ parent) => {
    const l = level(parent);
    if (!l) return html`<div class="tree-note">Loading…</div>`;
    if (!l.rows.length) return null;
    return html`
      <ul role="group">
        ${l.rows.map((r) => {
          const path = parent ? (parent === '/' ? '' : parent) + '/' + r.name : r.name;
          const isOpen = open.has(path);
          return html`
            <li key=${r.id} role="treeitem" aria-expanded=${r.dirs > 0 ? (isOpen ? 'true' : 'false') : undefined} aria-current=${path === current ? 'page' : undefined}>
              <div class=${'tree-row' + (path === current ? ' cur' : '')}>
                ${r.dirs > 0
                  ? html`<button class="tree-tw" type="button" aria-label=${(isOpen ? 'Close ' : 'Open ') + r.name} onClick=${() => toggle(path, r)}>${isOpen ? '−' : '+'}</button>`
                  : html`<span class="tree-tw"></span>`}
                <button class="tree-name fs" type="button" title=${path} onClick=${() => onGo(path)}>${r.name}</button>
                <span class="tree-size">${bytes(sizeOf(r))}</span>
              </div>
              ${isOpen && r.dirs > 0 && branch(path)}
            </li>`;
        })}
        ${l.total > l.rows.length && html`<li class="tree-note">and ${count(l.total - l.rows.length)} smaller folders; open the folder to see them</li>`}
      </ul>`;
  };

  return html`
    <nav class="panel tree" aria-label="Folders">
      <div class="tree-head"><h2 class="t">Folders</h2>
        <button class="btn small" type="button" onClick=${onClose}>Hide</button></div>
      <div role="tree">${branch('')}</div>
    </nav>`;
}
