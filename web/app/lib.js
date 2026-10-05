// The libraries the interface is built on, gathered in one place so that
// every other file imports from here and a library can be replaced here.
//
// Importing signals before anything renders matters: it is what makes a
// component redraw when a signal it read changes.
import { h, render } from '../lib/preact.js';
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from '../lib/hooks.js';
import htm from '../lib/htm.js';
import { batch, computed, effect, signal } from '../lib/signals.js';

/** Templates: html`<div class="x">${child}</div>`. Values are inserted as text or properties, never parsed as markup. */
export const html = htm.bind(h);

export { h, render, useEffect, useLayoutEffect, useMemo, useRef, useState, batch, computed, effect, signal };
