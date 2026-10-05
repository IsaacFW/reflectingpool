// Scans: the status the whole page watches, the dialog that starts one, and
// the strip that shows one in progress.
import { api, session } from './api.js';
import { html, signal, useEffect, useRef, useState } from './lib.js';
import { count, duration, moment } from './format.js';
import { say } from './state.js';

/**
 * @typedef {{index_id: string, started: string, intensity: string, trigger: string, seconds: number, entries: number, errors: number}} ScanRecord
 * @typedef {{id: string, started: string, finished: string, roots: string[], files: number, dirs: number, size: number, disk: number, errors: number, intensity?: string}} IndexInfo
 * @typedef {{running: boolean, started?: string, intensity?: string, rested_seconds: number, entries: number,
 *   expected: number, percent: number, errors: number, last_error?: string, warnings?: string[],
 *   index?: IndexInfo, last_choice: string, history: ScanRecord[],
 *   schedule: {enabled: boolean, time: string, intensity: string}, next_scheduled?: string}} ScanStatus
 */

/** What GET /api/scan last said; null until it has answered. */
export const scan = signal(/** @type {ScanStatus | null} */ (null));

/** Whether the dialog that starts a scan is open. */
export const scanDialog = signal(false);

let timer = 0;
let stoppedHere = false; // the user pressed Stop, so the scan ending early is no surprise

/** Asks for the scan status now, and again later: often while a scan runs, seldom otherwise. */
export async function watchScan() {
  clearTimeout(timer);
  if (!session.value) return;
  const before = scan.value;
  try {
    const now = /** @type {ScanStatus} */ (await api('GET', '/api/scan'));
    scan.value = now;
    if (before && before.running && !now.running) {
      say(stoppedHere ? 'Scan stopped.' : now.last_error ? `The scan did not finish: ${now.last_error}`
        : now.index ? `Scan finished: ${count(now.index.files)} files in ${count(now.index.dirs)} folders.` : 'Scan finished.');
    }
  } catch { /* the next look will try again */ }
  timer = setTimeout(watchScan, scan.value && scan.value.running ? 1500 : 20000);
}

const INTENSITIES = [
  { id: 'low', name: 'Low impact', what: 'One folder at a time, with pauses. Streaming, backups and other work on the server are not slowed. The disks stay active the whole time.' },
  { id: 'balanced', name: 'Balanced', what: 'A few folders at a time. The disks are busy and the server stays responsive.' },
  { id: 'aggressive', name: 'Aggressive', what: 'As many folders at once as the server allows. Every disk works flat out, so other disk work slows down until it finishes.' },
];

/** How long the last scan at an intensity took here, in words. */
function lastTime(/** @type {ScanRecord[]} */ history, /** @type {string} */ intensity) {
  const last = history.find((h) => h.intensity === intensity);
  if (!last) return 'Not run on this server yet';
  return `Took ${duration(last.seconds)} last time (${moment(new Date(last.started))})`;
}

export function ScanDialog() {
  const ref = useRef(/** @type {HTMLDialogElement | null} */ (null));
  const status = scan.value;
  const open = scanDialog.value;
  const [choice, setChoice] = useState('balanced');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    const dlg = ref.current;
    if (!dlg) return;
    if (open && !dlg.open) {
      // Balanced the first time, then whatever was chosen last.
      setChoice((status && status.last_choice) || 'balanced');
      setError('');
      dlg.showModal();
    } else if (!open && dlg.open) {
      dlg.close();
    }
  }, [open]);

  async function start(/** @type {Event} */ e) {
    e.preventDefault();
    setBusy(true);
    try {
      await api('POST', '/api/scan', { intensity: choice });
      stoppedHere = false;
      scanDialog.value = false;
      await watchScan();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
    setBusy(false);
  }

  const history = (status && status.history) || [];
  return html`
    <dialog ref=${ref} aria-labelledby="scan-title" onClose=${() => { scanDialog.value = false; }}>
      <form class="dlg" onSubmit=${start}>
        <h2 id="scan-title">Start a scan</h2>
        <p class="muted">A scan reads names, sizes and dates only. Whichever you pick, every disk in the pool is used,
          because that information is spread across all of them. The choice is how hard the disks work and for how long.
          You keep browsing the last index until the scan finishes.</p>
        ${INTENSITIES.map((it) => html`
          <label class="opt-card" key=${it.id}>
            <input type="radio" name="intensity" value=${it.id} checked=${choice === it.id} onChange=${() => setChoice(it.id)} />
            <b>${it.name}</b>
            <span class="tm">${lastTime(history, it.id)}</span>
            <span>${it.what}</span>
          </label>`)}
        ${error && html`<p class="err" role="alert">${error}</p>`}
        <div class="dlg-f">
          <button class="btn" type="button" onClick=${() => { scanDialog.value = false; }}>Cancel</button>
          <button class="btn primary" type="submit" disabled=${busy}>Start scan</button>
        </div>
      </form>
    </dialog>`;
}

/** The strip under the top bar while a scan runs. */
export function ScanStrip() {
  const s = scan.value;
  const [stopping, setStopping] = useState(false);
  useEffect(() => { if (!s || !s.running) setStopping(false); }, [s && s.running]);
  if (!s || !s.running) return null;

  async function stop() {
    setStopping(true);
    stoppedHere = true;
    try {
      await api('DELETE', '/api/scan');
    } catch { /* it had already finished */ }
    watchScan();
  }
  const progress = s.expected > 0
    ? `${count(s.entries)} of about ${count(s.expected)} files and folders read (${Math.floor(s.percent)}%)`
    : `${count(s.entries)} files and folders read so far`;
  return html`
    <div class="banner info" role="status">
      <div>
        <span class="st"><i aria-hidden="true">i</i>Scanning, ${s.intensity === 'low' ? 'low impact' : s.intensity}.</span>
        <span> ${progress}${s.errors ? `, ${count(s.errors)} could not be read` : ''}.
          ${s.index ? ' You are looking at the previous index until it finishes.' : ''}</span>
        ${s.expected > 0 && html`<div class="prog"><i style=${{ width: `${s.percent.toFixed(1)}%` }}></i></div>`}
      </div>
      <button class="btn small" type="button" onClick=${stop} disabled=${stopping}>${stopping ? 'Stopping' : 'Stop'}</button>
    </div>`;
}
