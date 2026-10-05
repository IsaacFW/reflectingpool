// The fields that record what an item is for. The inspector and the review
// screen both use them, so a note is written the same way wherever it is.
import { api } from './api.js';
import { html, signal } from './lib.js';

/** @typedef {import('./api.js').Annotation} Annotation */
/** @typedef {{note: string, prefixes: string, display_name: string, owner: string, review_after: string}} Form */

/** @type {Form} */
export const BLANK = { note: '', prefixes: '', display_name: '', owner: '', review_after: '' };

/** The form's starting values for what is recorded already. A skip records nothing. */
export function formFrom(/** @type {Annotation | null | undefined} */ a) {
  if (!a || a.skipped) return { ...BLANK };
  return {
    note: a.note || '', prefixes: (a.prefixes || []).join(', '), display_name: a.display_name || '',
    owner: a.owner || '', review_after: a.review_after || '',
  };
}

export function isBlank(/** @type {Form} */ f) {
  return !Object.values(f).some((v) => v.trim());
}

export function sameForm(/** @type {Form} */ a, /** @type {Form} */ b) {
  return a.note === b.note && a.prefixes === b.prefixes && a.display_name === b.display_name
    && a.owner === b.owner && a.review_after === b.review_after;
}

/** Turns "6m" or "1y" into a date; leaves anything else as it is. */
export function resolveReview(/** @type {string} */ text) {
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

/** What the server is sent for a form. All of it empty is how an item is skipped. */
export function bodyOf(/** @type {Form} */ f) {
  return {
    note: f.note, display_name: f.display_name, owner: f.owner, review_after: resolveReview(f.review_after),
    prefixes: f.prefixes.split(',').map((p) => p.trim()).filter(Boolean),
  };
}

/** The prefix list: null until fetched. Read again after it is edited. */
export const prefixList = signal(/** @type {{name: string, meaning: string, count?: number}[] | null} */ (null));

export function loadPrefixes() {
  return api('GET', '/api/prefixes').then((r) => { prefixList.value = r.prefixes; }).catch(() => {});
}

/**
 * @param {{form: Form, onChange: (f: Form) => void, disabled?: boolean, id?: string,
 *   noteRef?: {current: HTMLTextAreaElement | null}, displayRef?: {current: HTMLInputElement | null},
 *   onNoteKey?: (e: KeyboardEvent) => void}} props
 */
export function Fields({ form, onChange, disabled, id = 'an', noteRef, displayRef, onNoteKey }) {
  const known = prefixList.value || [];
  const set = (/** @type {keyof Form} */ k) => (/** @type {Event} */ e) =>
    onChange({ ...form, [k]: /** @type {HTMLInputElement} */ (e.target).value });
  const review = resolveReview(form.review_after);
  return html`
    <div class="field">
      <label for=${id + '-note'}>What is it for?</label>
      <textarea id=${id + '-note'} ref=${noteRef} rows="3" maxlength="10000" disabled=${disabled}
        value=${form.note} onInput=${set('note')} onKeyDown=${onNoteKey}></textarea>
    </div>
    ${known.length > 0 && html`
      <div class="field">
        <label for=${id + '-prefixes'}>Prefixes</label>
        <input id=${id + '-prefixes'} type="text" list=${id + '-prefix-list'} disabled=${disabled} value=${form.prefixes} onInput=${set('prefixes')} />
        <datalist id=${id + '-prefix-list'}>${known.map((p) => html`<option key=${p.name} value=${p.name}>${p.meaning}</option>`)}</datalist>
        <span class="hint">Separate several with commas. Known: ${known.map((p) => p.name).join(', ')}.</span>
      </div>`}
    <div class="field">
      <label for=${id + '-display'}>Display name</label>
      <input id=${id + '-display'} ref=${displayRef} type="text" maxlength="255" disabled=${disabled} value=${form.display_name} onInput=${set('display_name')} />
      <span class="hint">A readable name shown here. The name on disk does not change.</span>
    </div>
    <div class="field">
      <label for=${id + '-owner'}>Owner</label>
      <input id=${id + '-owner'} type="text" maxlength="100" disabled=${disabled} value=${form.owner} onInput=${set('owner')} />
    </div>
    <div class="field">
      <label for=${id + '-review'}>Review after</label>
      <input id=${id + '-review'} type="text" placeholder="2027-01-31, or 6m, 1y" disabled=${disabled} value=${form.review_after} onInput=${set('review_after')} />
      ${review && review !== form.review_after.trim() && html`<span class="hint">That is ${review}.</span>`}
    </div>`;
}
