// The map above the list in Space: the folder's contents as areas, with the
// contents of its larger folders drawn inside them. The list says exactly
// what is biggest here; the map shows when something large sits a level down.
import { api, superseded } from './api.js';
import { bytes, count } from './format.js';
import { typeClass } from './inspector.js';
import { html, useEffect, useRef, useState } from './lib.js';

/** @typedef {import('./api.js').Entry} Entry */
/** @typedef {{x: number, y: number, w: number, h: number}} Rect */

const OUTER = 40; // children drawn; the rest become one block
const INNER = 12; // children drawn inside a folder
const HEAD = 18; // px: the strip with a folder's name above its contents

/**
 * A squarified treemap (Bruls, Huizing, van Wijk): areas in proportion to
 * the sizes, kept as near to square as the order allows. Sizes must be
 * positive and largest first.
 * @returns {Rect[]}
 */
export function squarify(/** @type {number[]} */ sizes, /** @type {Rect} */ box) {
  let { x, y, w, h } = box;
  const total = sizes.reduce((a, b) => a + b, 0);
  /** @type {Rect[]} */
  const out = [];
  if (total <= 0 || w <= 0 || h <= 0) return sizes.map(() => ({ x, y, w: 0, h: 0 }));
  const scale = (w * h) / total;
  for (let i = 0; i < sizes.length;) {
    const short = Math.min(w, h);
    // Add to the row while that keeps its worst shape from getting worse.
    let end = i;
    let sum = 0;
    let best = Infinity;
    let max = 0;
    let min = Infinity;
    for (let j = i; j < sizes.length; j++) {
      const area = sizes[j] * scale;
      const s = sum + area;
      const hi = Math.max(max, area);
      const lo = Math.min(min, area);
      const worst = Math.max((short * short * hi) / (s * s), (s * s) / (short * short * lo));
      if (worst > best) break;
      [best, sum, max, min, end] = [worst, s, hi, lo, j + 1];
    }
    const thick = sum / short;
    let along = 0;
    for (let k = i; k < end; k++) {
      const len = (sizes[k] * scale) / thick;
      out.push(w >= h ? { x, y: y + along, w: thick, h: len } : { x: x + along, y, w: len, h: thick });
      along += len;
    }
    if (w >= h) {
      x += thick;
      w -= thick;
    } else {
      y += thick;
      h -= thick;
    }
    i = end;
  }
  return out;
}

/** An icicle: the items side by side, each as wide as its share. */
function strip(/** @type {number[]} */ sizes, /** @type {Rect} */ box) {
  const total = sizes.reduce((a, b) => a + b, 0);
  let x = box.x;
  return sizes.map((s) => {
    const w = total > 0 ? (box.w * s) / total : 0;
    const r = { x, y: box.y, w, h: box.h };
    x += w;
    return r;
  });
}

const px = (/** @type {Rect} */ r, /** @type {number} */ gap = 1) =>
  ({ left: `${r.x}px`, top: `${r.y}px`, width: `${Math.max(0, r.w - gap)}px`, height: `${Math.max(0, r.h - gap)}px` });

/**
 * @param {{folder: Entry, mode: 'disk' | 'apparent', form: 'map' | 'icicle', version: string,
 *   selectedId: number, onSelect: (e: Entry) => void, onOpen: (e: Entry, via?: Entry) => void}} props
 * onOpen is given the folder between, when what is opened sits one level further down.
 */
export function FolderMap({ folder, mode, form, version, selectedId, onSelect, onOpen }) {
  const box = useRef(/** @type {HTMLDivElement | null} */ (null));
  const [width, setWidth] = useState(0);
  const [data, setData] = useState(/** @type {{key: string, kids: Entry[], total: number, inner: Map<number, {kids: Entry[], total: number}>} | null} */ (null));
  const sizeOf = (/** @type {Entry} */ e) => (mode === 'disk' ? e.disk : e.size);
  const sort = mode === 'disk' ? 'disk' : 'size';
  const key = `${version}|${folder.id}|${mode}`;

  useEffect(() => {
    const el = box.current;
    if (!el) return;
    const measure = () => setWidth(el.clientWidth);
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const top = await api('GET', `/api/tree?id=${folder.id}&sort=${sort}&desc=1&limit=${OUTER}`);
        const kids = /** @type {Entry[]} */ (top.children).filter((k) => sizeOf(k) > 0);
        // One more request for each folder large enough to show what is in it.
        const whole = sizeOf(folder) || 1;
        const deep = kids.filter((k) => k.kind === 'dir' && sizeOf(k) / whole >= 0.04).slice(0, 12);
        const inner = new Map();
        await Promise.all(deep.map(async (k) => {
          const r = await api('GET', `/api/tree?id=${k.id}&sort=${sort}&desc=1&limit=${INNER}`);
          inner.set(k.id, { kids: /** @type {Entry[]} */ (r.children).filter((c) => sizeOf(c) > 0), total: r.total });
        }));
        if (live) setData({ key, kids, total: top.total, inner });
      } catch (e) {
        if (live && !superseded(e)) setData({ key, kids: [], total: 0, inner: new Map() });
      }
    })();
    return () => { live = false; };
  }, [key]);

  const height = form === 'icicle' ? 96 : 300;
  const cells = [];
  let note = '';
  if (data && data.key === key && width > 0) {
    const whole = sizeOf(folder);
    const shown = data.kids.reduce((n, k) => n + sizeOf(k), 0);
    // What the drawn children leave over: the many small ones, as one block.
    const rest = Math.max(0, whole - shown);
    const sizes = data.kids.map(sizeOf);
    if (rest > 0 && data.total > data.kids.length) sizes.push(rest);
    const outerBox = form === 'icicle' ? { x: 0, y: 0, w: width, h: 44 } : { x: 0, y: 0, w: width, h: height };
    const rects = form === 'icicle' ? strip(sizes, outerBox) : squarify(sizes, outerBox);
    if (!data.kids.length || (data.kids[0] && sizeOf(data.kids[0]) / (whole || 1) < 0.005)) {
      note = data.total > 0
        ? `Nothing here is large enough to draw: ${count(data.total)} items averaging ${bytes(whole / data.total)}.`
        : 'This folder is empty.';
    } else {
      const label = (/** @type {Entry} */ e, /** @type {Rect} */ r) => (r.w > 54 && r.h > 16
        ? html`<b class="fs">${e.name}</b>${r.h > 32 && html`<span>${bytes(sizeOf(e))}</span>`}` : null);
      const tip = (/** @type {Entry} */ e) => `${e.name}\n${bytes(sizeOf(e))}, ${whole > 0 ? ((100 * sizeOf(e)) / whole).toFixed(1) : 0}% of this folder`;
      data.kids.forEach((k, i) => {
        const r = rects[i];
        if (!r || r.w < 2 || r.h < 2) return;
        const within = data.inner.get(k.id);
        const sel = k.id === selectedId ? ' sel' : '';
        if (!within || !within.kids.length || (form === 'map' && (r.w < 70 || r.h < 50))) {
          cells.push(html`<div key=${k.id} class=${`cell ${typeClass(k)}${sel}`} style=${px(r)} title=${tip(k)}
            onClick=${() => onSelect(k)} onDblClick=${() => onOpen(k)}>${label(k, r)}</div>`);
          return;
        }
        // A folder with its contents inside: a strip with its name, then the contents.
        const inSizes = within.kids.map(sizeOf);
        const left = Math.max(0, sizeOf(k) - inSizes.reduce((a, b) => a + b, 0));
        if (left > 0 && within.total > within.kids.length) inSizes.push(left);
        const inBox = form === 'icicle'
          ? { x: r.x, y: 46, w: r.w - 1, h: height - 46 }
          : { x: r.x + 2, y: r.y + HEAD, w: r.w - 5, h: r.h - HEAD - 3 };
        const inRects = form === 'icicle' ? strip(inSizes, inBox) : squarify(inSizes, inBox);
        cells.push(html`<div key=${k.id} class="grp" style=${px(r)}></div>`);
        cells.push(html`<div key=${'h' + k.id} class=${`grp-h fs${sel}`} style=${{ left: `${r.x}px`, top: `${r.y}px`, width: `${Math.max(0, r.w - 1)}px`, height: `${form === 'icicle' ? 44 : HEAD}px`, right: 'auto' }}
          title=${tip(k)} onClick=${() => onSelect(k)} onDblClick=${() => onOpen(k)}>${r.w > 40 ? `${k.name}  ${bytes(sizeOf(k))}` : ''}</div>`);
        within.kids.forEach((c, j) => {
          const cr = inRects[j];
          if (!cr || cr.w < 2 || cr.h < 2) return;
          cells.push(html`<div key=${c.id} class=${`cell ${typeClass(c)}${c.id === selectedId ? ' sel' : ''}`} style=${px(cr)} title=${`${k.name}/${c.name}\n${bytes(sizeOf(c))}`}
            onClick=${() => onSelect(c)} onDblClick=${() => (c.kind === 'dir' ? onOpen(c, k) : onOpen(k))}>${label(c, cr)}</div>`);
        });
        const lr = inRects[within.kids.length];
        if (lr && lr.w >= 2 && lr.h >= 2) {
          cells.push(html`<div key=${'r' + k.id} class="cell k-rest" style=${px(lr)} title=${`${count(within.total - within.kids.length)} smaller items in ${k.name}`}></div>`);
        }
      });
      const rr = rects[data.kids.length];
      if (rr && rr.w >= 2 && rr.h >= 2) {
        const n = data.total - data.kids.length;
        cells.push(html`<div key="rest" class="cell k-rest" style=${px(rr)} title=${`${count(n)} smaller items, ${bytes(rest)}`}>
          ${rr.w > 90 && rr.h > 16 && html`<b>${count(n)} smaller items</b>`}</div>`);
      }
    }
  }

  // The list beside it is the same data as a table; the map is a picture of it.
  return html`
    <div class="map" ref=${box} role="img" style=${{ height: `${height}px` }}
      aria-label=${`A map of what is in ${folder.name}: areas in proportion to size. The list below holds the same figures.`}>
      ${cells}
      ${note && html`<div class="map-empty">${note}</div>`}
      ${!data && html`<div class="map-empty">Drawing the map…</div>`}
    </div>`;
}
