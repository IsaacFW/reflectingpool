// The right-hand column: whatever item is selected, with its facts, a
// preview and the form that records what it is for. Review uses the same
// preview and form.
import { api, contentURL, FLAG, indexId, session, STATE, superseded } from './api.js';
import { ago, bytes, count, date, plural } from './format.js';
import { html, useEffect, useState } from './lib.js';
import { prefs, say } from './state.js';

/** @typedef {import('./api.js').Entry} Entry */
/** @typedef {import('./api.js').Annotation} Annotation */

const MiB = 1024 * 1024;
// What a browser shows by itself. Anything else waits for generated previews.
const IMAGES = ['jpg', 'jpeg', 'png', 'gif', 'webp', 'avif', 'bmp', 'ico', 'svg'];
const VIDEOS = ['mp4', 'm4v', 'webm', 'mov', 'mkv', 'ogv'];
const AUDIO = ['mp3', 'flac', 'wav', 'ogg', 'opus', 'm4a', 'aac'];

/** The colour class for what a file's bytes are. Three hues and a neutral: the most that stay apart for colour-blind readers. */
export function typeClass(/** @type {Entry} */ e) {
  if (e.kind === 'dir') return 'k-plain'; // folders are neutral until the index sums types per folder
  if (e.type === 'video') return 'k-video';
  if (e.type === 'archive' || e.type === 'disk-image') return 'k-backup';
  if (e.type === 'image') return 'k-image';
  return 'k-other';
}

/** The first part of a text file, fetched as text and shown as text. */
function TextPreview(/** @type {{entry: Entry}} */ { entry }) {
  const [text, setText] = useState(/** @type {string | null} */ (null));
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    let live = true;
    setText(null);
    setFailed(false);
    fetch(contentURL(entry.id), { headers: { Range: 'bytes=0-65535' }, credentials: 'same-origin' })
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error(String(r.status)))))
      .then((t) => { if (live) setText(t); })
      .catch(() => { if (live) setFailed(true); });
    return () => { live = false; };
  }, [entry.id, indexId.value]);
  if (failed) return html`<div class="pv-msg"><b>The file could not be read.</b>It may have been moved or deleted since the last scan.</div>`;
  if (text === null) return html`<div class="pv-msg">Loading…</div>`;
  return html`
    <pre class="pv-text">${text}</pre>
    ${entry.size > 65536 && html`<div class="pv-cap">Showing the first 64 KiB of ${bytes(entry.size)}.</div>`}`;
}

/** What is in a folder: its largest children. */
function FolderPreview(/** @type {{entry: Entry}} */ { entry }) {
  const [kids, setKids] = useState(/** @type {{children: Entry[], total: number} | null} */ (null));
  const mode = prefs.value.size;
  useEffect(() => {
    let live = true;
    setKids(null);
    api('GET', `/api/tree?id=${entry.id}&sort=${mode === 'disk' ? 'disk' : 'size'}&desc=1&limit=8`)
      .then((t) => { if (live) setKids(t); })
      .catch(() => { /* the list beside it reports the failure */ });
    return () => { live = false; };
  }, [entry.id, mode, indexId.value]);
  if (!kids) return null;
  if (kids.total === 0) return html`<div class="pv-msg">This folder is empty.</div>`;
  return html`
    <table class="mini">
      <tbody>
        ${kids.children.map((k) => html`
          <tr key=${k.id}><td class="fs">${k.name}${k.kind === 'dir' ? '/' : ''}</td><td class="s num">${bytes(mode === 'disk' ? k.disk : k.size)}</td></tr>`)}
      </tbody>
    </table>
    ${kids.total > kids.children.length && html`<div class="pv-cap">The largest ${kids.children.length} of ${count(kids.total)}.</div>`}`;
}

/**
 * A best-effort look at the item itself.
 * @param {{entry: Entry}} props
 */
export function Preview({ entry }) {
  const [broken, setBroken] = useState(false);
  const [asked, setAsked] = useState(false);
  useEffect(() => { setBroken(false); setAsked(false); }, [entry.id]);
  const src = contentURL(entry.id);
  const none = (/** @type {string} */ why) => html`<div class="pv-msg"><b>No preview.</b>${why}</div>`;
  // Large files are fetched only when asked for.
  const ask = (/** @type {string} */ what) => html`
    <div class="pv-msg"><b>${what}, ${bytes(entry.size)}.</b>
      <button class="btn small" type="button" onClick=${() => setAsked(true)}>Load the preview</button></div>`;

  if (entry.kind === 'dir') return html`<div class="pv compact"><${FolderPreview} entry=${entry} /></div>`;
  if (entry.kind !== 'file') return none(entry.kind === 'symlink' ? 'This is a link to another place.' : 'This is not a regular file.');
  if (entry.size === 0) return none('The file is empty.');
  if (broken) return none('The file could not be shown. This browser may not support its format, or the file has moved since the last scan.');

  let body;
  if (IMAGES.includes(entry.ext)) {
    body = entry.size > 50 * MiB && !asked ? ask('A large image')
      : html`<img src=${src} alt="" onError=${() => setBroken(true)} />`;
  } else if (VIDEOS.includes(entry.ext)) {
    body = html`<video src=${src} controls preload="metadata" onError=${() => setBroken(true)}></video>`;
  } else if (AUDIO.includes(entry.ext)) {
    body = html`<audio src=${src} controls preload="none" onError=${() => setBroken(true)}></audio>`;
  } else if (entry.ext === 'pdf') {
    body = entry.size > 150 * MiB && !asked ? ask('A large PDF')
      : html`<iframe src=${src} title=${'Preview of ' + entry.name}></iframe>`;
  } else if (entry.type === 'text' || entry.type === 'subtitle') {
    body = html`<${TextPreview} entry=${entry} />`;
  } else {
    return none(entry.type === 'image' || entry.type === 'video'
      ? 'This browser cannot show this format. Generated previews for such files are on the way.'
      : 'There is nothing to show for this kind of file.');
  }
  return html`<div class="pv compact">${body}</div>`;
}

/** The marks next to a name: what is recorded about the item and what is unusual about it. */
export function Marks(/** @type {{entry: Entry}} */ { entry }) {
  const out = [];
  if (entry.state & (STATE.note | STATE.prefix | STATE.displayName)) out.push(html`<span class="mark noted" key="d">described</span>`);
  else if (entry.state & STATE.skipped) out.push(html`<span class="mark" key="s" title="Skipped in a review">skipped</span>`);
  if (entry.kind === 'file' && entry.nlink > 1) {
    out.push(html`<span class="mark" key="l" title=${entry.counted ? 'Hardlinked. Its size is counted here.' : 'Hardlinked. Its size is counted at its other name.'}>${entry.nlink} links</span>`);
  }
  if (entry.flags & FLAG.excluded) out.push(html`<span class="mark" key="x" title="Left out of scans">not scanned</span>`);
  if (entry.flags & FLAG.error) out.push(html`<span class="mark" key="e" title="Part of it could not be read during the scan">could not be read completely</span>`);
  return out;
}

/** Turns "6m" or "1y" into a date; leaves a date as it is. */
function resolveReview(/** @type {string} */ text) {
  const m = /^(\d+)\s*([dwmy])$/i.exec(text.trim());
  if (!m) return text.trim();
  const d = new Date();
  const n = Number(m[1]);
  const unit = m[2].toLowerCase();
  if (unit === 'd') d.setDate(d.getDate() + n);
  if (unit === 'w') d.setDate(d.getDate() + 7 * n);
  if (unit === 'm') d.setMonth(d.getMonth() + n);
  if (unit === 'y') d.setFullYear(d.getFullYear() + n);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

/**
 * The fields that record what an item is for.
 * @param {{entry: Entry, annotation: Annotation | null, onSaved: () => void}} props
 */
export function AnnotationForm({ entry, annotation, onSaved }) {
  const blank = { note: '', prefixes: '', display_name: '', owner: '', review_after: '' };
  const from = (/** @type {Annotation | null} */ a) => (a && !a.skipped ? {
    note: a.note || '', prefixes: (a.prefixes || []).join(', '), display_name: a.display_name || '',
    owner: a.owner || '', review_after: a.review_after || '',
  } : blank);
  const [form, setForm] = useState(from(annotation));
  const [known, setKnown] = useState(/** @type {{name: string, meaning: string}[]} */ ([]));
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const readOnly = !!(session.value && session.value.read_only);
  const outside = entry.share === 0;

  useEffect(() => { setForm(from(annotation)); setError(''); }, [entry.id, annotation && annotation.updated]);
  useEffect(() => {
    api('GET', '/api/prefixes').then((r) => setKnown(r.prefixes)).catch(() => {});
  }, []);

  const set = (/** @type {string} */ k) => (/** @type {Event} */ e) =>
    setForm({ ...form, [k]: /** @type {HTMLInputElement} */ (e.target).value });
  const empty = !Object.values(form).some((v) => v.trim());
  const review = resolveReview(form.review_after);

  async function save(/** @type {Event} */ e) {
    e.preventDefault();
    if (empty || busy) return;
    setBusy(true);
    setError('');
    try {
      await api('PUT', `/api/entries/${entry.id}/annotation`, {
        note: form.note, display_name: form.display_name, owner: form.owner, review_after: review,
        prefixes: form.prefixes.split(',').map((p) => p.trim()).filter(Boolean),
      });
      say(`Saved: ${entry.name}`);
      onSaved();
    } catch (err) {
      if (!superseded(err)) setError(err instanceof Error ? err.message : String(err));
    }
    setBusy(false);
  }

  async function remove() {
    setBusy(true);
    setError('');
    try {
      await api('DELETE', `/api/entries/${entry.id}/annotation`);
      say(annotation && annotation.skipped ? `Returned to review: ${entry.name}` : `Removed what was recorded about ${entry.name}`);
      onSaved();
    } catch (err) {
      if (!superseded(err)) setError(err instanceof Error ? err.message : String(err));
    }
    setBusy(false);
  }

  if (outside) {
    return html`<p class="hint">Notes are kept inside each share, so only items inside a share can have one.</p>`;
  }
  const off = readOnly || busy;
  return html`
    <form class="stack" onSubmit=${save}
      onKeyDown=${(/** @type {KeyboardEvent} */ e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) save(e); }}>
      ${readOnly && html`<p class="hint">Read-only mode: nothing can be recorded.</p>`}
      ${annotation && annotation.skipped && html`<p class="hint">This item was skipped in a review.</p>`}
      <div class="field">
        <label for="an-note">What is it for?</label>
        <textarea id="an-note" rows="3" maxlength="10000" disabled=${off} value=${form.note} onInput=${set('note')}></textarea>
      </div>
      ${known.length > 0 && html`
        <div class="field">
          <label for="an-prefixes">Prefixes</label>
          <input id="an-prefixes" type="text" list="an-prefix-list" disabled=${off} value=${form.prefixes} onInput=${set('prefixes')} />
          <datalist id="an-prefix-list">${known.map((p) => html`<option key=${p.name} value=${p.name}>${p.meaning}</option>`)}</datalist>
          <span class="hint">Separate several with commas. Known: ${known.map((p) => p.name).join(', ')}.</span>
        </div>`}
      <div class="field">
        <label for="an-display">Display name</label>
        <input id="an-display" type="text" maxlength="255" disabled=${off} value=${form.display_name} onInput=${set('display_name')} />
        <span class="hint">A readable name shown here. The name on disk does not change.</span>
      </div>
      <div class="field">
        <label for="an-owner">Owner</label>
        <input id="an-owner" type="text" maxlength="100" disabled=${off} value=${form.owner} onInput=${set('owner')} />
      </div>
      <div class="field">
        <label for="an-review">Review after</label>
        <input id="an-review" type="text" placeholder="2027-01-31, or 6m, 1y" disabled=${off} value=${form.review_after} onInput=${set('review_after')} />
        ${review && review !== form.review_after.trim() && html`<span class="hint">That is ${review}.</span>`}
      </div>
      ${error && html`<p class="err" role="alert">${error}</p>`}
      <div class="actions">
        <button class="btn primary" type="submit" disabled=${off || empty}>Save <span class="kbd">Ctrl+Enter</span></button>
        ${annotation && html`<button class="btn" type="button" disabled=${off} onClick=${remove}>
          ${annotation.skipped ? 'Return to review' : 'Remove'}</button>`}
      </div>
    </form>`;
}

/**
 * Everything about one item.
 * @param {{id: number, rev: number, onOpen: (e: Entry) => void, onChanged: () => void}} props
 */
export function Inspector({ id, rev, onOpen, onChanged }) {
  const [data, setData] = useState(/** @type {{entry: Entry, annotation: Annotation | null, links: Entry[]} | null} */ (null));
  const [error, setError] = useState('');
  const mode = prefs.value.size;

  useEffect(() => {
    let live = true;
    setError('');
    api('GET', `/api/entries/${id}`)
      .then((d) => { if (live) setData(d); })
      .catch((e) => { if (live && !superseded(e)) setError(e instanceof Error ? e.message : String(e)); });
    return () => { live = false; };
  }, [id, rev, indexId.value]);

  if (error) return html`<aside class="panel insp side" aria-label="Selected item"><p class="err" role="alert">Could not load this item: ${error}</p></aside>`;
  if (!data || data.entry.id !== id) return html`<aside class="panel insp side" aria-label="Selected item"><p class="muted">Loading…</p></aside>`;

  const { entry: e, annotation, links } = data;
  const isDir = e.kind === 'dir';
  const changed = isDir && e.max_mtime ? e.max_mtime : e.mtime;
  const others = links.filter((l) => l.id !== e.id);
  return html`
    <aside class="panel insp side" aria-label="Selected item">
      <div>
        <div class="insp-kind">${isDir ? 'Folder' : e.kind === 'file' ? `File, ${e.type}` : e.kind}</div>
        ${annotation && annotation.display_name
          ? html`<h3>${annotation.display_name}</h3><div class="insp-name fs">${e.name}</div>`
          : html`<h3 class="fs">${e.name}</h3>`}
        <div class="insp-path fs">${e.path}</div>
      </div>
      <div class="chips"><${Marks} entry=${e} /></div>
      <dl class="facts">
        <dt>On disk</dt><dd class=${mode === 'disk' ? 'on' : ''}>${bytes(e.disk)}</dd>
        <dt>Apparent size</dt><dd class=${mode === 'apparent' ? 'on' : ''}>${bytes(e.size)}</dd>
        ${isDir && html`<dt>Inside</dt><dd>${plural(e.files, 'file', 'files')}, ${plural(e.dirs, 'folder', 'folders')}</dd>`}
        <dt>Last changed</dt><dd>${changed ? `${date(changed)} (${ago(changed)})` : 'not recorded'}</dd>
        ${e.btime > 0 && html`<dt>Created</dt><dd>${date(e.btime)} (${ago(e.btime)})</dd>`}
      </dl>
      ${others.length > 0 && html`
        <div class="links">The same file is also at:
          ${others.map((l) => html`<div class="fs" key=${l.id}>${l.path}</div>`)}
          Removing one of these names frees nothing while the others exist.
        </div>`}
      ${isDir && html`<div class="actions"><button class="btn small" type="button" onClick=${() => onOpen(e)}>Open this folder</button></div>`}
      <${Preview} entry=${e} />
      <${AnnotationForm} entry=${e} annotation=${annotation} onSaved=${onChanged} />
    </aside>`;
}
