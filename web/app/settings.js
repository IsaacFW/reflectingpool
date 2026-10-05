// Settings: how sizes are shown, the optional daily scan, and the account.
import { api, session } from './api.js';
import { html, useEffect, useState } from './lib.js';
import { scan, watchScan } from './scan.js';
import { prefs, say, setPref } from './state.js';

/**
 * A row of buttons of which one is on.
 * @template {string} V
 * @param {{label: string, value: V, options: [V, string][], onPick: (v: V) => void}} props
 */
function Choice({ label, value, options, onPick }) {
  return html`
    <div class="ctl"><span class="ctl-l">${label}</span>
      <div class="seg" role="group" aria-label=${label}>
        ${options.map(([v, text]) => html`
          <button type="button" key=${v} aria-pressed=${v === value ? 'true' : 'false'} onClick=${() => onPick(v)}>${text}</button>`)}
      </div>
    </div>`;
}

function Schedule() {
  const [saved, setSaved] = useState(/** @type {{enabled: boolean, time: string, intensity: string} | null} */ (null));
  const [form, setForm] = useState({ enabled: false, time: '03:00', intensity: 'low' });
  const [error, setError] = useState('');
  const readOnly = false; // a schedule changes nothing on the pool, so read-only mode allows it

  useEffect(() => {
    api('GET', '/api/settings').then((s) => {
      setSaved(s.scan_schedule);
      setForm({ ...s.scan_schedule, time: s.scan_schedule.time || '03:00', intensity: s.scan_schedule.intensity || 'low' });
    }).catch((e) => setError(e.message));
  }, []);

  async function save(/** @type {Event} */ e) {
    e.preventDefault();
    setError('');
    try {
      const s = await api('PUT', '/api/settings', { scan_schedule: form });
      setSaved(s.scan_schedule);
      say(form.enabled ? `A scan will run every day at ${form.time}.` : 'No scan will run by itself.');
      watchScan();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }

  const next = scan.value && scan.value.next_scheduled;
  return html`
    <form class="panel pad stack" onSubmit=${save}>
      <h2>Scheduled scan</h2>
      <p class="muted">Nothing is scanned unless you start a scan or set a time here.</p>
      <label class="row"><input type="checkbox" checked=${form.enabled} disabled=${readOnly}
        onChange=${(/** @type {Event} */ e) => setForm({ ...form, enabled: /** @type {HTMLInputElement} */ (e.target).checked })} />
        Scan every day</label>
      <div class="row">
        <div class="field">
          <label for="sch-time">At</label>
          <input id="sch-time" type="time" required disabled=${!form.enabled} value=${form.time}
            onInput=${(/** @type {Event} */ e) => setForm({ ...form, time: /** @type {HTMLInputElement} */ (e.target).value })} />
        </div>
        <div class="field">
          <label for="sch-intensity">Intensity</label>
          <select id="sch-intensity" disabled=${!form.enabled} value=${form.intensity}
            onChange=${(/** @type {Event} */ e) => setForm({ ...form, intensity: /** @type {HTMLSelectElement} */ (e.target).value })}>
            <option value="low">Low impact</option>
            <option value="balanced">Balanced</option>
            <option value="aggressive">Aggressive</option>
          </select>
        </div>
      </div>
      ${saved && saved.enabled && next && html`<p class="hint">Next: ${new Date(next).toLocaleString()}</p>`}
      ${error && html`<p class="err" role="alert">${error}</p>`}
      <div><button class="btn primary" type="submit">Save the schedule</button></div>
    </form>`;
}

export function Settings() {
  const p = prefs.value;
  const s = session.value;
  async function signOut() {
    try {
      await api('POST', '/api/logout');
    } catch { /* signed out either way */ }
    session.value = null;
  }
  return html`
    <div class="stack settings">
      <div class="sh"><h1>Settings</h1></div>
      <div class="panel pad stack">
        <h2>Display</h2>
        <${Choice} label="Units" value=${p.units} onPick=${(/** @type {'decimal' | 'binary'} */ v) => setPref('units', v)}
          options=${[['decimal', 'GB, TB (decimal)'], ['binary', 'GiB, TiB (binary)']]} />
        <${Choice} label="Sizes" value=${p.size} onPick=${(/** @type {'disk' | 'apparent'} */ v) => setPref('size', v)}
          options=${[['disk', 'On disk'], ['apparent', 'Apparent']]} />
        <${Choice} label="Theme" value=${p.theme} onPick=${(/** @type {'system' | 'light' | 'dark'} */ v) => setPref('theme', v)}
          options=${[['system', 'Follow the system'], ['light', 'Light'], ['dark', 'Dark']]} />
        <p class="hint">On disk is what removing something frees. Apparent is the size of the contents before compression.
          These choices are kept in this browser.</p>
      </div>
      <${Schedule} />
      <div class="panel pad stack">
        <h2>Account</h2>
        <p>Signed in as <b>${s ? s.user : ''}</b>.</p>
        <div><button class="btn" type="button" onClick=${signOut}>Sign out</button></div>
        <p class="hint">Forgotten password: run <span class="fs">reflectingpool reset-admin</span> in the container, then create the account again with the setup code from its log.</p>
      </div>
      <div class="panel pad stack">
        <h2>About</h2>
        <dl class="facts">
          <dt>Version</dt><dd>${s ? s.version : ''}</dd>
          <dt>Scans</dt><dd class="fs">${s ? s.roots.join(', ') : ''}</dd>
          <dt>Mode</dt><dd>${s && s.read_only ? 'Read-only: nothing on the pool is changed' : 'Annotations are written into each share'}</dd>
        </dl>
      </div>
    </div>`;
}
