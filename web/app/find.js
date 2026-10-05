// Find: everything in the index that matches a set of filters, in one table,
// with saved views for the questions that come up again and again.
import { api, indexId, superseded } from './api.js';
import { prefixList, loadPrefixes } from './annotate.js';
import { ago, bytes, count, date, plural } from './format.js';
import { Inspector, Marks, Parts } from './inspector.js';
import { html, useEffect, useRef, useState } from './lib.js';
import { VirtualList } from './list.js';
import { go, prefs, route } from './state.js';

/** @typedef {import('./api.js').Entry} Entry */

const TYPES = ['video', 'audio', 'image', 'document', 'archive', 'disk-image', 'text', 'subtitle', 'other'];
const SIZES = [['', 'any size'], ['1000000', '1 MB'], ['100000000', '100 MB'], ['1000000000', '1 GB'], ['10000000000', '10 GB']];
const AGES = [['', 'any time'], ['6m', '6 months'], ['1y', '1 year'], ['2y', '2 years'], ['5y', '5 years']];
const STATES = [['', 'any'], ['annotated', 'described'], ['unannotated', 'not described'], ['skipped', 'skipped']];

/**
 * The views that are always there. Later finders arrive as more of these.
 * @type {{name: string, params: Record<string, string>}[]}
 */
const VIEWS = [
  { name: 'Largest files', params: { kind: 'file' } },
  { name: 'Largest folders', params: { kind: 'dir' } },
  { name: 'Old and large', params: { kind: 'file', min: '1000000000', older: '2y' } },
  { name: 'Not described yet', params: { kind: 'file', state: 'unannotated' } },
];

/** The time before which "not changed for 2y" means, in Unix seconds. */
function before(/** @type {string} */ age) {
  const m = /^(\d+)([my])$/.exec(age);
  if (!m) return 0;
  const d = new Date();
  if (m[2] === 'm') d.setMonth(d.getMonth() - Number(m[1]));
  else d.setFullYear(d.getFullYear() - Number(m[1]));
  return Math.floor(d.getTime() / 1000);
}

// Views the user saved, kept in this browser.
function savedViews() {
  try {
    return /** @type {{name: string, params: Record<string, string>}[]} */ (JSON.parse(localStorage.getItem('rp-views') || '[]'));
  } catch {
    return [];
  }
}
function storeViews(/** @type {{name: string, params: Record<string, string>}[]} */ views) {
  try {
    localStorage.setItem('rp-views', JSON.stringify(views));
  } catch { /* not kept */ }
}

/** The filters that decide what is listed, without what only arranges it. */
function filterOf(/** @type {Record<string, string>} */ p) {
  const { sort, desc, sel, ...rest } = p;
  return rest;
}

export function Find() {
  const { params } = route.value;
  const index = indexId.value;
  const mode = prefs.value.size;
  // While something is searched for, the nearest matches come first unless a column is chosen.
  const sort = params.sort || (params.q ? 'match' : 'size');
  const desc = params.desc ? params.desc === '1' : sort !== 'name';
  const [shares, setShares] = useState(/** @type {{id: number, name: string}[]} */ ([]));
  const [scope, setScope] = useState(/** @type {{index: string, key: string, query: string} | null} */ (null));
  const [error, setError] = useState('');
  const [totals, setTotals] = useState(/** @type {{total: number, size: number | null} | null} */ (null));
  const [selected, setSelected] = useState(/** @type {Entry | null} */ (null));
  const [rev, setRev] = useState(0);
  const [text, setText] = useState(params.q || '');
  const [views, setViews] = useState(savedViews);
  const [naming, setNaming] = useState(/** @type {string | null} */ (null));
  const largest = useRef(0);

  const set = (/** @type {Record<string, string | undefined>} */ change) => go('find', { ...params, sel: undefined, ...change }, true);
  const filter = filterOf(params);
  const key = JSON.stringify(filter);

  useEffect(() => { if (!prefixList.value) loadPrefixes(); }, []);
  // The box follows the address when a view is picked or Back is pressed.
  useEffect(() => { setText(params.q || ''); }, [params.q]);
  // A name search across a million entries takes a moment, so it waits for
  // a pause in the typing. Enter runs it at once.
  useEffect(() => {
    if (text === (params.q || '')) return;
    const t = setTimeout(() => set({ q: text || undefined }), 300);
    return () => clearTimeout(t);
  }, [text]);

  // Names and paths in the address become the entry IDs of the index in use.
  useEffect(() => {
    let live = true;
    setError('');
    (async () => {
      try {
        const list = (await api('GET', '/api/shares')).shares;
        if (!live) return;
        setShares(list);
        const q = new URLSearchParams();
        if (params.kind) q.set('kind', params.kind);
        if (params.shares) {
          q.set('share', params.shares.split(',').map((name) => {
            const s = list.find((/** @type {{name: string}} */ x) => x.name === name);
            if (!s) throw new Error(`There is no share called ${name}.`);
            return s.id;
          }).join(','));
        }
        if (params.under) q.set('under', (await api('GET', '/api/entries/lookup?path=' + encodeURIComponent(params.under))).entry.id);
        if (params.types) q.set('type', params.types);
        if (params.min) q.set('min_size', params.min);
        if (params.older) q.set('modified_before', String(before(params.older)));
        if (params.state) q.set('state', params.state);
        if (params.prefix) q.set('prefix', params.prefix);
        if (params.q) q.set('name', params.q);
        if (!live) return;
        largest.current = 0;
        setSelected(null);
        setTotals(null);
        setScope({ index: indexId.value, key, query: q.toString() });
      } catch (e) {
        if (live && !superseded(e)) setError(e instanceof Error ? e.message : String(e));
      }
    })();
    return () => { live = false; };
  }, [key, index]);

  const sizeOf = (/** @type {Entry} */ e) => (mode === 'disk' ? e.disk : e.size);
  const sortKey = sort === 'size' ? (mode === 'disk' ? 'disk' : 'size') : sort;
  const ready = scope && scope.index === index && scope.key === key ? scope : null;

  const fetchPage = async (/** @type {number} */ offset, /** @type {number} */ limit) => {
    const r = await api('GET', `/api/entries?${ready ? ready.query : ''}&sort=${sortKey}&desc=${desc ? 1 : 0}&limit=${limit}&offset=${offset}`);
    if (offset === 0) {
      largest.current = r.items.reduce((/** @type {number} */ n, /** @type {Entry} */ e) => Math.max(n, sizeOf(e)), 0);
      setTotals({ total: r.total, size: mode === 'disk' ? r.total_disk : r.total_size });
    }
    return { rows: r.items, total: r.total };
  };

  const parent = (/** @type {Entry} */ e) => (e.path || '').slice(0, Math.max(1, (e.path || '').lastIndexOf('/')));
  const show = (/** @type {Entry} */ e) => go('space', { path: parent(e), sel: e.name });
  const open = (/** @type {Entry} */ e) => (e.kind === 'dir' ? go('space', { path: e.path }) : show(e));
  const sortBy = (/** @type {string} */ id, /** @type {boolean} */ firstDesc) => () =>
    go('find', { ...params, sort: id, desc: (sort === id ? !desc : firstDesc) ? '1' : '0' }, true);
  const head = [['name', 'Name', '', false], ['', '', 'c-bar', false], ['size', 'Size', 'c-size num', true], ['', 'Type', 'c-type', false], ['modified', 'Last changed', 'c-age', true]]
    .map(([id, label, cls, firstDesc]) => html`
      <div key=${label || 'bar'} class=${cls} role="columnheader" aria-sort=${id && sort === id ? (desc ? 'descending' : 'ascending') : undefined}>
        ${id ? html`<button type="button" onClick=${sortBy(String(id), !!firstDesc)}>${label}${sort === id ? (desc ? ' ↓' : ' ↑') : ''}</button>` : label}
      </div>`);
  const renderRow = (/** @type {Entry} */ e) => {
    const size = sizeOf(e);
    const changed = e.kind === 'dir' && e.max_mtime ? e.max_mtime : e.mtime;
    return html`
      <div role="gridcell">
        <div class="nm"><span class="fs">${e.name}${e.kind === 'dir' ? '/' : ''}</span><${Marks} entry=${e} /></div>
        <div class="where fs">${parent(e)}</div>
      </div>
      <div class="c-bar" role="gridcell"><div class="bar">
        <div class="fill" style=${{ width: `${largest.current > 0 ? Math.min(100, (100 * size) / largest.current).toFixed(2) : 0}%` }}>
          ${size > 0 && html`<${Parts} entry=${e} mode=${mode} />`}</div>
      </div></div>
      <div class="c-size num" role="gridcell">${bytes(size)}</div>
      <div class="c-type" role="gridcell">${e.kind === 'dir' ? 'folder' : e.type}</div>
      <div class="c-age" role="gridcell" title=${date(changed)}>${ago(changed)}</div>`;
  };

  const select = (/** @type {string} */ name, /** @type {string[][]} */ options, /** @type {string} */ label) => html`
    <label>${label}
      <select value=${params[name] || ''} onChange=${(/** @type {Event} */ e) => set({ [name]: /** @type {HTMLSelectElement} */ (e.target).value || undefined })}>
        ${options.map(([v, text]) => html`<option key=${v} value=${v}>${text}</option>`)}
      </select></label>`;
  const isView = (/** @type {Record<string, string>} */ p) => JSON.stringify(p) === key;
  const reviewParams = () => ({
    kind: params.kind === 'dir' ? 'dir' : params.kind === 'file' ? undefined : 'any', shares: params.shares, types: params.types,
    under: params.under, min: params.min, older: params.older ? String(before(params.older)) : undefined, q: params.q,
  });
  const saveView = () => {
    if (!naming || !naming.trim()) return;
    const next = [...views.filter((v) => v.name !== naming.trim()), { name: naming.trim(), params: filter }];
    storeViews(next);
    setViews(next);
    setNaming(null);
  };
  const dropView = (/** @type {string} */ name) => {
    const next = views.filter((v) => v.name !== name);
    storeViews(next);
    setViews(next);
  };
  const known = prefixList.value || [];
  const filtered = Object.keys(filter).length > 0;

  return html`
    <div class="stack">
      <div class="sh"><h1>Find</h1></div>
      <div>
        <div class="presets" role="group" aria-label="Views">
          ${VIEWS.map((v) => html`<button class="chipb" type="button" key=${v.name} aria-pressed=${isView(v.params) ? 'true' : 'false'} onClick=${() => go('find', v.params)}>${v.name}</button>`)}
          ${views.map((v) => html`
            <span class="scope" key=${v.name}><button type="button" onClick=${() => go('find', v.params)}>${v.name}</button>
              <button type="button" aria-label=${'Forget the view ' + v.name} onClick=${() => dropView(v.name)}>×</button></span>`)}
          ${naming === null
            ? html`<button class="chipb plan" type="button" disabled=${!filtered} onClick=${() => setNaming('')}>Save this view</button>`
            : html`<form class="row" onSubmit=${(/** @type {Event} */ e) => { e.preventDefault(); saveView(); }}>
                <input type="text" aria-label="Name for this view" placeholder="Name for this view" value=${naming}
                  onInput=${(/** @type {Event} */ e) => setNaming(/** @type {HTMLInputElement} */ (e.target).value)} />
                <button class="btn small" type="submit">Save</button>
                <button class="btn small" type="button" onClick=${() => setNaming(null)}>Cancel</button></form>`}
        </div>
        <div class="filters">
          ${select('kind', [['', 'files and folders'], ['file', 'files'], ['dir', 'folders']], 'Kind')}
          ${select('shares', [['', 'all shares'], ...shares.map((s) => [s.name, s.name])], 'Share')}
          ${select('types', [['', 'all types'], ...TYPES.map((t) => [t, t])], 'Type')}
          ${select('min', SIZES, 'At least')}
          ${select('older', AGES, 'Not changed for')}
          ${select('state', STATES, 'Recorded')}
          ${known.length > 0 && select('prefix', [['', 'any'], ...known.map((p) => [p.name, p.name])], 'Prefix')}
          <label class="grow">Words in the name
            <input type="search" value=${text} onInput=${(/** @type {Event} */ e) => setText(/** @type {HTMLInputElement} */ (e.target).value)}
              onKeyDown=${(/** @type {KeyboardEvent} */ e) => { if (e.key === 'Enter') set({ q: text || undefined }); }} /></label>
        </div>
        ${params.under && html`<span class="scope">Inside <span class="fs">${params.under}</span>
          <button type="button" aria-label="Search everywhere instead" onClick=${() => set({ under: undefined })}>×</button></span>`}
      </div>
      ${error ? html`<div class="panel pad" role="alert"><b>This search could not be run.</b> ${error}</div>` : html`
        <div class=${"split" + (selected ? "" : " one")}>
          <div class="panel">
            <div class="sum">
              <span role="status">${totals
                ? `${plural(totals.total, 'item', 'items')}${totals.size !== null && totals.size !== undefined ? `, ${bytes(totals.size)} ${mode === 'disk' ? 'on disk' : 'apparent'}` : ''}`
                : 'Searching…'}</span>
              <span class="row">
                ${params.q && html`<button class="chipb" type="button" aria-pressed=${sort === "match" ? "true" : "false"} title="Whole words first, then the start of a word, then anywhere in a name"
                  onClick=${() => go("find", { ...params, sort: undefined, desc: undefined }, true)}>Best match first</button>`}
                ${filtered && html`<button class="btn small" type="button" onClick=${() => go('find')}>Clear the filters</button>`}
                <button class="btn small" type="button" disabled=${!totals || totals.total === 0} onClick=${() => go('review', reviewParams())}>Review these results</button>
              </span>
            </div>
            ${ready && html`
              <${VirtualList} className="tall find" rowHeight=${44}
                listKey=${`${index}|${key}|${sortKey}|${desc}`} rev=${rev}
                fetchPage=${fetchPage} head=${head} renderRow=${renderRow}
                rowId=${(/** @type {Entry} */ e) => e.id} selectedId=${selected ? selected.id : 0}
                onSelect=${setSelected} onOpen=${open} onUp=${() => {}}
                label="Search results"
                empty=${filtered ? 'Nothing matches these filters.' : 'The index is empty.'} />`}
          </div>
          ${selected && html`<${Inspector} id=${selected.id} rev=${rev} onOpen=${open} onShow=${show} onChanged=${() => setRev((n) => n + 1)} />`}
        </div>`}
    </div>`;
}
