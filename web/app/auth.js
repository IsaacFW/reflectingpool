// The two pages in front of everything else: creating the account the first
// time, and signing in.
import { api, ApiError, session } from './api.js';
import { html, useEffect, useState } from './lib.js';

function Brand() {
  return html`
    <div class="brand">
      <svg width="22" height="22" viewBox="0 0 22 22" aria-hidden="true">
        <rect class="lg-a" x="3" y="3" width="16" height="7" rx="1.5" /><rect class="lg-b" x="3" y="12" width="16" height="7" rx="1.5" />
      </svg>
      <span>Reflecting Pool</span>
    </div>`;
}

/** Counts down the seconds of a lockout, so the message stays true while it is read. */
function useCountdown(/** @type {number} */ seconds) {
  const [left, setLeft] = useState(seconds);
  useEffect(() => {
    setLeft(seconds);
    if (seconds <= 0) return;
    const t = setInterval(() => setLeft((n) => Math.max(0, n - 1)), 1000);
    return () => clearInterval(t);
  }, [seconds]);
  return left;
}

/** @param {{username: string, password: string}} who */
async function signIn(who) {
  session.value = await api('POST', '/api/login', who);
}

/** @param {{onDone: () => void}} props */
export function Setup({ onDone }) {
  const [form, setForm] = useState({ code: '', username: '', password: '', again: '' });
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const set = (/** @type {string} */ k) => (/** @type {Event} */ e) =>
    setForm({ ...form, [k]: /** @type {HTMLInputElement} */ (e.target).value });

  async function submit(/** @type {Event} */ e) {
    e.preventDefault();
    if (form.password !== form.again) {
      setError('The two passwords are not the same.');
      return;
    }
    setBusy(true);
    setError('');
    try {
      await api('POST', '/api/setup', { code: form.code.trim(), username: form.username.trim(), password: form.password });
      await signIn({ username: form.username.trim(), password: form.password });
      onDone();
    } catch (err) {
      setError(err instanceof ApiError && err.code === 'bad_setup_code'
        ? 'That is not the setup code. It is printed in the container log each time the program starts without an account.'
        : err instanceof Error ? err.message : String(err));
    }
    setBusy(false);
  }

  return html`
    <main class="gate">
      <${Brand} />
      <div class="panel pad">
        <form onSubmit=${submit}>
          <h1>Create the admin account</h1>
          <p class="muted">This is the only account. It keeps everyone else on the network out.</p>
          <div class="field">
            <label for="su-code">Setup code</label>
            <input id="su-code" type="text" autocomplete="off" required value=${form.code} onInput=${set('code')} />
            <span class="hint">Open the container's log in Unraid. The code is printed at start-up.</span>
          </div>
          <div class="field">
            <label for="su-user">Username</label>
            <input id="su-user" type="text" autocomplete="username" required value=${form.username} onInput=${set('username')} />
          </div>
          <div class="field">
            <label for="su-pass">Password</label>
            <input id="su-pass" type="password" autocomplete="new-password" required value=${form.password} onInput=${set('password')} />
          </div>
          <div class="field">
            <label for="su-again">Password again</label>
            <input id="su-again" type="password" autocomplete="new-password" required value=${form.again} onInput=${set('again')} />
          </div>
          ${error && html`<p class="err" role="alert">${error}</p>`}
          <button class="btn primary" type="submit" disabled=${busy}>Create account and sign in</button>
        </form>
      </div>
    </main>`;
}

export function SignIn() {
  const [form, setForm] = useState({ username: '', password: '' });
  const [error, setError] = useState('');
  const [locked, setLocked] = useState(0);
  const [busy, setBusy] = useState(false);
  const wait = useCountdown(locked);
  const set = (/** @type {string} */ k) => (/** @type {Event} */ e) =>
    setForm({ ...form, [k]: /** @type {HTMLInputElement} */ (e.target).value });

  async function submit(/** @type {Event} */ e) {
    e.preventDefault();
    setBusy(true);
    setError('');
    try {
      await signIn({ username: form.username.trim(), password: form.password });
    } catch (err) {
      if (err instanceof ApiError && err.code === 'locked') {
        setLocked(err.retryAfter || 30);
      } else {
        setError(err instanceof ApiError && err.code === 'invalid_credentials' ? 'Wrong username or password.'
          : err instanceof Error ? err.message : String(err));
      }
    }
    setBusy(false);
  }

  return html`
    <main class="gate">
      <${Brand} />
      <div class="panel pad">
        <form onSubmit=${submit}>
          <h1>Sign in</h1>
          <div class="field">
            <label for="si-user">Username</label>
            <input id="si-user" type="text" autocomplete="username" required value=${form.username} onInput=${set('username')} />
          </div>
          <div class="field">
            <label for="si-pass">Password</label>
            <input id="si-pass" type="password" autocomplete="current-password" required value=${form.password} onInput=${set('password')} />
          </div>
          ${wait > 0 && html`<p class="err" role="alert">Too many attempts. Try again in ${wait} s.</p>`}
          ${error && html`<p class="err" role="alert">${error}</p>`}
          <button class="btn primary" type="submit" disabled=${busy || wait > 0}>Sign in</button>
        </form>
      </div>
    </main>`;
}

export { Brand };
