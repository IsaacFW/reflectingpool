// Annotations: how much of each share is described, the list of prefixes,
// and the notes whose item could not be found at the last scan.
import { api, indexId, session, superseded } from './api.js';
import { loadPrefixes, prefixList } from './annotate.js';
import { bytes, count, plural } from './format.js';
import { html, useEffect, useState } from './lib.js';
import { go, say } from './state.js';

/**
 * @typedef {import('./api.js').Entry & {annotated: number, skipped: number, orphaned: number, covered_bytes: number, error?: string}} Share
 * @typedef {{name: string, meaning: string, count?: number}} Prefix
 */

function Coverage(/** @type {{shares: Share[]}} */ { shares }) {
  return html`
    <div class="panel">
      <div class="tw">
        <table class="list">
          <thead><tr>
            <th>Share</th><th class="num c-size">Size</th><th class="num c-items">Described</th><th class="num c-items">Skipped</th>
            <th class="num c-items">Missing</th><th class="c-cov">Inside described items</th><th class="c-note"></th>
          </tr></thead>
          <tbody>
            ${shares.map((s) => {
              const part = s.size > 0 ? Math.min(100, (100 * s.covered_bytes) / s.size) : 0;
              return html`
                <tr class="pseudo" key=${s.id}>
                  <td><div class="nm"><span class="fs">${s.name}</span></div>
                    ${s.annotated > 0 && html`<div class="where fs" title="A readable summary of this share's notes">${s.path}/.reflection/INDEX.md</div>`}
                    ${s.error && html`<div class="err">${s.error}</div>`}</td>
                  <td class="num">${bytes(s.size)}</td>
                  <td class="num">${count(s.annotated)}</td>
                  <td class="num">${count(s.skipped)}</td>
                  <td class="num">${count(s.orphaned)}</td>
                  <td><div class="meter" title=${`${bytes(s.covered_bytes)} of ${bytes(s.size)}`}><i style=${{ width: `${part}%` }}></i></div>
                    <span class="hint">${part.toFixed(part < 10 ? 1 : 0)}% of its bytes</span></td>
                  <td><button class="btn small" type="button" onClick=${() => go('review', { shares: s.name })}>Review this share</button></td>
                </tr>`;
            })}
          </tbody>
        </table>
      </div>
    </div>`;
}

function Prefixes() {
  const readOnly = !!(session.value && session.value.read_only);
  const saved = prefixList.value;
  const [rows, setRows] = useState(/** @type {Prefix[] | null} */ (null));
  const [error, setError] = useState('');
  useEffect(() => { loadPrefixes(); }, [indexId.value]);
  // The table starts from what is saved, and again after each save.
  useEffect(() => { if (saved) setRows(saved.map((p) => ({ ...p }))); }, [saved]);
  if (!rows) return null;

  const edit = (/** @type {number} */ i, /** @type {'name' | 'meaning'} */ k) => (/** @type {Event} */ e) =>
    setRows(rows.map((r, j) => (j === i ? { ...r, [k]: /** @type {HTMLInputElement} */ (e.target).value } : r)));
  const move = (/** @type {number} */ i, /** @type {number} */ by) => {
    const next = [...rows];
    [next[i], next[i + by]] = [next[i + by], next[i]];
    setRows(next);
  };
  async function save(/** @type {Event} */ e) {
    e.preventDefault();
    setError('');
    try {
      await api('PUT', '/api/prefixes', { prefixes: (rows || []).filter((r) => r.name.trim()).map((r) => ({ name: r.name.trim(), meaning: r.meaning.trim() })) });
      await loadPrefixes();
      say('The prefix list is saved.');
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }
  const changed = JSON.stringify(rows.map((r) => [r.name, r.meaning])) !== JSON.stringify((saved || []).map((r) => [r.name, r.meaning]));

  return html`
    <form class="panel pad stack" onSubmit=${save}>
      <h2 class="t">Prefixes</h2>
      <p class="muted">Short labels you can put on any item, such as KEEP or ARCHIVE. File names are never changed.</p>
      ${rows.length > 0 && html`
        <table class="list">
          <thead><tr><th class="c-type">Prefix</th><th>What it means</th><th class="num c-items">Items</th><th class="c-age"></th></tr></thead>
          <tbody>
            ${rows.map((r, i) => html`
              <tr class="pseudo" key=${i}>
                <td><input type="text" aria-label="Prefix" maxlength="32" disabled=${readOnly} value=${r.name} onInput=${edit(i, 'name')} /></td>
                <td><input type="text" aria-label=${'What ' + (r.name || 'it') + ' means'} maxlength="500" disabled=${readOnly} value=${r.meaning} onInput=${edit(i, 'meaning')} /></td>
                <td class="num">${r.count === undefined ? '' : count(r.count)}</td>
                <td><div class="row">
                  <button class="btn small" type="button" aria-label=${'Move ' + r.name + ' up'} disabled=${readOnly || i === 0} onClick=${() => move(i, -1)}>Up</button>
                  <button class="btn small" type="button" aria-label=${'Move ' + r.name + ' down'} disabled=${readOnly || i === rows.length - 1} onClick=${() => move(i, 1)}>Down</button>
                  <button class="btn small" type="button" aria-label=${'Remove ' + r.name} disabled=${readOnly} onClick=${() => setRows(rows.filter((_, j) => j !== i))}>Remove</button>
                </div></td>
              </tr>`)}
          </tbody>
        </table>`}
      <p class="hint">Removing a prefix from this list leaves it on the items that already carry it. A prefix cannot be renamed yet: add the new one and remove the old.</p>
      ${error && html`<p class="err" role="alert">${error}</p>`}
      <div class="row">
        <button class="btn" type="button" disabled=${readOnly} onClick=${() => setRows([...rows, { name: '', meaning: '' }])}>Add a prefix</button>
        <button class="btn primary" type="submit" disabled=${readOnly || !changed}>Save the list</button>
      </div>
    </form>`;
}

/** Notes whose item was not found at the last scan. */
function Missing(/** @type {{shares: Share[], onChange: () => void}} */ { shares, onChange }) {
  const readOnly = !!(session.value && session.value.read_only);
  const withMissing = shares.filter((s) => s.orphaned > 0);
  const [notes, setNotes] = useState(/** @type {{share: Share, path: string, note?: string, prefixes?: string[]}[] | null} */ (null));
  useEffect(() => {
    let live = true;
    Promise.all(withMissing.map((s) => api('GET', `/api/shares/${s.id}/annotations`)
      .then((r) => r.annotations.filter((/** @type {{orphaned?: boolean}} */ a) => a.orphaned).map((/** @type {object} */ a) => ({ ...a, share: s })))))
      .then((lists) => { if (live) setNotes(lists.flat()); })
      .catch(() => {});
    return () => { live = false; };
  }, [shares]);
  if (!withMissing.length || !notes || !notes.length) return null;

  async function remove(/** @type {{share: Share, path: string}} */ n) {
    try {
      await api('DELETE', `/api/shares/${n.share.id}/annotations?path=${encodeURIComponent(n.path)}`);
      say(`Deleted the note for ${n.path}.`);
      onChange();
    } catch (e) {
      if (!superseded(e)) say(e instanceof Error ? e.message : String(e));
    }
  }
  return html`
    <div class="panel pad stack">
      <h2 class="t">Notes whose item is missing</h2>
      <p class="muted">These items were not found at the last scan. A note is kept until you delete it, and is attached again by itself if its item comes back.</p>
      <table class="list">
        <thead><tr><th>Last known place</th><th>Note</th><th class="c-type"></th></tr></thead>
        <tbody>
          ${notes.map((n) => html`
            <tr class="pseudo" key=${n.share.id + ':' + n.path}>
              <td><div class="nm"><span class="fs">${n.share.name}/${n.path}</span></div></td>
              <td>${n.note || ''} ${(n.prefixes || []).map((p) => html`<span class="tag" key=${p}>${p}</span>`)}</td>
              <td><button class="btn small" type="button" disabled=${readOnly} onClick=${() => remove(n)}>Delete</button></td>
            </tr>`)}
        </tbody>
      </table>
    </div>`;
}

export function Annotations() {
  const [shares, setShares] = useState(/** @type {Share[] | null} */ (null));
  const [error, setError] = useState('');
  const [rev, setRev] = useState(0);
  useEffect(() => {
    let live = true;
    api('GET', '/api/shares')
      .then((r) => { if (live) setShares(r.shares); })
      .catch((e) => { if (live && !superseded(e)) setError(e instanceof Error ? e.message : String(e)); });
    return () => { live = false; };
  }, [indexId.value, rev]);

  if (error) return html`<div class="panel pad" role="alert"><b>The shares could not be loaded.</b> ${error}</div>`;
  if (!shares) return html`<p class="muted">Loading…</p>`;
  const described = shares.reduce((n, s) => n + s.annotated, 0);
  return html`
    <div class="stack">
      <div class="sh"><h1>Annotations</h1>
        <span class="sub">${plural(described, 'item', 'items')} described across ${plural(shares.length, 'share', 'shares')}</span></div>
      <${Coverage} shares=${shares} />
      <${Missing} shares=${shares} onChange=${() => setRev((n) => n + 1)} />
      <${Prefixes} />
    </div>`;
}
