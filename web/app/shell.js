// The frame around every screen: the top bar, the list of screens on the
// left, and the strips that say what state the server is in.
import { Annotations } from './annotations.js';
import { api, ApiError, indexId, loadShareAccess, lockedShares, session, shareProblems } from './api.js';
import { Brand, Setup, SignIn } from './auth.js';
import { Find } from './find.js';
import { count, moment } from './format.js';
import { html, useEffect, useState } from './lib.js';
import { Overview } from './overview.js';
import { Review } from './review.js';
import { scan, scanDialog, ScanDialog, ScanStrip, watchScan } from './scan.js';
import { Settings } from './settings.js';
import { Space } from './space.js';
import { Storage } from './storage.js';
import { applyTheme, go, href, route, SCREENS, toast } from './state.js';

const COMING = {

  quarantine: 'Items set aside before deletion, and how to bring them back.',
};

function IndexStatus() {
  const s = scan.value;
  if (!s) return null;
  const cls = s.running ? 'idx run' : s.last_error || !s.index ? 'idx stale' : 'idx';
  const text = s.running ? 'Scanning' : s.index ? `Index: ${moment(new Date(s.index.finished))}` : 'Nothing scanned yet';
  return html`<span class=${cls} title=${s.index ? `${count(s.index.files)} files in ${count(s.index.dirs)} folders` : ''}><i></i>${text}</span>`;
}

function Rail() {
  const current = route.value.screen;
  return html`
    <nav class="rail" aria-label="Screens">
      ${SCREENS.map((s) => html`
        <a key=${s.id} href=${href(s.id)} aria-current=${current === s.id ? 'page' : undefined}
          onClick=${(/** @type {MouseEvent} */ e) => {
            if (e.ctrlKey || e.metaKey || e.shiftKey || e.button !== 0) return; // let the browser open a new tab
            e.preventDefault();
            go(s.id);
          }}>${s.label}${!s.built && html`<small>later</small>`}</a>`)}
    </nav>`;
}

/** What every screen shows before the first scan. */
function FirstRun() {
  const s = session.value;
  const running = scan.value && scan.value.running;
  return html`
    <div class="panel pad stack">
      <div class="sh"><h1>Nothing has been scanned yet</h1></div>
      <p>A scan reads the name, size and dates of every file and folder. It opens no files and changes nothing.</p>
      <div>
        <p class="muted">These will be scanned:</p>
        ${s && s.roots.map((r) => html`<div class="fs" key=${r}>${r}</div>`)}
      </div>
      <div><button class="btn primary" type="button" disabled=${running} onClick=${() => { scanDialog.value = true; }}>
        ${running ? 'The first scan is running' : 'Start the first scan'}</button></div>
    </div>`;
}

function Screen() {
  const { screen } = route.value;
  const status = scan.value;
  if (screen === 'settings') return html`<${Settings} />`;
  if (screen === 'storage') return html`<${Storage} />`;
  if (!status) return html`<p class="muted">Loading…</p>`;
  if (!status.index) return html`<${FirstRun} />`;
  if (screen === 'space') return html`<${Space} />`;
  if (screen === 'review') return html`<${Review} />`;
  if (screen === 'overview') return html`<${Overview} />`;
  if (screen === 'find') return html`<${Find} />`;
  if (screen === 'annotations') return html`<${Annotations} />`;
  const known = SCREENS.find((s) => s.id === screen);
  return html`
    <div class="panel pad stack">
      <div class="sh"><h1>${known ? known.label : 'Quarantine'}</h1><span class="sub">Not built yet</span></div>
      <p>${COMING[/** @type {keyof typeof COMING} */ (screen)] || ''}</p>
      <p class="muted">Space works now: browse what is on the pool, look at an item and record what it is for.</p>
      <div><button class="btn" type="button" onClick=${() => go('space')}>Open Space</button></div>
    </div>`;
}

function Main() {
  const s = session.value;
  const status = scan.value;
  useEffect(() => {
    watchScan();
  }, []);
  const index = indexId.value;
  useEffect(() => {
    if (index) loadShareAccess();
  }, [index]);
  // A share with a problem in its .reflection folder gets its own banner, not the read-only one.
  const problems = [...shareProblems.value.values()];
  const locked = [...lockedShares.value.entries()].filter(([id]) => !shareProblems.value.has(id)).map(([, name]) => name);
  return html`
    <div class="shell">
      <header class="top">
        <${Brand} />
        <div class="top-r">
          <${IndexStatus} />
          <button class="btn small" type="button" disabled=${!status || status.running} onClick=${() => { scanDialog.value = true; }}>Scan now</button>
        </div>
      </header>
      <${ScanStrip} />
      ${s && s.read_only && html`
        <div class="banner" role="status"><div><span class="st"><i aria-hidden="true">i</i>Read-only mode.</span>
          <span> Browsing works. Nothing can be recorded, and nothing on the pool is changed.</span></div></div>`}
      ${s && !s.read_only && locked.length > 0 && html`
        <div class="banner warn" role="status"><div><span class="st"><i aria-hidden="true">!</i>Nothing can be recorded in ${locked.join(", ")}.</span>
          <span> ${locked.length === 1 ? "It is" : "They are"} mapped into the container read-only. Browsing works. To write notes, give the path read-write access
            (in Unraid: Read/Write - Slave) and restart the container.</span></div></div>`}
      ${s && problems.length > 0 && html`
        <div class="banner warn" role="status"><div><span class="st"><i aria-hidden="true">!</i>Something has been put where this program keeps its notes.</span>
          ${problems.map((w, i) => html`<div key=${i}>${w}</div>`)}
          <span>Remove it, or rename it if it is yours, and the share can be written to again.</span></div></div>`}
      ${status && !status.running && status.last_error && html`
        <div class="banner warn" role="status"><div><span class="st"><i aria-hidden="true">!</i>The last scan did not finish.</span>
          <span> Reason: ${status.last_error}. ${status.index ? 'You are looking at the index before it.' : ''}</span></div>
          <button class="btn small" type="button" onClick=${() => { scanDialog.value = true; }}>Scan now</button></div>`}
      ${status && status.warnings && status.warnings.length > 0 && html`
        <div class="banner warn" role="status"><div><span class="st"><i aria-hidden="true">!</i>The last scan had warnings.</span>
          ${status.warnings.map((w, i) => html`<div key=${i}>${w}</div>`)}</div></div>`}
      <div class="body">
        <${Rail} />
        <main id="view"><${Screen} /></main>
      </div>
      <${ScanDialog} />
      ${toast.value && html`<div class="toast" role="status">${toast.value}</div>`}
    </div>`;
}

/** Decides what stands in front of the user: setup, sign-in or the app. */
export function App() {
  const [setup, setSetup] = useState(/** @type {boolean | null} */ (null));
  const [error, setError] = useState('');
  const s = session.value;

  async function boot() {
    setError('');
    try {
      const health = await api('GET', '/api/health');
      setSetup(health.setup_required);
      if (health.setup_required) {
        session.value = null;
        return;
      }
      try {
        session.value = await api('GET', '/api/session');
      } catch (e) {
        if (!(e instanceof ApiError && e.status === 401)) throw e;
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }
  useEffect(() => {
    applyTheme();
    boot();
  }, []);

  if (error) {
    return html`
      <main class="gate"><${Brand} />
        <div class="panel pad stack" role="alert"><p><b>Reflecting Pool could not be reached.</b> ${error}</p>
          <div><button class="btn" type="button" onClick=${boot}>Try again</button></div></div>
      </main>`;
  }
  if (setup === null || s === undefined) return html`<main class="gate"><${Brand} /></main>`;
  if (setup) return html`<${Setup} onDone=${() => setSetup(false)} />`;
  if (!s) return html`<${SignIn} />`;
  return html`<${Main} />`;
}
