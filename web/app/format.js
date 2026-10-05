// How numbers, sizes and times are written.
import { prefs } from './state.js';

const DECIMAL = ['B', 'kB', 'MB', 'GB', 'TB', 'PB'];
const BINARY = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

/** A size in the units the user chose: 3.8 TB, or 3.5 TiB. */
export function bytes(/** @type {number} */ n) {
  const binary = prefs.value.units === 'binary';
  const step = binary ? 1024 : 1000;
  const names = binary ? BINARY : DECIMAL;
  let i = 0;
  let v = n;
  while (v >= step && i < names.length - 1) {
    v /= step;
    i++;
  }
  const digits = i === 0 || v >= 100 ? 0 : v >= 10 ? 1 : 2;
  return `${v.toFixed(digits)} ${names[i]}`;
}

export function count(/** @type {number} */ n) {
  return n.toLocaleString('en-US');
}

/** "1 file", "2 files". */
export function plural(/** @type {number} */ n, /** @type {string} */ one, /** @type {string} */ many) {
  return `${count(n)} ${n === 1 ? one : many}`;
}

function pad(/** @type {number} */ n) {
  return String(n).padStart(2, '0');
}

/** A date as 2026-10-05, in the browser's time zone. */
export function day(/** @type {Date} */ d) {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** A moment as "today 03:04", "yesterday 03:04" or "2026-10-01 03:04". */
export function moment(/** @type {Date} */ d) {
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  const now = new Date();
  const yesterday = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1);
  if (day(d) === day(now)) return `today ${time}`;
  if (day(d) === day(yesterday)) return `yesterday ${time}`;
  return `${day(d)} ${time}`;
}

/** How long ago a time on a file was, from Unix seconds: "3 days ago", "2 years ago". Empty for no time. */
export function ago(/** @type {number} */ unix) {
  if (!unix) return '';
  const days = Math.floor((Date.now() / 1000 - unix) / 86400);
  if (days < 0) return 'in the future';
  if (days < 1) return 'today';
  if (days < 2) return 'yesterday';
  if (days < 60) return `${days} days ago`;
  if (days < 730) return `${Math.floor(days / 30.44)} months ago`;
  return `${Math.floor(days / 365.25)} years ago`;
}

/** The date of a time on a file, from Unix seconds. */
export function date(/** @type {number} */ unix) {
  return unix ? day(new Date(unix * 1000)) : '';
}

/** A length of time: "45 s", "3 min 20 s", "1 h 5 min". */
export function duration(/** @type {number} */ seconds) {
  const s = Math.round(seconds);
  if (s < 60) return `${s} s`;
  if (s < 3600) return `${Math.floor(s / 60)} min ${s % 60} s`;
  return `${Math.floor(s / 3600)} h ${Math.floor((s % 3600) / 60)} min`;
}
