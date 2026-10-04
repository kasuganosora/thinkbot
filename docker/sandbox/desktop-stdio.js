#!/usr/bin/env node
'use strict';
// ThinkBot sandbox desktop. Speaks RFB 3.8 on stdin/stdout and reads the bot's
// own X display (the same Xvfb Chromium uses). One process, one display.
// Host mode never scans other processes' displays.

const net = require('net');
const fs = require('fs');
const { spawn } = require('child_process');

const SCREEN_W = 1280;
const SCREEN_H = 800;
const SCALE = 2;
const RFB_W = SCREEN_W / SCALE;
const RFB_H = SCREEN_H / SCALE;
const MODE = process.env.THINKBOT_DESKTOP_MODE || 'container';

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
    const family = buf.readUInt16BE(o); o += 2;
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
    void family;
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
  async round(req, extraOf) {
    this.seq = (this.seq + 1) & 0xffff;
    this.sock.write(req);
    for (;;) {
      const head = await this.need(32);
      const kind = head[0];
      if (kind === 0) throw new Error('x error ' + head[1]);
      if (kind !== 1) continue; // event
      const extra = extraOf ? extraOf(head) : head.readUInt32LE(4) * 4;
      const rest = extra ? await this.need(extra) : Buffer.alloc(0);
      return { head, rest };
    }
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
  async getImage(w, h) {
    const req = Buffer.alloc(20);
    req[0] = 73;
    req[1] = 2;
    req.writeUInt16LE(5, 2);
    req.writeUInt32LE(this.root, 4);
    req.writeUInt16LE(0, 8);
    req.writeUInt16LE(0, 10);
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
    req.writeUInt32LE(this.root, 12);
    req.writeUInt16LE(x, 16);
    req.writeUInt16LE(y, 18);
    this.seq = (this.seq + 1) & 0xffff;
    this.sock.write(req);
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

function scaleFrame(raw, sw, sh) {
  const out = Buffer.alloc(RFB_W * RFB_H * 4);
  const bpp = Math.max(4, Math.floor(raw.length / (sw * sh)) || 4);
  for (let y = 0; y < RFB_H; y++) {
    for (let x = 0; x < RFB_W; x++) {
      const sx = Math.min(sw - 1, x * SCALE);
      const sy = Math.min(sh - 1, y * SCALE);
      const si = (sy * sw + sx) * bpp;
      const di = (y * RFB_W + x) * 4;
      // X ZPixmap little-endian is B,G,R,x. RFB shift 16/8/0 wants the same bytes.
      out[di] = raw[si] || 0;
      out[di + 1] = raw[si + 1] || 0;
      out[di + 2] = raw[si + 2] || 0;
      out[di + 3] = 0;
    }
  }
  return out;
}

let pointer = { x: 0, y: 0 };
let buttons = 0;

async function applyPointer(x11, mask, x, y) {
  const px = Math.max(0, Math.min(SCREEN_W - 1, x * SCALE));
  const py = Math.max(0, Math.min(SCREEN_H - 1, y * SCALE));
  await x11.fakeInput(6, 0, px, py);
  for (let b = 0; b < 5; b++) {
    const bit = 1 << b;
    const was = buttons & bit;
    const now = mask & bit;
    if (was === now) continue;
    await x11.fakeInput(now ? 4 : 5, b + 1, px, py);
  }
  buttons = mask;
  try { pointer = await x11.queryPointer(); } catch (e) { pointer = { x: px, y: py }; }
}

function paintMarker(frame) {
  if (!pointer) return;
  const x = Math.max(0, Math.min(RFB_W - 1, Math.floor(pointer.x / SCALE)));
  const y = Math.max(0, Math.min(RFB_H - 1, Math.floor(pointer.y / SCALE)));
  for (let dy = -2; dy <= 2; dy++) {
    for (let dx = -2; dx <= 2; dx++) {
      const xx = x + dx, yy = y + dy;
      if (xx < 0 || yy < 0 || xx >= RFB_W || yy >= RFB_H) continue;
      const i = (yy * RFB_W + xx) * 4;
      frame[i] = 255; frame[i + 1] = 255; frame[i + 2] = 255;
    }
  }
}

function rfbRect(frame) {
  const hdr = Buffer.alloc(16);
  hdr[0] = 0;
  hdr.writeUInt16BE(1, 2);
  hdr.writeUInt16BE(0, 4);
  hdr.writeUInt16BE(0, 6);
  hdr.writeUInt16BE(RFB_W, 8);
  hdr.writeUInt16BE(RFB_H, 10);
  hdr.writeInt32BE(0, 12);
  return Buffer.concat([hdr, frame]);
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

  const sw = x11.width || SCREEN_W;
  const sh = x11.height || SCREEN_H;

  process.stdin.on('data', chunk => onClient(chunk, x11, sw, sh));
  process.stdin.on('end', () => shutdown());
  // server greeting is sent after the client version arrives
}

let cin = Buffer.alloc(0);
let stage = 'version';
let x11ref, swref, shref;
let sending = false;
let wantFrame = false;

function onClient(chunk, x11, sw, sh) {
  x11ref = x11; swref = sw; shref = sh;
  cin = Buffer.concat([cin, chunk]);
  drive().catch(e => { log(e.stack || e.message); shutdown(); });
}

async function drive() {
  const x11 = x11ref;
  while (true) {
    if (stage === 'version') {
      if (cin.length < 12) return;
      cin = cin.subarray(12);
      process.stdout.write(Buffer.from('RFB 003.008\n'));
      process.stdout.write(Buffer.from([1, 1]));
      stage = 'security';
      continue;
    }
    if (stage === 'security') {
      if (cin.length < 1) return;
      cin = cin.subarray(1);
      const ok = Buffer.alloc(4);
      process.stdout.write(ok);
      stage = 'init';
      continue;
    }
    if (stage === 'init') {
      if (cin.length < 1) return;
      cin = cin.subarray(1);
      const name = Buffer.from('thinkbot');
      const init = Buffer.alloc(24 + name.length);
      init.writeUInt16BE(RFB_W, 0);
      init.writeUInt16BE(RFB_H, 2);
      init[4] = 32; init[5] = 24; init[6] = 0; init[7] = 1;
      init.writeUInt16BE(255, 8);
      init.writeUInt16BE(255, 10);
      init.writeUInt16BE(255, 12);
      init[14] = 16; init[15] = 8; init[16] = 0;
      init.writeUInt32BE(name.length, 20);
      name.copy(init, 24);
      process.stdout.write(init);
      stage = 'msg';
      continue;
    }
    if (cin.length < 1) return;
    const t = cin[0];
    if (t === 0 && cin.length >= 20) { cin = cin.subarray(20); continue; } // pixel format
    if (t === 2) {
      if (cin.length < 4) return;
      const n = cin.readUInt16BE(2);
      const need = 4 + n * 4;
      if (cin.length < need) return;
      cin = cin.subarray(need);
      continue;
    }
    if (t === 3 && cin.length >= 10) {
      cin = cin.subarray(10);
      wantFrame = true;
      if (!sending) { sending = true; sendFrame(x11).finally(() => { sending = false; }); }
      continue;
    }
    if (t === 4 && cin.length >= 8) {
      const down = cin[1];
      const sym = cin.readUInt32BE(4);
      cin = cin.subarray(8);
      const code = x11.keysymToCode && x11.keysymToCode.get(sym);
      if (code) await x11.fakeInput(down ? 2 : 3, code, pointer.x, pointer.y);
      continue;
    }
    if (t === 5 && cin.length >= 6) {
      const mask = cin[1];
      const x = cin.readUInt16BE(2);
      const y = cin.readUInt16BE(4);
      cin = cin.subarray(6);
      await applyPointer(x11, mask, x, y);
      continue;
    }
    if (t === 6 && cin.length >= 8) {
      const n = cin.readUInt32BE(4);
      if (cin.length < 8 + n) return;
      cin = cin.subarray(8 + n);
      continue;
    }
    if (t !== 0 && t !== 2 && t !== 3 && t !== 4 && t !== 5 && t !== 6) {
      log('unknown client byte', t);
      shutdown();
    }
    return;
  }
}

async function sendFrame(x11) {
  if (!wantFrame) return;
  wantFrame = false;
  const raw = await x11.getImage(swref, shref);
  const frame = scaleFrame(raw, swref, shref);
  paintMarker(frame);
  process.stdout.write(rfbRect(frame));
}

function shutdown() {
  if (xvfb) { try { xvfb.kill(); } catch (e) {} }
  process.exit(0);
}
process.on('SIGTERM', shutdown);
process.on('SIGINT', shutdown);

main().catch(e => { process.stderr.write('desktop: ' + (e.stack || e.message) + '\n'); shutdown(); });
