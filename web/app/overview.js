// Overview: how full the pool is, the shares with their size and how much of
// each is described, where the last review stopped, and the last scan.
import { api, indexId, superseded } from './api.js';
import { bytes, count, duration, moment, plural } from './format.js';
import { html, useEffect, useState } from './lib.js';
import { defFromParams, lastParams, sentence } from './queue.js';
import { scan, scanDialog } from './scan.js';
import { go, prefs } from './state.js';

/**
 * @typedef {import('./api.js').Entry & {annotated: number, skipped: number, orphaned: number, covered_bytes: number}} Share
 * @typedef {{name: string, mountpoint: string, fstype: string, total: number, used: number, avail: number,
 *   zfs?: {used_by_snapshots: number, compress_ratio: number}, snapshots: number}} Dataset
 */

/**
 * How full the scanned filesystems are. Datasets of one ZFS pool share
 * their free space, so the free figure is taken once and not added up.
 */
export function capacity(/** @type {Dataset[]} */ datasets) {
  const used = datasets.reduce((n, d) => n + d.used, 0);
  const snapshots = datasets.reduce((n, d) => n + (d.zfs ? d.zfs.used_by_snapshots : 0), 0);
  const zfs = datasets.some((d) => d.fstype === 'zfs');
  const free = zfs ? datasets.reduce((n, d) => Math.max(n, d.avail), 0) : datasets.reduce((n, d) => n + d.avail, 0);
  return { used, snapshots, free, total: used + snapshots + free };
}

export function Overview() {
  const mode = prefs.value.size;
  const index = indexId.value;
  const status = scan.value;
  const [shares, setShares] = useState(/** @type {Share[] | null} */ (null));
  const [datasets, setDatasets] = useState(/** @type {Dataset[] | null} */ (null));
  const [error, setError] = useState('');
  useEffect(() => {
    let live = true;
    api('GET', '/api/shares').then((r) => { if (live) setShares(r.shares); })
      .catch((e) => { if (live && !superseded(e)) setError(e instanceof Error ? e.message : String(e)); });
    api('GET', '/api/storage').then((r) => { if (live) setDatasets(r.datasets); }).catch(() => {});
    return () => { live = false; };
  }, [index]);

  if (error) return html`<div class="panel pad" role="alert"><b>The overview could not be loaded.</b> ${error}</div>`;
  if (!shares) return html`<p class="muted">Loading…</p>`;

  const sizeOf = (/** @type {Share} */ s) => (mode === 'disk' ? s.disk : s.size);
  const biggest = shares.reduce((n, s) => Math.max(n, sizeOf(s)), 0);
  const sorted = [...shares].sort((a, b) => sizeOf(b) - sizeOf(a));
  const cap = datasets && datasets.length ? capacity(datasets) : null;
  const info = status && status.index;
  const last = status && status.history && status.history[0];
  const queue = lastParams();
  const open = (/** @type {Share} */ s) => (/** @type {Event} */ e) => {
    e.preventDefault();
    go('space', { path: s.path });
  };

  return html`
    <div class="stack">
      <div class="sh"><h1>Overview</h1></div>
      ${cap && cap.total > 0 && html`
        <div class="panel pad">
          <div class="hero"><b>${bytes(cap.free)}</b><span>free of ${bytes(cap.total)}</span></div>
          <div class="capbar" role="img" aria-label=${`${bytes(cap.used)} used by files, ${bytes(cap.snapshots)} held by snapshots, ${bytes(cap.free)} free`}>
            <i class="k-plain" style=${{ flexGrow: cap.used }}></i>
            ${cap.snapshots > 0 && html`<i class="k-snap" style=${{ flexGrow: cap.snapshots }}></i>`}
            <i class="free" style=${{ flexGrow: cap.free }}></i>
          </div>
          <div class="legend">
            <span class="legend-i"><i class="sw k-plain"></i>Files, ${bytes(cap.used)}</span>
            ${cap.snapshots > 0 && html`<span class="legend-i"><i class="sw k-snap"></i>Held by snapshots, ${bytes(cap.snapshots)}</span>`}
            <span class="legend-i"><i class="sw k-rest"></i>Free, ${bytes(cap.free)}</span>
          </div>
        </div>`}

      <div class="panel">
        <div class="tw">
          <table class="list">
            <thead><tr><th>Share</th><th class="c-bar"></th><th class="num c-size">Size</th><th class="num c-items">Files</th>
              <th class="c-cov">Described</th></tr></thead>
            <tbody>
              ${sorted.map((s) => {
                const part = s.size > 0 ? Math.min(100, (100 * s.covered_bytes) / s.size) : 0;
                return html`
                  <tr class="pseudo" key=${s.id}>
                    <td><div class="nm"><a class="fs" href="/space" onClick=${open(s)}>${s.name}</a></div></td>
                    <td><div class="bar"><div class="fill" style=${{ width: `${biggest > 0 ? (100 * sizeOf(s)) / biggest : 0}%` }}><i class="k-plain" style=${{ flexGrow: 1 }}></i></div></div></td>
                    <td class="num">${bytes(sizeOf(s))}</td>
                    <td class="num">${count(s.files)}</td>
                    <td><div class="meter" title=${`${plural(s.annotated, 'item', 'items')} described, ${count(s.skipped)} skipped`}><i style=${{ width: `${part}%` }}></i></div>
                      <span class="hint">${part.toFixed(part < 10 ? 1 : 0)}% of its bytes</span></td>
                  </tr>`;
              })}
            </tbody>
          </table>
        </div>
      </div>

      <div class="cards">
        <div class="panel pad card">
          <h2 class="t">Review</h2>
          ${queue ? html`<p class="big">Carry on where you stopped</p><p>${sentence(defFromParams(queue))}.</p>`
            : html`<p class="big">Nothing reviewed yet</p><p>Go through the pool one item at a time and record what things are for.</p>`}
          <button class="btn primary" type="button" onClick=${() => go('review', queue || {})}>${queue ? 'Continue the review' : 'Start a review'}</button>
        </div>
        <div class="panel pad card">
          <h2 class="t">Last scan</h2>
          ${info ? html`
            <p class="big">${moment(new Date(info.finished))}</p>
            <div class="rows">
              <div><span>Files</span><span>${count(info.files)}</span></div>
              <div><span>Folders</span><span>${count(info.dirs)}</span></div>
              ${last && html`<div><span>Took</span><span>${duration(last.seconds)}, ${last.intensity === 'low' ? 'low impact' : last.intensity}</span></div>`}
              ${info.errors > 0 && html`<div><span>Could not be read</span><span>${count(info.errors)}</span></div>`}
              ${status && status.next_scheduled && html`<div><span>Next</span><span>${moment(new Date(status.next_scheduled))}</span></div>`}
            </div>` : html`<p>Nothing has been scanned yet.</p>`}
          <button class="btn" type="button" disabled=${!status || status.running} onClick=${() => { scanDialog.value = true; }}>Scan now</button>
        </div>
        <div class="panel pad card">
          <h2 class="t">Find</h2>
          <p class="big">What is taking the room</p>
          <p>The largest files and folders anywhere, and what has not been touched for years.</p>
          <div class="row">
            <button class="btn" type="button" onClick=${() => go('find', { kind: 'file' })}>Largest files</button>
            <button class="btn" type="button" onClick=${() => go('find', { kind: 'file', min: '1000000000', older: '2y' })}>Old and large</button>
          </div>
        </div>
      </div>
    </div>`;
}
