// Where the page starts.
import { html, render } from './lib.js';
import { App } from './shell.js';

const root = document.getElementById('app');
if (root) render(html`<${App} />`, root);
