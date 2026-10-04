#!/usr/bin/env node
'use strict';
// ThinkBot sandbox desktop. Speaks RFB 3.8 on stdin/stdout and reads this bot's
// own X display (the same Xvfb Chromium uses). One process, one display.
// Host mode never scans other processes' displays.
//
// The picture is the real X size. Updates are raw rectangles of 64px tiles that
// changed, so a quiet screen is not a full 1280x800 frame.
// Clipboard bytes are UTF-8 on this private stream (not Latin-1). CJK keysyms
// are not on the Xvfb keymap, so typed CJK cannot be keycodes; paste uses the
// CLIPBOARD selection instead.

const net = require('net');
const fs = require('fs');
const { spawn } = require('child_process');

const SCREEN_W = 1280;
const SCREEN_H = 800;
const TILE = 64;
const MODE = process.env.THINKBOT_DESKTOP_MODE || 'container';
const MARKER = process.env.THINKBOT_DESKTOP_MARKER === '1';
const CLIP_ACK = process.env.THINKBOT_DESKTOP_CLIP_ACK === '1';

let xvfb = null;
function log(...a) {
  if (process.env.THINKBOT_DESKTOP_DEBUG) process.stderr.write(a.join(' ') + '\n');
}
function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }

function displayNum(disp) {
  const n = String(disp || '').replace(/^:/, '').split('.')[0];
  if (!/^\d+$/.test(n)) throw new Error('bad display');
  return n;
}

function readEnviron(pid) {
  try {
    const raw = fs.readFileSync('/proc/' + pid + '/environ');
    const out = {};
    for (const part of raw.toString('utf8').split('\0')) {
      const i = part.indexOf('=');
      if (i > 0) out[part.slice(0, i)] = part.slice(i + 1);
    }
    return out;
  } catch (e) { return null; }
}

function findBrowserDisplay() {
  let ents = [];
  try { ents = fs.readdirSync('/proc'); } catch (e) { return null; }
  for (const pid of ents) {
    if (!/^\d+$/.test(pid)) continue;
    let cmd = '';
    try { cmd = fs.readFileSync('/proc/' + pid + '/cmdline').toString('utf8'); } catch (e) { continue; }
    if (!cmd.includes('thinkbot-browser-mcp') && !cmd.includes('thinkbot-browser-launch')) continue;
    const env = readEnviron(pid);
    if (env && env.DISPLAY) return { display: env.DISPLAY, xauthority: env.XAUTHORITY || '' };
  }
  return null;
}

function readCookie(xauthority, display) {
  if (!xauthority) return null;
  let buf;
  try { buf = fs.readFileSync(xauthority); } catch (e) { return null; }
  const want = displayNum(display);
  let o = 0;
  while (o + 4 <= buf.length) {
    o += 2;
    const alen = buf.readUInt16BE(o); o += 2;
    if (o + alen + 2 > buf.length) break;
    o += alen;
    const nlen = buf.readUInt16BE(o); o += 2;
    if (o + nlen + 2 > buf.length) break;
    const number = buf.slice(o, o + nlen).toString('utf8'); o += nlen;
    const namelen = buf.readUInt16BE(o); o += 2;
    if (o + namelen + 2 > buf.length) break;
    o += namelen;
    const dlen = buf.readUInt16BE(o); o += 2;
    if (o + dlen > buf.length) break;
    const data = buf.slice(o, o + dlen); o += dlen;
    if (number === want && data.length === 16) return data;
  }
  return null;
}

async function ensureDisplay() {
  if (MODE === 'container') {
    const found = findBrowserDisplay();
    if (found) return { display: found.display, cookie: readCookie(found.xauthority, found.display) };
  }
  const display = MODE === 'host' ? (process.env.THINKBOT_DESKTOP_DISPLAY || ':99') : ':99';
  const num = displayNum(display);
  const sock = '/tmp/.X11-unix/X' + num;
  if (!fs.existsSync(sock)) {
    try { fs.mkdirSync('/tmp/.X11-unix', { recursive: true }); } catch (e) {}
    xvfb = spawn('Xvfb', [':' + num, '-screen', '0', SCREEN_W + 'x' + SCREEN_H + 'x24', '-ac', '-nolisten', 'tcp'], {
      stdio: 'ignore',
    });
    for (let i = 0; i < 50 && !fs.existsSync(sock); i++) await sleep(20);
    if (!fs.existsSync(sock)) throw new Error('Xvfb did not start on ' + display);
  }
  return { display, cookie: null };
}

function pad4(n) { return (n + 3) & ~3; }

class XConn {
  constructor(sock) {
    this.sock = sock;
    this.buf = Buffer.alloc(0);
    this.waiters = [];
    this.seq = 0;
    this.pendingEvents = [];
    sock.on('data', d => { this.buf = Buffer.concat([this.buf, d]); this.pump(); });
    sock.on('error', e => this.fail(e));
    this.closed = new Promise((_, rej) => sock.on('close', () => rej(new Error('x closed'))));
  }
  fail(e) { while (this.waiters.length) this.waiters.shift().rej(e); }
  pump() {
    while (this.waiters.length && this.buf.length >= this.waiters[0].need) {
      const w = this.waiters.shift();
      const out = this.buf.subarray(0, w.need);
      this.buf = this.buf.subarray(w.need);
      w.res(out);
    }
  }
  need(n) {
    return new Promise((res, rej) => { this.waiters.push({ need: n, res, rej }); this.pump(); });
  }
  send(req) {
    this.seq = (this.seq + 1) & 0xffff;
    this.sock.write(req);
  }
  async handshake(cookie) {
    const name = cookie ? Buffer.from('MIT-MAGIC-COOKIE-1') : Buffer.alloc(0);
    const data = cookie || Buffer.alloc(0);
    const hdr = Buffer.alloc(12);
    hdr[0] = 0x6c;
    hdr.writeUInt16LE(11, 2);
    hdr.writeUInt16LE(0, 4);
    hdr.writeUInt16LE(name.length, 6);
    hdr.writeUInt16LE(data.length, 8);
    const np = Buffer.alloc(pad4(name.length)); name.copy(np);
    const dp = Buffer.alloc(pad4(data.length)); data.copy(dp);
    this.sock.write(Buffer.concat([hdr, np, dp]));
    const head = await this.need(8);
    if (head[0] !== 1) throw new Error('x auth failed');
    const extra = head.readUInt16LE(6) * 4;
    const body = extra ? await this.need(extra) : Buffer.alloc(0);
    this.parseSetup(body);
  }
  parseSetup(b) {
    this.minKey = b[26];
    this.maxKey = b[27];
    let o = 32 + pad4(b.readUInt16LE(16));
    const nformats = b[21];
    o += nformats * 8;
    this.root = b.readUInt32LE(o);
    this.width = b.readUInt16LE(o + 20);
    this.height = b.readUInt16LE(o + 22);
    this.depth = b[o + 38] || 24;
    this.ridBase = b.readUInt32LE(4);
    this.ridMask = b.readUInt32LE(8);
    this.nextRid = this.ridBase;
  }
  allocId() {
    const id = this.nextRid;
    this.nextRid = (this.nextRid + 1) & 0xffffffff;
    return id;
  }
  takeEvent() {
    if (this.buf.length < 32) return null;
    const kind = this.buf[0];
    if (kind === 1 || kind === 0) return null;
    const ev = Buffer.from(this.buf.subarray(0, 32));
    this.buf = this.buf.subarray(32);
    return ev;
  }
  async round(req, extraOf) {
    this.send(req);
    for (;;) {
      const head = await this.need(32);
      const kind = head[0];
      if (kind === 0) throw new Error('x error ' + head[1] + ' major ' + head[10]);
      if (kind !== 1) {
        const ev = Buffer.from(head);
        if ((ev[0] & 0x7f) === 30) await this.onSelectionRequest(ev);
        if ((ev[0] & 0x7f) === 29) this.clipOwned = false;
        this.pendingEvents.push(ev);
        continue;
      }
      const extra = extraOf ? extraOf(head) : head.readUInt32LE(4) * 4;
      const rest = extra ? await this.need(extra) : Buffer.alloc(0);
      return { head, rest };
    }
  }
  async waitEvent(pred, ms) {
    const deadline = Date.now() + ms;
    while (Date.now() < deadline) {
      let ev;
      while ((ev = this.pendingEvents.shift())) {
        if (pred(ev)) return ev;
      }
      ev = this.takeEvent();
      if (ev) {
        if ((ev[0] & 0x7f) === 30) await this.onSelectionRequest(ev);
        if ((ev[0] & 0x7f) === 29) this.clipOwned = false;
        if (pred(ev)) return ev;
        continue;
      }
      const left = deadline - Date.now();
      if (left <= 0) break;
      await new Promise(res => {
        const on = () => { clearTimeout(t); res(); };
        const t = setTimeout(() => { this.sock.removeListener('data', on); res(); }, Math.min(left, 40));
        this.sock.on('data', on);
      });
    }
    return null;
  }
  async queryExtension(name) {
    const nb = Buffer.from(name);
    const len = 2 + Math.ceil(nb.length / 4);
    const req = Buffer.alloc(len * 4);
    req[0] = 98;
    req.writeUInt16LE(len, 2);
    req.writeUInt16LE(nb.length, 4);
    nb.copy(req, 8);
    const { head } = await this.round(req);
    return { present: head[8] === 1, major: head[9] };
  }
  async intern(name) {
    const nb = Buffer.from(name);
    const len = 2 + Math.ceil(nb.length / 4);
    const req = Buffer.alloc(len * 4);
    req[0] = 16;
    req.writeUInt16LE(len, 2);
    req.writeUInt16LE(nb.length, 4);
    nb.copy(req, 8);
    const { head } = await this.round(req, () => 0);
    const atom = head.readUInt32LE(8);
    if (!atom) throw new Error('no atom ' + name);
    return atom;
  }
  async setupClipboard() {
    const names = ['CLIPBOARD', 'UTF8_STRING', 'STRING', 'TARGETS', 'ATOM', 'INCR', 'THINKBOT_CLIP'];
    this.atom = {};
    for (const n of names) this.atom[n] = await this.intern(n);
    this.win = this.allocId();
    const req = Buffer.alloc(36);
    req[0] = 1;
    req.writeUInt16LE(9, 2);
    req.writeUInt32LE(this.win, 4);
    req.writeUInt32LE(this.root, 8);
    req.writeUInt16LE(1, 16);
    req.writeUInt16LE(1, 18);
    req.writeUInt16LE(1, 22);
    req.writeUInt32LE(0x800, 28);
    req.writeUInt32LE(0x400000, 32);
    this.send(req);
    this.clipText = '';
    this.clipOwned = false;
  }
  changeProperty(win, prop, type, data, format) {
    const nbytes = data.length;
    const units = format === 32 ? nbytes / 4 : format === 16 ? nbytes / 2 : nbytes;
    const pad = pad4(nbytes);
    const len = 6 + pad / 4;
    const req = Buffer.alloc(len * 4);
    req[0] = 18;
    req.writeUInt16LE(len, 2);
    req.writeUInt32LE(win, 4);
    req.writeUInt32LE(prop, 8);
    req.writeUInt32LE(type, 12);
    req[16] = format;
    req.writeUInt32LE(units, 20);
    data.copy(req, 24);
    this.send(req);
  }
  deleteProperty(win, prop) {
    const req = Buffer.alloc(12);
    req[0] = 19;
    req.writeUInt16LE(3, 2);
    req.writeUInt32LE(win, 4);
    req.writeUInt32LE(prop, 8);
    this.send(req);
  }
  async getProperty(win, prop) {
    const req = Buffer.alloc(24);
    req[0] = 20;
    req[1] = 1;
    req.writeUInt16LE(6, 2);
    req.writeUInt32LE(win, 4);
    req.writeUInt32LE(prop, 8);
    req.writeUInt32LE(65536, 20);
    const { head, rest } = await this.round(req);
    const format = head[1];
    const type = head.readUInt32LE(8);
    const valueLen = head.readUInt32LE(16);
    if (!format || !valueLen) return { type, data: Buffer.alloc(0) };
    const nbytes = format === 8 ? valueLen : format === 16 ? valueLen * 2 : valueLen * 4;
    return { type, data: rest.subarray(0, Math.min(nbytes, rest.length)) };
  }
  async onSelectionRequest(ev) {
    if (!this.atom) return;
    const time = ev.readUInt32LE(4);
    const requestor = ev.readUInt32LE(12);
    const selection = ev.readUInt32LE(16);
    const target = ev.readUInt32LE(20);
    let property = ev.readUInt32LE(24);
    if (!property) property = this.atom.THINKBOT_CLIP;
    let propOut = 0;
    const text = Buffer.from(this.clipText || '', 'utf8');
    if (target === this.atom.TARGETS) {
      const atoms = Buffer.alloc(12);
      atoms.writeUInt32LE(this.atom.TARGETS, 0);
      atoms.writeUInt32LE(this.atom.UTF8_STRING, 4);
      atoms.writeUInt32LE(this.atom.STRING, 8);
      this.changeProperty(requestor, property, this.atom.ATOM, atoms, 32);
      propOut = property;
    } else if (target === this.atom.UTF8_STRING || target === this.atom.STRING) {
      this.changeProperty(requestor, property, target, text, 8);
      propOut = property;
    }
    const notify = Buffer.alloc(32);
    notify[0] = 31;
    notify.writeUInt32LE(time, 4);
    notify.writeUInt32LE(requestor, 8);
    notify.writeUInt32LE(selection, 12);
    notify.writeUInt32LE(target, 16);
    notify.writeUInt32LE(propOut, 20);
    const req = Buffer.alloc(44);
    req[0] = 25;
    req.writeUInt16LE(11, 2);
    req.writeUInt32LE(requestor, 4);
    notify.copy(req, 12);
    this.send(req);
  }
  async setClipboard(text) {
    if (!this.atom) return;
    const buf = Buffer.from(String(text), 'utf8');
    if (buf.length > 256 * 1024) return;
    this.clipText = String(text);
    this.changeProperty(this.win, this.atom.THINKBOT_CLIP, this.atom.UTF8_STRING, buf, 8);
    const req = Buffer.alloc(16);
    req[0] = 22;
    req.writeUInt16LE(4, 2);
    req.writeUInt32LE(this.win, 4);
    req.writeUInt32LE(this.atom.CLIPBOARD, 8);
    this.send(req);
    this.clipOwned = true;
  }
  async convertSelection(target) {
    const req = Buffer.alloc(24);
    req[0] = 24;
    req.writeUInt16LE(6, 2);
    req.writeUInt32LE(this.win, 4);
    req.writeUInt32LE(this.atom.CLIPBOARD, 8);
    req.writeUInt32LE(target, 12);
    req.writeUInt32LE(this.atom.THINKBOT_CLIP, 16);
    this.send(req);
    const ev = await this.waitEvent(e => (e[0] & 0x7f) === 31, 300);
    if (!ev) return null;
    const prop = ev.readUInt32LE(20);
    if (!prop) return '';
    let got = await this.getProperty(this.win, prop);
    if (got.type === this.atom.INCR) {
      const chunks = [];
      let total = 0;
      for (;;) {
        const pn = await this.waitEvent(e => (e[0] & 0x7f) === 28, 400);
        if (!pn) break;
        got = await this.getProperty(this.win, prop);
        if (!got.data.length) break;
        total += got.data.length;
        if (total > 1024 * 1024) break;
        chunks.push(Buffer.from(got.data));
      }
      return Buffer.concat(chunks).toString('utf8');
    }
    return got.data.toString('utf8');
  }
  async readClipboard() {
    if (!this.atom) return null;
    const utf = await this.convertSelection(this.atom.UTF8_STRING);
    if (utf) return utf;
    const latin = await this.convertSelection(this.atom.STRING);
    return latin == null ? utf : latin;
  }
  async getImage(w, h) {
    const req = Buffer.alloc(20);
    req[0] = 73;
    req[1] = 2;
    req.writeUInt16LE(5, 2);
    req.writeUInt32LE(this.root, 4);
    req.writeUInt16LE(w, 12);
    req.writeUInt16LE(h, 14);
    req.writeUInt32LE(0xffffffff, 16);
    const { rest } = await this.round(req, head => head.readUInt32LE(4) * 4);
    return rest;
  }
  async keymap() {
    const count = this.maxKey - this.minKey + 1;
    const req = Buffer.alloc(8);
    req[0] = 101;
    req.writeUInt16LE(2, 2);
    req[4] = this.minKey;
    req[5] = count;
    const { head, rest } = await this.round(req, h => h.readUInt32LE(4) * 4);
    const per = head[1];
    this.keysymToCode = new Map();
    for (let k = 0; k < count; k++) {
      for (let j = 0; j < per; j++) {
        const sym = rest.readUInt32LE((k * per + j) * 4);
        if (sym && !this.keysymToCode.has(sym)) this.keysymToCode.set(sym, this.minKey + k);
      }
    }
  }
  async fakeInput(type, detail, x, y) {
    if (!this.xtest) return;
    const req = Buffer.alloc(36);
    req[0] = this.xtest;
    req[1] = 2;
    req.writeUInt16LE(9, 2);
    req[4] = type;
    req[5] = detail;
    req.writeUInt32LE(0, 8);
    req.writeUInt32LE(this.root, 12);
    // XTest reads the payload as an xEvent: rootX/rootY sit at 24/26, not 16/18.
    req.writeInt16LE(x | 0, 24);
    req.writeInt16LE(y | 0, 26);
    this.send(req);
  }
  async queryPointer() {
    const req = Buffer.alloc(8);
    req[0] = 38;
    req.writeUInt16LE(2, 2);
    req.writeUInt32LE(this.root, 4);
    const { head } = await this.round(req, () => 0);
    return { x: head.readUInt16LE(16), y: head.readUInt16LE(18) };
  }
}

function toFrame(raw, sw, sh) {
  const bpp = Math.max(4, Math.floor(raw.length / (sw * sh)) || 4);
  const out = Buffer.alloc(sw * sh * 4);
  for (let i = 0; i < sw * sh; i++) {
    const s = i * bpp;
    const d = i * 4;
    out[d] = raw[s] || 0;
    out[d + 1] = raw[s + 1] || 0;
    out[d + 2] = raw[s + 2] || 0;
  }
  return out;
}

let lastFrame = null;
let pointer = { x: 0, y: 0 };
let prevPointer = { x: -1, y: -1 };
let buttons = 0;
let lastSentClip = null;
let lastPoll = 0;

function tileDirty(frame, sw, t) {
  if (!lastFrame) return true;
  if (MARKER) {
    const hit = (p) => p.x >= t.x && p.y >= t.y && p.x < t.x + t.tw && p.y < t.y + t.th;
    if (hit(pointer) || hit(prevPointer)) return true;
  }
  for (let row = 0; row < t.th; row++) {
    const o = ((t.y + row) * sw + t.x) * 4;
    if (frame.compare(lastFrame, o, o + t.tw * 4, o, o + t.tw * 4) !== 0) return true;
  }
  return false;
}

function collectTiles(frame, sw, sh) {
  const tiles = [];
  for (let y = 0; y < sh; y += TILE) {
    for (let x = 0; x < sw; x += TILE) {
      const t = { x, y, tw: Math.min(TILE, sw - x), th: Math.min(TILE, sh - y) };
      if (tileDirty(frame, sw, t)) tiles.push(t);
    }
  }
  return tiles;
}

function rememberTiles(frame, sw, tiles) {
  if (!lastFrame || lastFrame.length !== frame.length) lastFrame = Buffer.alloc(frame.length);
  for (const t of tiles) {
    for (let row = 0; row < t.th; row++) {
      const o = ((t.y + row) * sw + t.x) * 4;
      frame.copy(lastFrame, o, o, o + t.tw * 4);
    }
  }
}

function paintMarker(pix, t) {
  for (let dy = -2; dy <= 2; dy++) {
    for (let dx = -2; dx <= 2; dx++) {
      const xx = pointer.x + dx;
      const yy = pointer.y + dy;
      if (xx < t.x || yy < t.y || xx >= t.x + t.tw || yy >= t.y + t.th) continue;
      const i = ((yy - t.y) * t.tw + (xx - t.x)) * 4;
      pix[i] = 255; pix[i + 1] = 255; pix[i + 2] = 255;
    }
  }
}

function encodeTiles(frame, sw, tiles) {
  const parts = [Buffer.alloc(4)];
  parts[0][0] = 0;
  parts[0].writeUInt16BE(tiles.length, 2);
  for (const t of tiles) {
    const rh = Buffer.alloc(12);
    rh.writeUInt16BE(t.x, 0);
    rh.writeUInt16BE(t.y, 2);
    rh.writeUInt16BE(t.tw, 4);
    rh.writeUInt16BE(t.th, 6);
    rh.writeInt32BE(0, 8);
    const pix = Buffer.alloc(t.tw * t.th * 4);
    for (let row = 0; row < t.th; row++) {
      const o = ((t.y + row) * sw + t.x) * 4;
      frame.copy(pix, row * t.tw * 4, o, o + t.tw * 4);
    }
    if (MARKER) paintMarker(pix, t);
    parts.push(rh, pix);
  }
  return Buffer.concat(parts);
}

let outChain = Promise.resolve();
function writeOut(buf) {
  const run = outChain.then(() => new Promise(res => {
    if (process.stdout.write(buf)) res();
    else process.stdout.once('drain', res);
  }));
  outChain = run.then(() => {}, () => {});
  return run;
}

async function emitCut(text) {
  const b = Buffer.from(text, 'utf8');
  const hdr = Buffer.alloc(8);
  hdr[0] = 3;
  hdr.writeUInt32BE(b.length, 4);
  await writeOut(Buffer.concat([hdr, b]));
}

async function applyPointer(x11, mask, x, y) {
  const px = Math.max(0, Math.min(x11.width - 1, x));
  const py = Math.max(0, Math.min(x11.height - 1, y));
  await x11.fakeInput(6, 0, px, py);
  for (let b = 0; b < 8; b++) {
    const bit = 1 << b;
    const was = buttons & bit;
    const now = mask & bit;
    if (was === now) continue;
    await x11.fakeInput(now ? 4 : 5, b + 1, px, py);
  }
  buttons = mask;
  try { pointer = await x11.queryPointer(); } catch (e) { pointer = { x: px, y: py }; }
}

async function sendFrame(x11) {
  const sw = x11.width;
  const sh = x11.height;
  const raw = await x11.getImage(sw, sh);
  const frame = toFrame(raw, sw, sh);
  let tiles = collectTiles(frame, sw, sh).slice(0, 40);
  if (Date.now() - lastPoll > 800) {
    lastPoll = Date.now();
    try {
      const text = await x11.readClipboard();
      if (text && text !== lastSentClip) {
        lastSentClip = text;
        await emitCut(text);
      }
    } catch (e) { log('clip', e.message); }
  }
  if (!tiles.length) {
    await sleep(40);
    tiles = [];
  }
  await writeOut(encodeTiles(frame, sw, tiles));
  rememberTiles(frame, sw, tiles);
  prevPointer = { x: pointer.x, y: pointer.y };
}

let cin = Buffer.alloc(0);
let stage = 'version';
let x11ref = null;
let driving = false;

function onClient(chunk) {
  if (chunk && chunk.length) cin = Buffer.concat([cin, chunk]);
  if (driving) return;
  driving = true;
  drive().catch(e => { log(e.stack || e.message); shutdown(); }).finally(() => {
    driving = false;
    if (cin.length) onClient(Buffer.alloc(0));
  });
}

async function drive() {
  const x11 = x11ref;
  while (true) {
    if (stage === 'version') {
      if (cin.length < 12) return;
      cin = cin.subarray(12);
      await writeOut(Buffer.from('RFB 003.008\n'));
      await writeOut(Buffer.from([1, 1]));
      stage = 'security';
      continue;
    }
    if (stage === 'security') {
      if (cin.length < 1) return;
      cin = cin.subarray(1);
      await writeOut(Buffer.alloc(4));
      stage = 'init';
      continue;
    }
    if (stage === 'init') {
      if (cin.length < 1) return;
      cin = cin.subarray(1);
      const name = Buffer.from('thinkbot');
      const init = Buffer.alloc(24 + name.length);
      init.writeUInt16BE(x11.width, 0);
      init.writeUInt16BE(x11.height, 2);
      init[4] = 32; init[5] = 24; init[6] = 0; init[7] = 1;
      init.writeUInt16BE(255, 8);
      init.writeUInt16BE(255, 10);
      init.writeUInt16BE(255, 12);
      init[14] = 16; init[15] = 8; init[16] = 0;
      init.writeUInt32BE(name.length, 20);
      name.copy(init, 24);
      await writeOut(init);
      stage = 'msg';
      continue;
    }
    if (!x11) return;
    if (cin.length < 1) return;
    const t = cin[0];
    if (t === 0) {
      if (cin.length < 20) return;
      cin = cin.subarray(20);
      continue;
    }
    if (t === 2) {
      if (cin.length < 4) return;
      const n = cin.readUInt16BE(2);
      if (cin.length < 4 + n * 4) return;
      cin = cin.subarray(4 + n * 4);
      continue;
    }
    if (t === 3) {
      if (cin.length < 10) return;
      cin = cin.subarray(10);
      await sendFrame(x11);
      continue;
    }
    if (t === 4) {
      if (cin.length < 8) return;
      const down = cin[1];
      const sym = cin.readUInt32BE(4);
      cin = cin.subarray(8);
      const code = x11.keysymToCode && x11.keysymToCode.get(sym);
      if (code) await x11.fakeInput(down ? 2 : 3, code, pointer.x, pointer.y);
      continue;
    }
    if (t === 5) {
      if (cin.length < 6) return;
      const mask = cin[1];
      const x = cin.readUInt16BE(2);
      const y = cin.readUInt16BE(4);
      cin = cin.subarray(6);
      await applyPointer(x11, mask, x, y);
      continue;
    }
    if (t === 6) {
      if (cin.length < 8) return;
      const n = cin.readUInt32BE(4);
      if (n > 256 * 1024) { shutdown(); return; }
      if (cin.length < 8 + n) return;
      const text = cin.subarray(8, 8 + n).toString('utf8');
      cin = cin.subarray(8 + n);
      try {
        await x11.setClipboard(text);
        lastSentClip = text;
        if (CLIP_ACK) {
          const back = await x11.readClipboard();
          if (back === text) await emitCut(text);
        }
      } catch (e) { log('set clip', e.message); }
      continue;
    }
    log('unknown client byte', t);
    shutdown();
    return;
  }
}

async function main() {
  const disp = await ensureDisplay();
  const num = displayNum(disp.display);
  const sock = net.createConnection('/tmp/.X11-unix/X' + num);
  await new Promise((res, rej) => { sock.once('connect', res); sock.once('error', rej); });
  const x11 = new XConn(sock);
  await x11.handshake(disp.cookie);
  const ext = await x11.queryExtension('XTEST');
  if (ext.present) x11.xtest = ext.major;
  try { await x11.keymap(); } catch (e) { log('keymap', e.message); }
  try { await x11.setupClipboard(); } catch (e) { log('clip setup', e.message); }
  if (!x11.width || !x11.height) {
    x11.width = SCREEN_W;
    x11.height = SCREEN_H;
  }
  x11ref = x11;
  process.stdin.on('data', chunk => onClient(chunk));
  process.stdin.on('end', () => shutdown());
  process.stdin.resume();
}

function shutdown() {
  if (xvfb) { try { xvfb.kill(); } catch (e) {} }
  process.exit(0);
}
process.on('SIGTERM', shutdown);
process.on('SIGINT', shutdown);

main().catch(e => { process.stderr.write('desktop: ' + (e.stack || e.message) + '\n'); shutdown(); });
