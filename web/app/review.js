// Review: one item at a time, its preview beside it, in the order the user
// chose. Saving never waits: the next item shows at once and the write
// finishes behind it.
import { api, ApiError, indexId, lockedShares, session, superseded } from './api.js';
import { BLANK, bodyOf, Fields, formFrom, isBlank, loadPrefixes, prefixList, sameForm } from './annotate.js';
import { ago, bytes, count, plural } from './format.js';
import { Preview } from './inspector.js';
import { html, useEffect, useLayoutEffect, useRef, useState } from './lib.js';
import { DEFAULT, defFromParams, GROUP_KEYS, groupPhrase, lastParams, ORDERS, paramsFromDef, PRESETS, rememberParams, resolve, sentence } from './queue.js';
import { go, href, route, say } from './state.js';

/** @typedef {import('./api.js').Entry} Entry */
/** @typedef {import('./annotate.js').Form} Form */
/** @typedef {import('./queue.js').QueueDef} QueueDef */
/**
 * @typedef {{values: {key: string, value: unknown, label: string}[], remaining: number, total: number, size: number}} Group
 * @typedef {{groups: Group[], group_count: number, group_total: number, group_position: number,
 *   remaining: number, total: number, total_size: number, items: Entry[]}} Queue
 * @typedef {{entry: Entry, form: Form, outcome: 'saving' | 'saved' | 'skipped' | 'failed', error?: string, gone?: boolean}} Done
 */

// What is being typed, kept outside the screen so that it survives the
// screen being rebuilt when a scan replaces the index under it.
let draft = /** @type {{path: string, form: Form} | null} */ (null);
// Notes saved earlier in this visit, newest first, for reuse with the Up key.
/** @type {Form[]} */
const recent = [];

// Writes that had not been sent when a scan replaced the index. The next run
// of the screen finds their items again by path and sends them.
/** @type {{path: string, form: Form}[]} */
const carried = [];

/** The folders between an item and its share, outermost first. The share itself is not among them. */
function foldersAbove(/** @type {string} */ path) {
  const roots = (session.value && session.value.roots) || [];
  const root = roots.find((r) => path.startsWith(r === '/' ? '/' : r + '/'));
  if (!root) return [];
  const names = path.slice(root === '/' ? 1 : root.length + 1).split('/');
  const out = [];
  let at = (root === '/' ? '' : root) + '/' + names[0]; // the share
  for (const name of names.slice(1, -1)) {
    at += '/' + name;
    out.push(at);
  }
  return out;
}

/** The folder a path is in, or '' at the top of its share. */
function parentPath(/** @type {string} */ path) {
  const cut = path.lastIndexOf('/');
  return cut <= 0 ? '' : path.slice(0, cut);
}

// The keys of the review: one listener for the life of the page, acting on
// whichever item's form is on screen.
/** @type {{owner: object, form: Form, touched: boolean, display: {current: HTMLInputElement | null},
 *   props: {paused: boolean, canBack: boolean, onSubmit: (f: Form, touched: boolean) => void, onBack: () => void, onClimb: () => void, onDescend: () => void}} | null} */
let onScreen = null;
window.addEventListener('keydown', (e) => {
  const c = onScreen;
  if (!c || c.props.paused || document.querySelector('dialog[open]')) return;
  const mod = e.ctrlKey || e.metaKey;
  if (e.key === 'Enter' && mod) {
    c.props.onSubmit(c.form, c.touched);
  } else if ((e.key === 'z' || e.key === 'Z') && mod && !e.shiftKey && !c.touched && c.props.canBack) {
    // With something typed, Ctrl+Z is the browser's own undo. Once there
    // is nothing left to undo, it goes back an item.
    c.props.onBack();
  } else if (e.key === 'ArrowUp' && e.altKey) {
    c.props.onClimb();
  } else if (e.key === 'ArrowDown' && e.altKey) {
    c.props.onDescend();
  } else if (e.key === 'F2') {
    if (c.display.current) c.display.current.focus();
  } else {
    return;
  }
  e.preventDefault();
}, true);

/**
 * The form for one item. It owns what is typed and the keys of the review.
 * A different item is a different form, so nothing typed can leak across.
 * @param {{entry: Entry, initial: Form, readOnly: boolean, paused: boolean, canBack: boolean, lookingBack: boolean,
 *   onSubmit: (f: Form, touched: boolean) => void, onBack: () => void, onClimb: () => void, onDescend: () => void,
 *   onChange: (f: Form) => void}} props
 */
function ItemForm(props) {
  const [form, setForm] = useState(props.initial);
  const [recall, setRecall] = useState(-1);
  const note = useRef(/** @type {HTMLTextAreaElement | null} */ (null));
  const display = useRef(/** @type {HTMLInputElement | null} */ (null));
  const touched = !sameForm(form, props.initial);
  // The page's one key listener reads the form on screen from here. It is
  // set while drawing, not afterwards, so there is no moment after an item
  // appears in which a key would go nowhere.
  const me = useRef({});
  onScreen = { owner: me.current, form, touched, props, display };

  const change = (/** @type {Form} */ f) => {
    setForm(f);
    props.onChange(f);
  };

  // The cursor is in the note before the item is painted.
  useLayoutEffect(() => {
    if (note.current) note.current.focus();
    return () => {
      if (onScreen && onScreen.owner === me.current) onScreen = null;
    };
  }, []);

  // Up in an empty note brings back what was last saved; again for older.
  const onNoteKey = (/** @type {KeyboardEvent} */ e) => {
    if (e.altKey || e.ctrlKey || e.metaKey || !recent.length) return;
    if (e.key === 'ArrowUp' && (form.note === '' || recall >= 0) && recall < recent.length - 1) {
      setRecall(recall + 1);
      change(recent[recall + 1]);
      e.preventDefault();
    } else if (e.key === 'ArrowDown' && recall >= 0) {
      setRecall(recall - 1);
      change(recall > 0 ? recent[recall - 1] : { ...BLANK });
      e.preventDefault();
    }
  };

  const blank = isBlank(form);
  return html`
    <form class="stack" onSubmit=${(/** @type {Event} */ e) => { e.preventDefault(); props.onSubmit(form, touched); }}>
      <${Fields} form=${form} onChange=${change} disabled=${props.readOnly} id="rv" folder=${props.entry.kind === "dir"} noteRef=${note} displayRef=${display} onNoteKey=${onNoteKey} />
      <div class="rv-btns">
        <button class="btn primary" type="submit">
          ${props.readOnly ? 'Next' : props.lookingBack ? (touched ? 'Save the change' : 'Leave as it is') : blank ? 'Skip' : 'Save and next'}
          <span class="kbd">Ctrl+Enter</span></button>
        <button class="btn" type="button" disabled=${!props.canBack} onClick=${props.onBack}>Back <span class="kbd">Ctrl+Z</span></button>
      </div>
    </form>`;
}

/**
 * Where the queue's order is chosen, with a look at what the choice gives.
 * @param {{def: QueueDef, onClose: () => void}} props
 */
function Builder({ def, onClose }) {
  const [d, setD] = useState(def);
  const [shares, setShares] = useState(/** @type {{name: string}[]} */ ([]));
  const [look, setLook] = useState(/** @type {{q?: Queue, error?: string} | null} */ (null));
  const set = (/** @type {Partial<QueueDef>} */ change) => setD({ ...d, ...change });

  useEffect(() => {
    api('GET', '/api/shares').then((r) => setShares(r.shares)).catch(() => {});
  }, []);
  // The first count of a new kind of queue can take a second on a large
  // pool, so it waits for a pause and never holds up the controls.
  useEffect(() => {
    let live = true;
    setLook(null);
    const t = setTimeout(async () => {
      try {
        const q = await api('POST', '/api/queue', { ...(await resolve(d)), limit: 1 });
        if (live) setLook({ q });
      } catch (e) {
        if (live && !superseded(e)) setLook({ error: e instanceof Error ? e.message : String(e) });
      }
    }, 300);
    return () => { live = false; clearTimeout(t); };
  }, [JSON.stringify(d)]);

  const move = (/** @type {number} */ i, /** @type {number} */ by) => {
    const groups = [...d.groups];
    [groups[i], groups[i + by]] = [groups[i + by], groups[i]];
    set({ groups });
  };
  const unused = GROUP_KEYS.filter(([id]) => !d.groups.includes(id));
  const label = (/** @type {string} */ id) => (GROUP_KEYS.find(([k]) => k === id) || ['', id])[1];
  const toggleShare = (/** @type {string} */ name) =>
    set({ shares: d.shares.includes(name) ? d.shares.filter((s) => s !== name) : [...d.shares, name] });

  return html`
    <div class="panel builder" role="region" aria-label="Choose the order">
      <div>
        <h2 class="t">Start from</h2>
        <div class="presets">
          ${PRESETS.map((p) => html`<button class="chipb" type="button" key=${p.name} onClick=${() => set(p.def)}>${p.name}</button>`)}
        </div>
        <h2 class="t">What</h2>
        <div class="brow">
          <div class="seg" role="group" aria-label="Files or folders">
            ${[['file', 'Files'], ['dir', 'Folders'], ['', 'Both']].map(([k, text]) => html`
              <button type="button" key=${k} aria-pressed=${d.kind === k ? 'true' : 'false'} onClick=${() => set({ kind: /** @type {QueueDef['kind']} */ (k) })}>${text}</button>`)}
          </div>
          <label class="row">at least
            <select value=${String(d.min)} onChange=${(/** @type {Event} */ e) => set({ min: Number(/** @type {HTMLSelectElement} */ (e.target).value) })}>
              <option value="0">any size</option><option value="1000000">1 MB</option><option value="100000000">100 MB</option>
              <option value="1000000000">1 GB</option><option value="10000000000">10 GB</option>
            </select></label>
        </div>
        ${d.under ? html`<div class="brow"><span class="scope">Inside <span class="fs">${d.under}</span>
            <button type="button" aria-label="Remove this limit" onClick=${() => set({ under: '' })}>×</button></span></div>`
          : html`<div class="brow">${shares.map((s) => html`
              <label class="row" key=${s.name}><input type="checkbox" checked=${d.shares.includes(s.name)} onChange=${() => toggleShare(s.name)} />${s.name}</label>`)}
              <span class="hint">${d.shares.length ? '' : 'No share ticked means all of them.'}</span></div>`}
        <h2 class="t">Group by, in this order</h2>
        <div class="brow">
          ${d.groups.map((g, i) => html`
            <span class="keychip" key=${g}>${label(g)}
              <button type="button" aria-label=${'Move ' + label(g) + ' earlier'} disabled=${i === 0} onClick=${() => move(i, -1)}>${"<"}</button>
              <button type="button" aria-label=${'Move ' + label(g) + ' later'} disabled=${i === d.groups.length - 1} onClick=${() => move(i, 1)}>${">"}</button>
              <button type="button" aria-label=${'Remove ' + label(g)} onClick=${() => set({ groups: d.groups.filter((x) => x !== g) })}>×</button>
            </span>`)}
          ${unused.map(([id, text]) => html`<button class="chipb plan" type="button" key=${id} onClick=${() => set({ groups: [...d.groups, id] })}>+ ${text}</button>`)}
        </div>
        <h2 class="t">Inside each group</h2>
        <div class="brow">
          <div class="seg" role="group" aria-label="Order inside a group">
            ${ORDERS.map(([id, text]) => html`
              <button type="button" key=${id} aria-pressed=${d.order === id ? 'true' : 'false'} onClick=${() => set({ order: /** @type {QueueDef['order']} */ (id) })}>${text}</button>`)}
          </div>
        </div>
        <label class="row"><input type="checkbox" checked=${d.covered} onChange=${() => set({ covered: !d.covered })} />
          Leave out items inside folders that are already described</label>
      </div>
      <div class="bprev">
        <h2 class="t">What this gives</h2>
        <p>${sentence(d)}.</p>
        ${!look ? html`<p class="muted">Counting…</p>`
          : look.error ? html`<p class="err" role="alert">${look.error}</p>`
          : look.q && html`
            <p><b>${plural(look.q.remaining, 'item', 'items')}</b> to go, of ${count(look.q.total)} (${bytes(look.q.total_size)}),
              in ${plural(look.q.group_count, 'group', 'groups')}.</p>
            <ol>${look.q.groups.slice(0, 6).map((g, i) => html`<li key=${i}>${groupPhrase(g, d.kind)}: ${plural(g.remaining, 'item', 'items')}, ${bytes(g.size)}</li>`)}</ol>
            <p class="hint">Groups run from the one holding the most bytes to the least; age groups run oldest first.</p>`}
        <div class="rv-btns">
          <button class="btn primary" type="button" onClick=${() => { go('review', paramsFromDef(d)); onClose(); }}>Use this order</button>
          <button class="btn" type="button" onClick=${onClose}>Cancel</button>
        </div>
      </div>
    </div>`;
}

/**
 * One run through a queue, against one index.
 * @param {{def: QueueDef}} props
 */
function Session({ def }) {
  const readOnly = !!(session.value && session.value.read_only);
  const body = useRef(/** @type {Record<string, unknown> | null} */ (null));
  const [q, setQ] = useState(/** @type {Queue | null} */ (null));
  const [error, setError] = useState('');
  const [offset, setOffset] = useState(0);
  const [trail, setTrail] = useState(/** @type {Done[]} */ ([]));
  const [back, setBack] = useState(/** @type {number | null} */ (null));
  const [climb, setClimb] = useState(/** @type {{entry: Entry, form: Form}[]} */ ([]));
  const [line, setLine] = useState('');
  const [failed, setFailed] = useState(/** @type {Done | null} */ (null));
  const [halted, setHalted] = useState(false);
  const [confirmSkip, setConfirmSkip] = useState(false);
  const [building, setBuilding] = useState(false);
  const [, redraw] = useState(0);
  // Writes on their way, oldest first, and the items they are for.
  const outbox = useRef(/** @type {Done[]} */ ([]));
  const pumping = useRef(false);
  const failures = useRef(0);
  const aside = useRef(/** @type {Set<number>} */ (new Set()));
  const lastGroup = useRef('');
  const alive = useRef(true);
  useEffect(() => () => { alive.current = false; }, []);

  async function fetchQueue(/** @type {number} */ at = offset) {
    try {
      if (!body.current) body.current = await resolve(def);
      const next = /** @type {Queue} */ (await api('POST', '/api/queue', { ...body.current, limit: 20, offset: at }));
      if (!alive.current) return null;
      setQ(next);
      setError('');
      return next;
    } catch (e) {
      if (alive.current && !superseded(e)) setError(e instanceof Error ? e.message : String(e));
      return null;
    }
  }
  useEffect(() => {
    if (!prefixList.value) loadPrefixes();
    (async () => {
      for (const job of carried.splice(0)) {
        try {
          const { entry } = await api('GET', '/api/entries/lookup?path=' + encodeURIComponent(job.path));
          await api('PUT', `/api/entries/${entry.id}/annotation`, isBlank(job.form) ? {} : bodyOf(job.form));
        } catch { /* the item is gone, or the index moved again; the queue will offer it if it still exists */ }
      }
      fetchQueue();
    })();
  }, []);

  // Sends the writes one at a time, in the order they were made.
  async function pump() {
    if (pumping.current) return;
    pumping.current = true;
    while (outbox.current.length && alive.current) {
      const job = outbox.current[0];
      try {
        await api('PUT', `/api/entries/${job.entry.id}/annotation`, isBlank(job.form) ? {} : bodyOf(job.form));
        job.outcome = isBlank(job.form) ? 'skipped' : 'saved';
        failures.current = 0;
      } catch (e) {
        if (superseded(e)) {
          // A scan replaced the index. This run ends; the next one sends what is left.
          for (const j of outbox.current) carried.push({ path: j.entry.path || '', form: j.form });
          outbox.current = [];
          break;
        }
        job.outcome = 'failed';
        job.error = e instanceof Error ? e.message : String(e);
        job.gone = e instanceof ApiError && e.code === 'gone';
        failures.current++;
        if (alive.current) {
          setFailed(job);
          if (failures.current >= 3) setHalted(true);
        }
      }
      outbox.current.shift();
      if (job.outcome === 'failed') break; // the user decides what happens to it before more are sent
      if (alive.current) await fetchQueue();
    }
    pumping.current = false;
    if (alive.current) redraw((n) => n + 1);
  }

  const waiting = new Set(outbox.current.map((j) => j.entry.id));
  if (failed) waiting.add(failed.entry.id);
  const items = q ? q.items.filter((it) => !waiting.has(it.id) && !aside.current.has(it.id)) : [];
  const head = items[0] || null;
  const group = q && q.groups[0] ? q.groups[0] : null;
  // The group in words: "video files in media".
  const label = group ? groupPhrase(group, def.kind) : '';
  const Label = label ? label[0].toUpperCase() + label.slice(1) : '';

  // Say so when one group ends and the next begins.
  useEffect(() => {
    if (label && lastGroup.current && lastGroup.current !== label && group) {
      setLine(`Finished ${lastGroup.current}. Now: ${label}, ${plural(group.remaining, 'item', 'items')}.`);
    }
    if (label) lastGroup.current = label;
  }, [label]);

  const lookingBack = back !== null;
  // Nothing can be recorded for an item in a share that is mapped read-only.
  const locked = !!head && lockedShares.value.has(head.share);
  const folder = climb.length ? climb[climb.length - 1] : null;
  const target = lookingBack ? trail[/** @type {number} */ (back)].entry : folder ? folder.entry : head;
  const initial = lookingBack ? trail[/** @type {number} */ (back)].form : folder ? folder.form
    : head && draft && draft.path === head.path ? draft.form : BLANK;

  function submit(/** @type {Form} */ form, /** @type {boolean} */ touched) {
    if (!target || halted) return;
    setLine('');
    if (readOnly || locked) {
      // Nothing is recorded; the queue is only walked through.
      const at = offset + 1;
      setOffset(at);
      fetchQueue(at);
      return;
    }
    if (lookingBack) {
      const was = trail[/** @type {number} */ (back)];
      setBack(null);
      if (touched) send({ entry: was.entry, form, outcome: 'saving' }, /** @type {number} */ (back));
      return;
    }
    if (folder) {
      if (isBlank(form)) {
        setLine('Nothing was typed, so the folder was left as it was.');
        setClimb([]);
        return;
      }
      setClimb([]);
      const before = q ? q.remaining : 0;
      send({ entry: folder.entry, form, outcome: 'saving' }).then(async () => {
        const after = await fetchQueue();
        if (after && alive.current && def.covered && before - after.remaining > 0) {
          setLine(`Described the folder ${folder.entry.name}. ${plural(before - after.remaining, 'item', 'items')} inside it left the queue.`);
        }
      });
      return;
    }
    draft = null;
    if (!isBlank(form)) recent.unshift(form);
    send({ entry: target, form, outcome: 'saving' });
  }

  /** Queues a write and shows the item as done. `replace` is the place in the trail it rewrites. */
  function send(/** @type {Done} */ job, /** @type {number} */ replace = -1) {
    setTrail((t) => (replace >= 0 ? t.map((d, i) => (i === replace ? job : d)) : [...t, job]));
    outbox.current.push(job);
    redraw((n) => n + 1);
    return pump();
  }

  // What to do with a write that did not go through.
  const retry = () => {
    if (!failed) return;
    failed.outcome = 'saving';
    outbox.current.unshift(failed);
    setFailed(null);
    setHalted(false);
    pump();
  };
  const edit = () => {
    if (!failed) return;
    const at = trail.indexOf(failed);
    setFailed(null);
    setHalted(false);
    if (at >= 0) setBack(at);
    pump();
  };
  const discard = (/** @type {boolean} */ skipIt) => {
    if (!failed) return;
    const job = failed;
    setFailed(null);
    setHalted(false);
    failures.current = 0;
    if (skipIt) {
      send({ entry: job.entry, form: { ...BLANK }, outcome: 'saving' }, trail.indexOf(job));
    } else {
      // Not skipped and not saved: it stays out of the way for this visit.
      if (job.gone) aside.current.add(job.entry.id);
      setTrail((t) => t.filter((d) => d !== job));
      pump();
      fetchQueue();
    }
  };

  async function climbTo(/** @type {string} */ path) {
    try {
      const { entry } = await api('GET', '/api/entries/lookup?path=' + encodeURIComponent(path));
      if (entry.kind !== 'dir' || entry.share === 0 || entry.share === entry.id) {
        say('A share itself is not described this way; open it in Space to add a note to it.');
        return;
      }
      const full = await api('GET', `/api/entries/${entry.id}`);
      setClimb((c) => [...c, { entry: full.entry, form: formFrom(full.annotation) }]);
      setLine(`Describing the folder instead of ${head ? head.name : 'the item'}. Saving it takes what is inside out of the queue.`);
    } catch (e) {
      if (!superseded(e)) say(e instanceof Error ? e.message : String(e));
    }
  }
  const climbUp = () => {
    if (lookingBack || readOnly || !target || !target.path) return;
    const up = parentPath(target.path);
    if (up) climbTo(up);
  };
  const descend = () => {
    if (climb.length) setClimb(climb.slice(0, -1));
  };
  const goBack = () => {
    if (readOnly) {
      if (offset > 0) {
        setOffset(offset - 1);
        fetchQueue(offset - 1);
      }
      return;
    }
    if (climb.length) setClimb([]);
    else if (lookingBack) setBack(/** @type {number} */ (back) > 0 ? /** @type {number} */ (back) - 1 : back);
    else if (trail.length) setBack(trail.length - 1);
  };

  async function skipGroup() {
    if (!group || !body.current) return;
    setConfirmSkip(false);
    try {
      const r = await api('POST', '/api/queue/skip', { ...body.current, group: group.values.map((v) => v.value) });
      say(`Skipped ${count(r.skipped)} ${label}.`);
      await fetchQueue();
    } catch (e) {
      if (!superseded(e)) say(e instanceof Error ? e.message : String(e));
    }
  }

  const top = html`
    <div class="rv-top">
      <div class="q"><b>Reviewing:</b> <span>${sentence(def)}${def.covered ? ', leaving out what is inside described folders' : ''}.</span></div>
      <div class="seg" role="group" aria-label="Review files or folders">
        ${[["file", "Files"], ["dir", "Folders"], ["", "Both"]].map(([k, text]) => html`
          <button type="button" key=${k} aria-pressed=${def.kind === k ? "true" : "false"}
            onClick=${() => go("review", paramsFromDef({ ...def, kind: /** @type {QueueDef["kind"]} */ (k) }))}>${text}</button>`)}
      </div>
      <button class="btn small" type="button" onClick=${() => setBuilding(!building)}>${building ? 'Close' : 'Change order'}</button>
    </div>
    ${building && html`<${Builder} def=${def} onClose=${() => setBuilding(false)} />`}`;

  if (error) {
    return html`${top}<div class="panel pad stack" role="alert"><p><b>The queue could not be loaded.</b> ${error}</p>
      <div><button class="btn" type="button" onClick=${() => fetchQueue()}>Try again</button></div></div>`;
  }
  if (!q) return html`${top}<p class="muted">Loading…</p>`;

  const sending = outbox.current.length;
  const leftInGroup = group ? Math.max(0, group.remaining - (readOnly ? 0 : sending)) : 0;
  const leftInAll = Math.max(0, q.remaining - (readOnly ? 0 : sending));
  if (!target && !failed) {
    if (sending) return html`${top}<p class="muted">Saving the last ones…</p>`;
    const saved = trail.filter((d) => d.outcome === 'saved').length;
    const skipped = trail.filter((d) => d.outcome === 'skipped').length;
    return html`${top}
      <div class="panel done-panel">
        <h2>${q.total ? 'This queue is finished' : 'Nothing matches this queue'}</h2>
        ${q.total > 0 && html`<p>${count(q.total)} items, ${bytes(q.total_size)}. In this visit you described ${count(saved)} and skipped ${count(skipped)}.</p>`}
        <div class="rv-btns">
          <button class="btn primary" type="button" onClick=${() => setBuilding(true)}>Build another queue</button>
          <a class="btn" href=${href('find', { state: 'skipped' })} onClick=${(/** @type {Event} */ e) => { e.preventDefault(); go('find', { state: 'skipped' }); }}>Look at skipped items</a>
          <a class="btn" href=${href('space')} onClick=${(/** @type {Event} */ e) => { e.preventDefault(); go('space'); }}>Back to Space</a>
        </div>
      </div>`;
  }

  const shown = trail.slice(-3);
  // The folders above the item, inside its share: each a way to describe that folder instead.
  const steps = target && target.path ? foldersAbove(target.path) : [];

  return html`${top}
    <div class="rv-grid">
      <div class="rv-queue">
        <div class="panel gnow">
          <div class="muted">Now going through</div>
          <b>${Label}</b>
          <div class="cnt">${count(leftInGroup)}</div>
          <div class="muted">left of ${count(group ? group.total : 0)} in this group</div>
          <div class="meter"><i style=${{ width: group && group.total ? `${(100 * (group.total - leftInGroup)) / group.total}%` : '0%' }}></i></div>
          <div class="muted">Group ${count(q.group_position)} of ${count(q.group_total)}. ${count(leftInAll)} left in the whole queue.</div>
          ${!readOnly && !lookingBack && !folder && (confirmSkip
            ? html`<div class="note-line warn">Skip all ${count(leftInGroup)} ${label} that are left? They are marked as skipped, with nothing recorded.
                <div class="rv-btns"><button class="btn small" type="button" onClick=${skipGroup}>Yes, skip them</button>
                <button class="btn small" type="button" onClick=${() => setConfirmSkip(false)}>No</button></div></div>`
            : html`<div><button class="btn small" type="button" disabled=${leftInGroup === 0} onClick=${() => setConfirmSkip(true)}>Skip the ${count(leftInGroup)} left</button>
                <div class="hint">That is every remaining one of the ${label}.</div></div>`)}
        </div>
        ${q.groups.length > 1 && html`
          <div class="panel">
            <h2 class="t">Next groups</h2>
            <div class="glist">${q.groups.slice(1, 9).map((g, i) => html`
              <div class="g" key=${i}><span>${groupPhrase(g, def.kind)}</span><span class="n">${count(g.remaining)}</span>
                <div class="meter"><i style=${{ width: g.total ? `${(100 * (g.total - g.remaining)) / g.total}%` : '0%' }}></i></div></div>`)}
            </div>
          </div>`}
        <div class="panel">
          <h2 class="t">Done, now, next</h2>
          <div class="trail">
            ${shown.map((d, i) => html`
              <div class="it done" key=${'d' + i}><span class="fs">${d.entry.name}</span>
                <span class=${'r' + (d.outcome === 'failed' ? ' bad' : '')}>${d.outcome === 'failed' ? 'not saved' : d.outcome}</span></div>`)}
            ${target && html`<div class="it cur"><span class="fs">${target.name}</span><span class="r">now</span></div>`}
            ${!lookingBack && !folder && items.slice(1, 6).map((it) => html`<div class="it" key=${it.id}><span class="fs">${it.name}</span><span class="r">${bytes(it.size)}</span></div>`)}
          </div>
        </div>
      </div>

      <div class="panel rv-form">
        ${failed && html`
          <div class="banner crit" role="alert"><div><span class="st"><i aria-hidden="true">!</i>Not saved: ${failed.entry.name}.</span>
            <span> ${failed.gone ? 'It is no longer where the last scan found it.' : failed.error} Your text is kept.</span>
            <div class="rv-btns">
              ${!failed.gone && html`<button class="btn small" type="button" onClick=${retry}>Retry</button>`}
              <button class="btn small" type="button" onClick=${edit}>Edit</button>
              ${failed.gone && html`<button class="btn small" type="button" onClick=${() => discard(true)}>Skip it</button>`}
              <button class="btn small" type="button" onClick=${() => discard(false)}>Discard</button>
            </div></div></div>`}
        ${halted && html`<div class="note-line warn" role="alert">Three saves in a row did not go through, so the review has stopped moving on. Deal with the one above to carry on.</div>`}
        ${readOnly && html`<div class="note-line">Read-only mode. You can look through the queue; nothing is recorded.</div>`}
        ${!readOnly && locked && html`<div class="note-line warn">This share is mapped into the container read-only, so nothing can be recorded for this item. Ctrl+Enter moves on.</div>`}
        ${lookingBack && html`<div class="note-line">Looking back at an item from this visit. Change it and save, or leave it as it is.</div>`}
        ${line && html`<div class="note-line" role="status">${line}</div>`}
        ${target && html`
          <div>
            <h2 class="fs">${target.name}</h2>
            <div class="rv-path fs">
              ${steps.map((p) => html`<button type="button" key=${p} title="Describe this folder instead" disabled=${readOnly || lookingBack} onClick=${() => climbTo(p)}>${p.slice(p.lastIndexOf('/') + 1)}</button><span>/</span>`)}
            </div>
            ${!readOnly && !locked && !lookingBack && !folder && steps.length > 0 && html`
              <div><button class="btn small" type="button" onClick=${climbUp}>Describe the folder instead <span class="kbd">Alt+Up</span></button></div>`}
            ${folder && html`<div><button class="btn small" type="button" onClick=${descend}>Back to the item <span class="kbd">Alt+Down</span></button></div>`}
            <div class="rv-facts">
              <span>${target.kind === 'dir' ? `Folder, ${plural(target.files, 'file', 'files')}` : target.type}</span>
              <span>${bytes(target.size)}${target.disk !== target.size ? ` (${bytes(target.disk)} on disk)` : ''}</span>
              <span>changed ${ago(target.kind === 'dir' && target.max_mtime ? target.max_mtime : target.mtime) || 'at an unknown time'}</span>
            </div>
          </div>
          <${ItemForm} key=${`${target.id}:${lookingBack ? 'b' + back : folder ? 'f' : 'h'}`} entry=${target} initial=${initial}
            readOnly=${readOnly || (locked && !lookingBack && !folder)} paused=${building || halted} canBack=${readOnly ? offset > 0 : (trail.length > 0 && back !== 0) || climb.length > 0}
            lookingBack=${lookingBack} onSubmit=${submit} onBack=${goBack} onClimb=${climbUp} onDescend=${descend}
            onChange=${(/** @type {Form} */ f) => { if (!lookingBack && !folder && head) draft = { path: head.path || '', form: f }; }} />`}
        <div class="rv-keys">
          <span><span class="kbd">Tab</span> next field</span>
          <span><span class="kbd">Ctrl+Enter</span> save and next; skips when nothing is typed</span>
          <span><span class="kbd">Ctrl+Z</span> back, once nothing is left to undo</span>
          <span><span class="kbd">Alt+Up</span> describe the folder instead</span>
          <span><span class="kbd">Up</span> in an empty note: reuse the last one</span>
          <span><span class="kbd">F2</span> display name</span>
        </div>
      </div>

      <div class="panel rv-preview">
        ${target && html`
          <div class="pv-head"><span class="fs">${target.path}</span></div>
          <div class="pv-body"><${Preview} entry=${target} large=${true} /></div>`}
      </div>
    </div>`;
}

export function Review() {
  const { params } = route.value;
  const index = indexId.value;
  const bare = Object.keys(params).length === 0;
  // With no queue in the address, carry on with the last one used here.
  useEffect(() => {
    if (bare) go('review', lastParams() || paramsFromDef(DEFAULT), true);
  }, [bare]);
  useEffect(() => {
    if (!bare) rememberParams(params);
  }, [JSON.stringify(params)]);
  if (bare) return null;
  const def = defFromParams(params);
  // A different queue, or a new index, is a new run: entry IDs do not carry over.
  return html`<${Session} key=${JSON.stringify(def) + '|' + index} def=${def} />`;
}
