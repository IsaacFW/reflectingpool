// A list that can hold a million rows: only the rows in view exist in the
// page, and they are fetched from the server a stretch at a time as the user
// scrolls. The server does the sorting.
import { superseded } from './api.js';
import { html, useEffect, useRef, useState } from './lib.js';

const ROW = 30; // px; matches .vl-row
const HEAD = 30; // px; matches .vl-head
const PAGE = 200; // rows per request
// Browsers stop laying out beyond some tens of millions of pixels. Past this
// height the scroll position stands for a fraction of the list instead.
const MAX_PX = 12_000_000;

/** The height limit in force. A test can lower it to reach, with a few thousand rows, what a real list only reaches with hundreds of thousands. */
function maxPx() {
  return /** @type {{rpListMaxPx?: number}} */ (globalThis).rpListMaxPx || MAX_PX;
}

/**
 * @template T
 * @typedef {object} ListProps
 * @property {string} listKey changes when a different list is shown; everything starts over
 * @property {number} rev changes when the same list should be read again, keeping the place
 * @property {(offset: number, limit: number) => Promise<{rows: T[], total: number}>} fetchPage
 * @property {unknown} head the header row's cells
 * @property {(row: T) => unknown} renderRow the cells of one row
 * @property {(row: T) => number} rowId
 * @property {number} selectedId
 * @property {(row: T) => void} onSelect
 * @property {(row: T) => void} onOpen Enter or a double click
 * @property {() => void} onUp Backspace or Alt+Up
 * @property {(row: T) => boolean} [startAt] the row to select when the list first loads, if it is near the top
 * @property {string} label what the list is, for screen readers
 * @property {number} [rowHeight] px; must match the height the stylesheet gives the rows
 * @property {string} [className] added to the list, for a different set of columns
 * @property {unknown} empty shown when the list has no rows
 */

/**
 * @template T
 * @param {ListProps<T>} props
 */
export function VirtualList(props) {
  const rowH = props.rowHeight || ROW;
  const box = useRef(/** @type {HTMLDivElement | null} */ (null));
  // What has been fetched. Kept outside state: pages arrive one by one and
  // the list is redrawn by hand when they do.
  const store = useRef({ key: '', rev: 0, total: -1, pages: /** @type {Map<number, T[]>} */ (new Map()), asked: /** @type {Set<number>} */ (new Set()) });
  const [, redraw] = useState(0);
  const [viewH, setViewH] = useState(600);
  // Where the user is in the list. It belongs to one list: kept with the
  // list's key, it starts afresh the moment another list is shown, with no
  // step afterwards that could undo something the user has just done.
  const fresh = { key: props.listKey, scrollTop: 0, cursor: -1, error: '' };
  const [place, setPlace] = useState(fresh);
  const { scrollTop, cursor, error } = place.key === props.listKey ? place : fresh;
  /** @param {Partial<typeof fresh> | ((now: typeof fresh) => Partial<typeof fresh>)} change */
  const move = (change) => setPlace((now) => {
    const from = now.key === props.listKey ? now : fresh;
    return { ...from, ...(typeof change === 'function' ? change(from) : change) };
  });
  const setCursor = (/** @type {number} */ i) => move({ cursor: i });
  const setError = (/** @type {string} */ text) => move({ error: text });

  const s = store.current;
  if (s.key !== props.listKey) {
    Object.assign(s, { key: props.listKey, rev: props.rev, total: -1 });
    s.pages = new Map();
    s.asked = new Set();
  } else if (s.rev !== props.rev) {
    s.rev = props.rev;
    s.pages = new Map();
    s.asked = new Set();
  }

  useEffect(() => {
    if (box.current) box.current.scrollTop = 0;
  }, [props.listKey]);

  useEffect(() => {
    const el = box.current;
    if (!el) return;
    const measure = () => setViewH(el.clientHeight);
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  function load(/** @type {number} */ page) {
    // A timer set by an earlier drawing can call this after the list has
    // moved on. Its fetchPage belongs to the old list, so it must not fill
    // the new one.
    const { listKey: key, rev } = props;
    if (s.key !== key || s.rev !== rev) return;
    if (s.pages.has(page) || s.asked.has(page)) return;
    s.asked.add(page);
    props.fetchPage(page * PAGE, PAGE).then(({ rows, total }) => {
      if (store.current.key !== key || store.current.rev !== rev) return; // an answer for a list no longer shown
      s.pages.set(page, rows);
      s.total = total;
      if (page === 0 && props.startAt) {
        const at = rows.findIndex(props.startAt);
        if (at >= 0) move((now) => (now.cursor < 0 ? { cursor: at } : {}));
      }
      redraw((n) => n + 1);
    }).catch((e) => {
      s.asked.delete(page);
      if (store.current.key !== key || superseded(e)) return;
      setError(e instanceof Error ? e.message : String(e));
    });
  }

  const total = s.total;
  const rowsInView = Math.max(1, Math.floor((viewH - HEAD) / rowH));
  const fullPx = Math.max(0, total) * rowH;
  const scaled = fullPx > maxPx();
  const bodyPx = scaled ? maxPx() : fullPx;
  const maxScroll = Math.max(1, bodyPx + HEAD - viewH);
  // The (fractional) index of the row at the top of the view.
  const top = scaled ? (scrollTop / maxScroll) * Math.max(0, total - rowsInView) : scrollTop / rowH;
  const first = Math.max(0, Math.floor(top) - 4);
  const last = Math.min(Math.max(0, total), Math.ceil(top) + rowsInView + 4);

  function loadView() {
    if (total < 0) load(0);
    else for (let p = Math.floor(first / PAGE); p <= Math.floor(Math.max(first, last - 1) / PAGE); p++) load(p);
  }
  // Fetch what is in view, once scrolling has paused for a moment.
  useEffect(() => {
    if (total < 0) {
      loadView();
      return;
    }
    const t = setTimeout(loadView, 50);
    return () => clearTimeout(t);
  }, [props.listKey, props.rev, total, first, last]);

  const rowAt = (/** @type {number} */ i) => {
    const page = s.pages.get(Math.floor(i / PAGE));
    return page ? page[i % PAGE] : undefined;
  };

  // The cursor can land on a row before it has arrived; select it when it does.
  const atCursor = cursor >= 0 ? rowAt(cursor) : undefined;
  useEffect(() => {
    if (atCursor && props.rowId(atCursor) !== props.selectedId) props.onSelect(atCursor);
  }, [atCursor && props.rowId(atCursor)]);

  function moveTo(/** @type {number} */ i) {
    if (total <= 0) return;
    const next = Math.min(total - 1, Math.max(0, i));
    setCursor(next);
    const el = box.current;
    if (!el) return;
    if (scaled) {
      if (next < Math.ceil(top) || next >= Math.floor(top) + rowsInView) {
        el.scrollTop = (next / Math.max(1, total - rowsInView)) * maxScroll;
      }
    } else if (next * rowH < el.scrollTop) {
      el.scrollTop = next * rowH;
    } else if ((next + 1) * rowH > el.scrollTop + viewH - HEAD) {
      el.scrollTop = (next + 1) * rowH - (viewH - HEAD);
    }
  }

  function onKey(/** @type {KeyboardEvent} */ e) {
    const step = { ArrowDown: 1, ArrowUp: -1, PageDown: rowsInView, PageUp: -rowsInView }[e.key];
    if (e.key === 'Backspace' || (e.key === 'ArrowUp' && e.altKey)) {
      props.onUp();
    } else if (step) {
      moveTo(cursor < 0 ? 0 : cursor + step);
    } else if (e.key === 'Home') {
      moveTo(0);
    } else if (e.key === 'End') {
      moveTo(total - 1);
    } else if (e.key === 'Enter' && atCursor) {
      props.onOpen(atCursor);
    } else {
      return;
    }
    e.preventDefault();
  }

  const rows = [];
  for (let i = first; i < last; i++) {
    const row = rowAt(i);
    const y = `${scaled ? scrollTop + (i - top) * rowH : i * rowH}px`;
    if (!row) {
      rows.push(html`<div class="vl-row wait" key=${'w' + i} style=${{ top: y }} role="row" aria-rowindex=${i + 2}><span>…</span></div>`);
      continue;
    }
    const selected = props.rowId(row) === props.selectedId;
    rows.push(html`
      <div class=${'vl-row' + (selected ? ' sel' : '')} key=${props.rowId(row)} style=${{ top: y }}
        role="row" aria-rowindex=${i + 2} aria-selected=${selected ? 'true' : 'false'}
        onClick=${() => { setCursor(i); props.onSelect(row); }}
        onDblClick=${() => props.onOpen(row)}>
        ${props.renderRow(row)}
      </div>`);
  }

  return html`
    <div class=${"vl" + (props.className ? " " + props.className : "")} ref=${box} tabindex="0" role="grid" aria-label=${props.label} aria-rowcount=${Math.max(0, total) + 1}
      onScroll=${(/** @type {Event} */ e) => move({ scrollTop: /** @type {HTMLElement} */ (e.currentTarget).scrollTop })} onKeyDown=${onKey}>
      <div class="vl-head" role="row" aria-rowindex="1">${props.head}</div>
      ${error ? html`
        <div class="vl-empty" role="alert">Could not load this list: ${error}
          <button class="btn small" type="button" onClick=${() => { setError(''); loadView(); }}>Retry</button>
        </div>`
      : total < 0 ? html`<div class="vl-empty">Loading…</div>`
      : total === 0 ? html`<div class="vl-empty">${props.empty}</div>`
      : html`<div class="vl-body" style=${{ height: `${bodyPx}px` }}>${rows}</div>`}
    </div>`;
}
