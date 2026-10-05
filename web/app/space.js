// Space: what is in a folder, largest first, with the selected item beside
// it. This is the sorted bar list of the design; the map above it comes later.
import { api, FLAG, indexId, superseded } from './api.js';
import { ago, bytes, count, date, plural } from './format.js';
import { Inspector, Marks, typeClass } from './inspector.js';
import { html, useEffect, useState } from './lib.js';
import { VirtualList } from './list.js';
import { FolderMap } from './map.js';
import { go, href, prefs, route, setPref } from './state.js';

/** @typedef {import('./api.js').Entry} Entry */

const FOLDER = html`<svg class="ico" width="14" height="14" viewBox="0 0 14 14" aria-hidden="true"><path d="M1.5 3.5h4l1.2 1.5h5.8v6.5h-11z" /></svg>`;
const FILE = html`<svg class="ico" width="14" height="14" viewBox="0 0 14 14" aria-hidden="true"><path d="M3.5 1.5h5l2 2v9h-7z" /></svg>`;

/** The folder that holds a path, or '' at a scan root. */
function parentOf(/** @type {string} */ path, /** @type {string[]} */ roots) {
  if (roots.includes(path)) return '';
  const cut = path.lastIndexOf('/');
  return cut <= 0 ? '/' : path.slice(0, cut);
}

/** The folders from the scan root down to a path, each a link. */
function Crumbs(/** @type {{path: string, roots: string[], keep: Record<string, string | undefined>}} */ { path, roots, keep }) {
  const root = roots.find((r) => path === r || path.startsWith(r === '/' ? '/' : r + '/')) || '';
  const rest = path.slice(root.length).split('/').filter(Boolean);
  const steps = [{ name: root, path: root }];
  for (const name of rest) {
    const prev = steps[steps.length - 1].path;
    steps.push({ name, path: (prev === '/' ? '' : prev) + '/' + name });
  }
  const link = (/** @type {{name: string, path: string}} */ s) => (/** @type {Event} */ e) => {
    e.preventDefault();
    go('space', { ...keep, path: s.path });
  };
  return html`
    <nav class="crumbs fs" aria-label="Folders above this one">
      ${roots.length > 1 && html`<a class="crumb" href=${href('space', keep)} onClick=${(/** @type {Event} */ e) => { e.preventDefault(); go('space', keep); }}>all</a><span class="sep">/</span>`}
      ${steps.map((s, i) => (i === steps.length - 1
        ? html`<span key=${s.path} aria-current="page">${s.name}</span>`
        : html`<a class="crumb" key=${s.path} href=${href('space', { ...keep, path: s.path })} onClick=${link(s)}>${s.name}</a><span class="sep">/</span>`))}
    </nav>`;
}

const COLUMNS = [
  { id: 'name', label: 'Name', cls: '', firstDesc: false },
  { id: 'bar', label: '', cls: 'c-bar' },
  { id: 'pct', label: 'Share', cls: 'c-pct num' },
  { id: 'size', label: 'Size', cls: 'c-size num', firstDesc: true },
  { id: 'files', label: 'Files', cls: 'c-items num', firstDesc: true },
  { id: 'modified', label: 'Last changed', cls: 'c-age', firstDesc: true },
];

export function Space() {
  const { params } = route.value;
  const path = params.path || '';
  const mode = prefs.value.size;
  const index = indexId.value;
  const sort = params.sort || 'size';
  const desc = params.desc ? params.desc === '1' : sort !== 'name';
  const keep = { sort: params.sort, desc: params.desc }; // the order chosen stays while walking between folders

  // What is on screen: the folder (null at the top, above the scan roots)
  // and the index it was read from. It is replaced only when the folder the
  // address names has arrived, so the last view stays up while the next loads.
  const [view, setView] = useState(/** @type {{index: string, roots: Entry[], folder: Entry | null} | null} */ (null));
  const [error, setError] = useState('');
  const [selected, setSelected] = useState(/** @type {Entry | null} */ (null));
  const [rev, setRev] = useState(0);

  // Find the folder the address names. Addresses hold paths, because entry
  // IDs are handed out afresh by every scan.
  useEffect(() => {
    let live = true;
    setError('');
    (async () => {
      try {
        const top = /** @type {{children: Entry[]}} */ (await api('GET', '/api/tree?sort=name&desc=0&limit=100'));
        if (!live) return;
        const roots = top.children;
        if (!path) {
          // With one scan root there is nothing to choose between: open it.
          if (roots.length === 1) go('space', { path: roots[0].name }, true);
          else {
            setSelected(null);
            setView({ index: indexId.value, roots, folder: null });
          }
          return;
        }
        const { entry } = /** @type {{entry: Entry}} */ (await api('GET', '/api/entries/lookup?path=' + encodeURIComponent(path)));
        if (!live) return;
        if (entry.kind !== 'dir') {
          // An address that names a file shows the folder it is in, with the file selected.
          go('space', { ...keep, path: parentOf(entry.path || path, roots.map((r) => r.name)) || undefined, sel: entry.name }, true);
        } else if (entry.path !== path) {
          go('space', { ...params, path: entry.path }, true); // the same folder, written the standard way
        } else {
          setSelected(null);
          setView({ index: indexId.value, roots, folder: entry });
        }
      } catch (e) {
        if (live && !superseded(e)) setError(e instanceof Error ? e.message : String(e));
      }
    })();
    return () => { live = false; };
  }, [path, index]);

  if (error) {
    return html`
      <div class="panel pad stack" role="alert">
        <p><b>This folder could not be opened.</b> ${error}</p>
        <p class="fs muted">${path}</p>
        <div><button class="btn" type="button" onClick=${() => go('space')}>Go to the top</button></div>
      </div>`;
  }
  // Entry IDs from an older index must not be used against the new one.
  if (!view || view.index !== index) return html`<p class="muted">Loading…</p>`;
  const { roots, folder } = view;

  const rootNames = roots.map((r) => r.name);
  const sizeOf = (/** @type {Entry} */ e) => (mode === 'disk' ? e.disk : e.size);
  // Bars show each row's share of the folder; at the top, of all the roots together.
  const whole = folder ? sizeOf(folder) : roots.reduce((n, r) => n + sizeOf(r), 0);
  const sortKey = sort === 'size' ? (mode === 'disk' ? 'disk' : 'size') : sort;

  const fetchPage = async (/** @type {number} */ offset, /** @type {number} */ limit) => {
    const q = `sort=${sortKey}&desc=${desc ? 1 : 0}&limit=${limit}&offset=${offset}` + (folder ? `&id=${folder.id}` : '');
    const t = /** @type {{children: Entry[], total: number}} */ (await api('GET', '/api/tree?' + q));
    return { rows: t.children, total: t.total };
  };

  // via is the folder between, when the map opens something one level further down.
  const open = (/** @type {Entry} */ e, /** @type {Entry} */ via) => {
    if (e.kind !== 'dir') return;
    const base = folder ? (folder.path === '/' ? '' : folder.path) + '/' : '';
    go('space', { ...keep, path: base + (via ? via.name + '/' : '') + e.name });
  };
  const up = () => {
    if (folder) go('space', { ...keep, path: parentOf(folder.path || path, rootNames) || undefined, sel: folder.name });
  };
  const sortBy = (/** @type {typeof COLUMNS[number]} */ col) => () => {
    const next = sort === col.id ? !desc : !!col.firstDesc;
    go('space', { path: (folder && folder.path) || undefined, sort: col.id, desc: next ? '1' : '0' }, true);
  };

  const head = COLUMNS.map((col) => html`
    <div key=${col.id} class=${col.cls} role="columnheader" aria-sort=${sort === col.id ? (desc ? 'descending' : 'ascending') : undefined}>
      ${col.firstDesc === undefined ? col.label
        : html`<button type="button" onClick=${sortBy(col)}>${col.label}${sort === col.id ? (desc ? ' ↓' : ' ↑') : ''}</button>`}
    </div>`);

  const renderRow = (/** @type {Entry} */ e) => {
    const size = sizeOf(e);
    const share = whole > 0 ? Math.min(100, (size / whole) * 100) : 0;
    const changed = e.kind === 'dir' && e.max_mtime ? e.max_mtime : e.mtime;
    // A hardlinked file counted at its other name still shows its own size, as an outline.
    const hollow = e.kind === 'file' && !e.counted;
    return html`
      <div class="nm" role="gridcell">${e.kind === 'dir' ? FOLDER : FILE}<span class="fs">${e.name}</span><${Marks} entry=${e} /></div>
      <div class="c-bar" role="gridcell"><div class="bar">
        <div class=${'fill' + (hollow ? ' hollow' : '')} style=${{ width: `${share.toFixed(2)}%` }}>
          ${!hollow && size > 0 && html`<i class=${typeClass(e)} style=${{ flexGrow: 1 }}></i>`}
        </div>
      </div></div>
      <div class="c-pct num" role="gridcell">${whole > 0 ? (share < 0.1 && size > 0 ? '<0.1%' : `${share.toFixed(share < 10 ? 1 : 0)}%`) : ''}</div>
      <div class="c-size num" role="gridcell">${e.flags & FLAG.excluded ? 'unknown' : bytes(size)}</div>
      <div class="c-items num" role="gridcell">${e.kind === 'dir' ? count(e.files) : ''}</div>
      <div class="c-age" role="gridcell" title=${date(changed)}>${ago(changed)}</div>`;
  };

  const shown = selected || folder;
  return html`
    <div class="stack">
      <div>
        ${folder ? html`<${Crumbs} path=${folder.path || path} roots=${rootNames} keep=${keep} />` : html`<div class="crumbs">Everything that is scanned</div>`}
        <div class="sh">
          <span class="sub">${folder
            ? `${bytes(sizeOf(folder))} ${mode === 'disk' ? 'on disk' : 'apparent'} in ${plural(folder.files, 'file', 'files')} and ${plural(folder.dirs, 'folder', 'folders')}`
            : `${bytes(whole)} ${mode === 'disk' ? 'on disk' : 'apparent'} in ${plural(roots.length, 'scanned folder', 'scanned folders')}`}</span>
        </div>
        <div class="ctls">
          <div class="ctl"><span class="ctl-l">Sizes</span>
            <div class="seg" role="group" aria-label="Which size to show">
              <button type="button" aria-pressed=${mode === 'disk' ? 'true' : 'false'} onClick=${() => setPref('size', 'disk')}>On disk</button>
              <button type="button" aria-pressed=${mode === 'apparent' ? 'true' : 'false'} onClick=${() => setPref('size', 'apparent')}>Apparent</button>
            </div>
          </div>
          ${folder && html`<div class="ctl"><span class="ctl-l">Map</span>
            <div class="seg" role="group" aria-label="How the map is drawn">
              ${[["map", "Areas"], ["icicle", "Bars"], ["off", "Off"]].map(([id, text]) => html`
                <button type="button" key=${id} aria-pressed=${prefs.value.map === id ? "true" : "false"} onClick=${() => setPref("map", /** @type {"map" | "icicle" | "off"} */ (id))}>${text}</button>`)}
            </div>
          </div>`}
          ${folder && html`<div class="ctl">
            <button class="btn small" type="button" onClick=${() => go("find", { under: folder.path })}>Find inside this folder</button>
            <button class="btn small" type="button" onClick=${() => go("review", { under: folder.path })}>Review what is inside</button>
          </div>`}
          <div class="legend" aria-label="What the bar colours mean">
            <span class="legend-i"><i class="sw k-video"></i>Video</span>
            <span class="legend-i"><i class="sw k-backup"></i>Archives and disk images</span>
            <span class="legend-i"><i class="sw k-image"></i>Images</span>
            <span class="legend-i"><i class="sw k-other"></i>Other files</span>
            <span class="legend-i"><i class="sw k-plain"></i>Folders</span>
          </div>
        </div>
      </div>
      ${folder && prefs.value.map !== "off" && html`
        <${FolderMap} folder=${folder} mode=${mode} form=${prefs.value.map} version=${`${index}|${rev}`}
          selectedId=${selected ? selected.id : 0} onSelect=${setSelected} onOpen=${open} />`}
      <div class="split">
        <div class="panel">
          <${VirtualList}
            listKey=${`${index}|${folder ? folder.id : 0}|${sortKey}|${desc}`} rev=${rev}
            fetchPage=${fetchPage} head=${head} renderRow=${renderRow}
            rowId=${(/** @type {Entry} */ e) => e.id} selectedId=${selected ? selected.id : 0}
            onSelect=${setSelected} onOpen=${open} onUp=${up}
            startAt=${params.sel ? (/** @type {Entry} */ e) => e.name === params.sel : undefined}
            label=${folder ? `Contents of ${folder.name}` : 'Scanned folders'}
            empty="This folder is empty." />
        </div>
        ${shown && html`<${Inspector} id=${shown.id} rev=${rev} onOpen=${open} onChanged=${() => setRev((n) => n + 1)} />`}
      </div>
    </div>`;
}
