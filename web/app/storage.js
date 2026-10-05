// Storage: the filesystems behind what is scanned. On ZFS this is where the
// figures that belong to a dataset and not to a folder are shown: space held
// by snapshots, compression, and whether read times are recorded.
import { api, superseded } from './api.js';
import { bytes, count, moment } from './format.js';
import { html, useEffect, useState } from './lib.js';
import { capacity } from './overview.js';

/**
 * @typedef {{name: string, type: string, used: number, avail: number, refer: number, used_by_snapshots: number,
 *   used_by_dataset: number, used_by_children: number, compress_ratio: number, atime: string, relatime: string}} ZFS
 * @typedef {{name: string, mountpoint: string, fstype: string, total: number, used: number, avail: number,
 *   objects: number, zfs?: ZFS, snapshots: number}} Dataset
 * @typedef {{datasets: Dataset[], zfs_origin: string, zfs_as_of?: string, zfs_error?: string}} Report
 */

/** Which times a dataset keeps, in words. Staleness can only use the ones that are kept. */
function clocks(/** @type {ZFS | undefined} */ z) {
  if (!z || !z.atime) return '';
  if (z.atime === 'off') return 'Read times are not recorded';
  return z.relatime === 'on' ? 'Read times recorded about once a day' : 'Read times recorded';
}

export function Storage() {
  const [rep, setRep] = useState(/** @type {Report | null} */ (null));
  const [error, setError] = useState('');
  useEffect(() => {
    let live = true;
    api('GET', '/api/storage').then((r) => { if (live) setRep(r); })
      .catch((e) => { if (live && !superseded(e)) setError(e instanceof Error ? e.message : String(e)); });
    return () => { live = false; };
  }, []);

  if (error) return html`<div class="panel pad" role="alert"><b>The storage figures could not be loaded.</b> ${error}</div>`;
  if (!rep) return html`<p class="muted">Loading…</p>`;
  const cap = capacity(rep.datasets);
  const whole = (/** @type {Dataset} */ d) => d.used + (d.zfs ? d.zfs.used_by_snapshots : 0);
  const largest = rep.datasets.reduce((n, d) => Math.max(n, whole(d)), 0);
  const anyZFS = rep.datasets.some((d) => d.fstype === 'zfs');
  // Whether read times are kept is worth a column only where datasets differ.
  const kept = [...new Set(rep.datasets.map((d) => clocks(d.zfs)).filter(Boolean))];
  const clockColumn = kept.length > 1;

  return html`
    <div class="stack">
      <div class="sh"><h1>Storage</h1>
        <span class="sub">${bytes(cap.free)} free of ${bytes(cap.total)} across ${count(rep.datasets.length)} ${rep.datasets.length === 1 ? 'filesystem' : 'filesystems'}</span></div>

      ${rep.zfs_origin === 'zfs' && html`<p class="muted">Snapshot and compression figures come straight from ZFS, read through <span class="fs">/dev/zfs</span>.</p>`}
      ${rep.zfs_origin === 'file' && html`<p class="muted">Snapshot and compression figures come from the file the host script writes${rep.zfs_as_of ? `, last written ${moment(new Date(rep.zfs_as_of))}` : ''}.</p>`}
      ${rep.zfs_origin === 'none' && anyZFS && html`
        <div class="banner"><div><span class="st"><i aria-hidden="true">i</i>No snapshot or compression figures.</span>
          <span> Sizes below are what each filesystem reports. To add the ZFS figures, either pass <span class="fs">/dev/zfs</span> into the container
            (it is only read; an unprivileged container cannot change the pool through it), or run the host script from the README on a schedule.</span></div></div>`}
      ${rep.zfs_error && html`
        <div class="banner warn" role="status"><div><span class="st"><i aria-hidden="true">!</i>ZFS could not be asked.</span><span> ${rep.zfs_error}</span></div></div>`}

      <div class="panel">
        <div class="tw">
          <table class="list">
            <thead><tr>
              <th>Dataset</th><th class="c-bar"></th><th class="num c-size">Used</th><th class="num c-size">By files</th>
              <th class="num c-snap">By snapshots</th><th class="num c-items">Snapshots</th><th class="num c-pct">Ratio</th>${clockColumn && html`<th class="c-note">Read times</th>`}
            </tr></thead>
            <tbody>
              ${rep.datasets.map((d) => {
                const snap = d.zfs ? d.zfs.used_by_snapshots : 0;
                return html`
                  <tr class="pseudo" key=${d.mountpoint}>
                    <td><div class="nm"><span class="fs">${d.name}</span>${d.fstype !== 'zfs' && html`<span class="mark">${d.fstype}</span>`}</div>
                      <div class="where fs">${d.mountpoint}</div></td>
                    <td><div class="bar"><div class="fill" style=${{ width: `${largest > 0 ? (100 * whole(d)) / largest : 0}%` }}>
                      <i class="k-plain" style=${{ flexGrow: Math.max(1, d.used) }}></i>
                      ${snap > 0 && html`<i class="k-snap" style=${{ flexGrow: snap }}></i>`}</div></div></td>
                    <td class="num">${bytes(whole(d))}</td>
                    <td class="num">${bytes(d.zfs ? d.zfs.used_by_dataset : d.used)}</td>
                    <td class="num">${d.zfs ? bytes(snap) : ''}</td>
                    <td class="num">${d.zfs ? count(d.snapshots) : ''}</td>
                    <td class="num" title="How much smaller compression makes the data">${d.zfs && d.zfs.compress_ratio ? `${d.zfs.compress_ratio.toFixed(2)}×` : ''}</td>
                    ${clockColumn && html`<td class="c-note">${clocks(d.zfs)}</td>`}
                  </tr>`;
              })}
            </tbody>
          </table>
        </div>
      </div>
      ${kept.length === 1 && html`<p class="hint">${kept[0]} on ${rep.datasets.length === 1 ? "this dataset" : "any of these datasets"}${kept[0].includes("not") ? ", so nothing here can tell when a file was last opened." : "."}</p>`}
      <p class="hint">Space held by snapshots belongs to a dataset, not to a folder, so it cannot be browsed in Space.
        Removing a file frees its space only once the snapshots that still hold it have expired.</p>
    </div>`;
}
