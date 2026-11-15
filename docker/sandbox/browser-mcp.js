#!/usr/bin/env node
'use strict';

// thinkbot browser MCP — 运行在 per-bot 容器内，通过 stdio 暴露浏览器工具。
// 驱动：patchright（Playwright 源码级反检测 fork）。浏览器：系统 chromium。
// 重要：本进程唯一浏览器会话，天然单会话互斥（Chromium profile 锁）。
// cookie：启动从 /data/.browser-state.json 注入，退出时回写（与 Web 管理面板共享同一文件）。

const { chromium } = require('patchright');
const fs = require('fs');
const path = require('path');
const os = require('os');

const PROFILE_DIR = process.env.BOT_BROWSER_PROFILE || '/data/.browser-profile';
const STATE_FILE = process.env.BOT_BROWSER_STATE || '/data/.browser-state.json';
const SHOT_DIR = process.env.BOT_BROWSER_SHOTS || '/data/browser-screenshots';
const PROXY = process.env.BOT_BROWSER_PROXY || '';
const UA = process.env.BOT_BROWSER_UA || 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36';

let browser = null;
let context = null;
let page = null;
let shuttingDown = false;
let stateSaver = null; // 周期落盘定时器（兜底，避免进程被非优雅终止时丢 cookie）

function log(...a) {
  // 仅打到 stderr 并被 thinkbot 侧收集（若已修复 cmd.Stderr=nil）；绝不打 cookie 值。
  process.stderr.write('[browser-mcp] ' + a.map(x => (x && x.stack) ? x.stack : String(x)).join(' ') + '\n');
}

function ensureDir(p) { try { fs.mkdirSync(p, { recursive: true }); } catch (e) {} }

async function loadState() {
  try {
    if (fs.existsSync(STATE_FILE)) {
      const raw = JSON.parse(fs.readFileSync(STATE_FILE, 'utf8'));
      if (Array.isArray(raw.cookies) && raw.cookies.length) {
        await context.addCookies(raw.cookies);
        return raw.cookies.length;
      }
    }
  } catch (e) { log('loadState error', e.message); }
  return 0;
}

async function saveState() {
  try {
    if (!context) return;
    const cookies = await context.cookies();
    fs.writeFileSync(STATE_FILE, JSON.stringify({ cookies }, null, 2));
  } catch (e) { log('saveState error', e.message); }
}

async function initBrowser() {
  ensureDir(PROFILE_DIR);
  ensureDir(SHOT_DIR);
  const launchOpts = {
    executablePath: '/usr/bin/chromium',
    headless: false, // headful（配合 xvfb），反检测更稳
    args: [
      '--no-sandbox',
      '--disable-dev-shm-usage',
      '--disable-gpu',
      '--disable-blink-features=AutomationControlled',
      '--ozone-platform=x11',
    ],
  };
  if (PROXY) {
    launchOpts.proxy = { server: PROXY };
    log('proxy enabled:', PROXY);
  }
  browser = await chromium.launch(launchOpts);
  const ctxOpts = {
    viewport: { width: 1280, height: 800 },
    userAgent: UA,
    locale: 'zh-CN',
    timezoneId: 'Asia/Shanghai',
  };
  context = await browser.newContext(ctxOpts);
  const n = await loadState();
  log('context ready, injected cookies:', n);
  page = await context.newPage();
  // 周期落盘：登录态常随导航/重定向落地，进程若被非优雅终止（如 docker exec 被 SIGKILL），
  // shutdown 钩子不会触发，故用定时器兜底，确保 cookie 不丢。
  if (!stateSaver) {
    stateSaver = setInterval(() => { saveState().catch(() => {}); }, 30000);
  }
}

async function shutdown(sig) {
  if (shuttingDown) return;
  shuttingDown = true;
  log('shutdown', sig);
  if (stateSaver) { clearInterval(stateSaver); stateSaver = null; }
  try { await saveState(); } catch (e) {}
  try { if (page) await page.close(); } catch (e) {}
  try { if (context) await context.close(); } catch (e) {}
  try { if (browser) await browser.close(); } catch (e) {}
  process.exit(0);
}
process.on('SIGTERM', () => shutdown('SIGTERM'));
process.on('SIGINT', () => shutdown('SIGINT'));
process.on('beforeExit', () => { if (!shuttingDown) shutdown('beforeExit'); });

// ---------------- JSON-RPC over stdio ----------------
const tools = [
  {
    name: 'navigate',
    description: 'Navigate to the specified URL. Returns the final URL, page title, HTTP status, and an accessibility tree summary.',
    inputSchema: { type: 'object', properties: { url: { type: 'string', description: 'Target URL' }, waitUntil: { type: 'string', enum: ['load','domcontentloaded','networkidle'], default: 'domcontentloaded' } }, required: ['url'] },
  },
  {
    name: 'click',
    description: 'Click an element matching a CSS selector.',
    inputSchema: { type: 'object', properties: { selector: { type: 'string' }, timeout: { type: 'number', default: 15000 } }, required: ['selector'] },
  },
  {
    name: 'fill',
    description: 'Clear and fill text into a text <input> or <textarea>. Use select_option for <select> dropdowns and check for checkboxes/radios; call form_fields first if unsure of selectors. Note: do NOT use this for automated login forms — import accounts via the Web panel instead.',
    inputSchema: { type: 'object', properties: { selector: { type: 'string' }, value: { type: 'string' }, timeout: { type: 'number', default: 15000 } }, required: ['selector', 'value'] },
  },
  {
    name: 'select_option',
    description: 'Select option(s) in a <select> dropdown (by value, label, or index — pass an array for multi-select). Do NOT use fill or click <option> on dropdowns.',
    inputSchema: { type: 'object', properties: {
      selector: { type: 'string', description: 'CSS selector of the <select> element' },
      value: { description: 'Option value attribute (string or array of strings)' },
      label: { description: 'Visible option text (string or array of strings)' },
      index: { description: 'Zero-based option index (number or array of numbers)' },
      timeout: { type: 'number', default: 15000 },
    }, required: ['selector'] },
  },
  {
    name: 'check',
    description: 'Check or uncheck a checkbox or select a radio button (checked defaults to true). Do NOT use fill on checkboxes or radio buttons.',
    inputSchema: { type: 'object', properties: { selector: { type: 'string' }, checked: { type: 'boolean', default: true }, timeout: { type: 'number', default: 15000 } }, required: ['selector'] },
  },
  {
    name: 'form_fields',
    description: 'List form controls on the current page (input/select/textarea/button) with type, name, id, usable selector, current value, and dropdown options. Call this before filling forms instead of guessing selectors.',
    inputSchema: { type: 'object', properties: { maxFields: { type: 'number', default: 80 } }, required: [] },
  },
  {
    name: 'screenshot',
    description: 'Take a screenshot of the current page, write it to /data/browser-screenshots, and return its path, title, and URL. Screenshots are for human review/archival; prefer browser_get_text for LLM reading.',
    inputSchema: { type: 'object', properties: { full: { type: 'boolean', default: false } }, required: [] },
  },
  {
    name: 'get_text',
    description: 'Return plain text of the current page body (innerText, truncated to 8000 characters by default). Best for reading page content as an LLM.',
    inputSchema: { type: 'object', properties: { maxChars: { type: 'number', default: 8000 } }, required: [] },
  },
  {
    name: 'wait',
    description: 'Wait for a CSS selector to appear on the page (or time out).',
    inputSchema: { type: 'object', properties: { selector: { type: 'string' }, timeout: { type: 'number', default: 15000 } }, required: ['selector'] },
  },
  {
    name: 'back',
    description: 'Navigate back in browser history.',
    inputSchema: { type: 'object', properties: {}, required: [] },
  },
  {
    name: 'forward',
    description: 'Navigate forward in browser history.',
    inputSchema: { type: 'object', properties: {}, required: [] },
  },
  {
    name: 'cookies_list',
    description: 'List cookie domains and names in the current browser context (values omitted to prevent leakage) to verify login state.',
    inputSchema: { type: 'object', properties: {}, required: [] },
  },
  {
    name: 'close',
    description: 'Close the current page, persist cookie state, and exit this MCP process.',
    inputSchema: { type: 'object', properties: {}, required: [] },
  },
  {
    name: 'fetch',
    description: 'Lightweight HTTP fetch (raw request without full browser rendering, fast for static pages/APIs/JSON). Returns status, response headers, and body text. Direct connection (no proxy); pass session cookies via cookie or headers.',
    inputSchema: {
      type: 'object',
      properties: {
        url: { type: 'string', description: 'Target URL' },
        method: { type: 'string', enum: ['GET','POST','PUT','DELETE','HEAD','PATCH'], default: 'GET' },
        headers: { type: 'object', description: 'Extra request headers (key/value strings)' },
        body: { type: 'string', description: 'Request body (for POST/PUT/PATCH)' },
        cookie: { type: 'string', description: 'Cookie header string (applied when Cookie is not explicitly set in headers)' },
        maxChars: { type: 'number', description: 'Maximum body characters before truncation', default: 8000 },
        redirect: { type: 'string', enum: ['follow','manual'], default: 'follow' },
      },
      required: ['url'],
    },
  },
];

function textResult(s) { return { content: [{ type: 'text', text: String(s) }] }; }

// actionHint turns common Playwright failures into a next step, so the model
// does not retry the same failing action (2026-09-27: fill on <select> /
// checkboxes, clicks on <option>, guessed selectors timing out, 11 failures
// in one turn).
function actionHint(name, args, msg) {
  const m = String(msg || '');
  if (/is not an <input>|not an <input>, <textarea>|Element is not an <input>/i.test(m) && name === 'fill') {
    return 'This element cannot be typed into. If it is a <select> dropdown use select_option; if it is a checkbox/radio use check. Call form_fields to see each control\'s type.';
  }
  if (/type "(checkbox|radio)" cannot be filled|cannot be filled/i.test(m)) {
    return 'Checkboxes and radio buttons cannot be filled: use check (checked: true/false).';
  }
  if (/Timeout .*exceeded/i.test(m)) {
    const sel = args && args.selector ? ` matching ${JSON.stringify(args.selector)}` : '';
    let extra = '';
    if (name === 'click' && /(^|[\s>])option\b/i.test(String(args && args.selector || ''))) {
      extra = ' To choose an option of a <select>, use select_option instead of clicking the <option>.';
    }
    return `No actionable element${sel} was found in time (it may not exist, be hidden, or be inside an iframe). Do not retry the same selector: call form_fields or get_text to see the real page, then use a selector from there.${extra}`;
  }
  if (/did not find some options|Options? not found/i.test(m)) {
    return 'The requested option does not exist in this <select>. Call form_fields to list its options (value and label) and pick one of those.';
  }
  return '';
}

async function callTool(name, args) {
  switch (name) {
    case 'navigate': {
      if (!page) throw new Error('page not ready');
      if (/^\s*javascript:/i.test(String(args.url || ''))) {
        throw new Error('javascript: URLs are not supported by navigate. Interact with the page through click / fill / select_option / check (call form_fields to find the right selectors).');
      }
      const waitUntil = args.waitUntil || 'domcontentloaded';
      const resp = await page.goto(args.url, { waitUntil, timeout: 45000 });
      const status = resp ? resp.status() : 'n/a';
      const title = await page.title();
      const url = page.url();
      // 登录态常随导航（重定向/Set-Cookie）落地，导航后即落盘，避免进程被非优雅终止时丢 cookie。
      saveState().catch(() => {});
      return textResult(`status=${status}\ntitle=${title}\nurl=${url}`);
    }
    case 'click': {
      await page.click(args.selector, { timeout: args.timeout || 15000 });
      return textResult(`clicked: ${args.selector}`);
    }
    case 'fill': {
      await page.fill(args.selector, args.value || '', { timeout: args.timeout || 15000 });
      return textResult(`filled: ${args.selector}`);
    }
    case 'select_option': {
      let opt;
      if (args.value !== undefined) opt = Array.isArray(args.value) ? args.value.map(v => ({ value: String(v) })) : { value: String(args.value) };
      else if (args.label !== undefined) opt = Array.isArray(args.label) ? args.label.map(v => ({ label: String(v) })) : { label: String(args.label) };
      else if (args.index !== undefined) opt = Array.isArray(args.index) ? args.index.map(v => ({ index: Number(v) })) : { index: Number(args.index) };
      else throw new Error('select_option requires one of value, label or index');
      const chosen = await page.selectOption(args.selector, opt, { timeout: args.timeout || 15000 });
      return textResult(`selected in ${args.selector}: ${JSON.stringify(chosen)}`);
    }
    case 'check': {
      const checked = args.checked === undefined ? true : !!args.checked;
      await page.setChecked(args.selector, checked, { timeout: args.timeout || 15000 });
      return textResult(`${checked ? 'checked' : 'unchecked'}: ${args.selector}`);
    }
    case 'form_fields': {
      const max = args.maxFields || 80;
      const fields = await page.evaluate((max) => {
        const esc = (v) => (window.CSS && CSS.escape) ? CSS.escape(v) : v;
        const out = [];
        const els = document.querySelectorAll('input, select, textarea, button, [contenteditable="true"]');
        for (const el of els) {
          if (out.length >= max) break;
          const type = el.tagName.toLowerCase() === 'input' ? (el.getAttribute('type') || 'text') : el.tagName.toLowerCase();
          if (type === 'hidden') continue;
          const r = el.getBoundingClientRect();
          const visible = r.width > 0 && r.height > 0;
          let selector = '';
          if (el.id) selector = '#' + esc(el.id);
          else if (el.name) selector = `${el.tagName.toLowerCase()}[name="${el.name}"]`;
          const f = { tag: el.tagName.toLowerCase(), type, selector, name: el.name || undefined, id: el.id || undefined, visible };
          const label = (el.labels && el.labels[0] && el.labels[0].innerText) || el.getAttribute('aria-label') || el.getAttribute('placeholder') || (el.tagName === 'BUTTON' ? el.innerText : '');
          if (label) f.label = String(label).trim().slice(0, 60);
          if (type === 'checkbox' || type === 'radio') { f.checked = el.checked; f.value = el.value; }
          else if (el.tagName === 'SELECT') { f.value = el.value; f.options = Array.from(el.options).slice(0, 30).map(o => ({ value: o.value, label: o.label })); }
          else if ('value' in el && type !== 'password') f.value = String(el.value).slice(0, 80);
          out.push(f);
        }
        return out;
      }, max);
      return textResult(JSON.stringify(fields, null, 1));
    }
    case 'screenshot': {
      ensureDir(SHOT_DIR);
      const ts = Date.now();
      const f = path.join(SHOT_DIR, `shot_${ts}.png`);
      await page.screenshot({ path: f, fullPage: !!args.full });
      const title = await page.title();
      return textResult(`screenshot saved: ${f}\ntitle=${title}\nurl=${page.url()}`);
    }
    case 'get_text': {
      const t = await page.locator('body').innerText().catch(() => '');
      const max = args.maxChars || 8000;
      return textResult(t.slice(0, max));
    }
    case 'wait': {
      await page.waitForSelector(args.selector, { timeout: args.timeout || 15000 });
      return textResult(`selector appeared: ${args.selector}`);
    }
    case 'back': { await page.goBack().catch(() => {}); return textResult('back'); }
    case 'forward': { await page.goForward().catch(() => {}); return textResult('forward'); }
    case 'cookies_list': {
      const cs = await context.cookies();
      const names = cs.map(c => `${c.domain}▸${c.name}`).join('\n');
      return textResult(`cookie count=${cs.length}\n${names}`);
    }
    case 'close': {
      // 先落盘并回包，再退出：thinkbot 侧 Close 会调用本工具触发优雅回收，
      // 必须在回包前完成 saveState，否则回收读到的是旧状态文件。
      await saveState().catch(() => {});
      setTimeout(() => shutdown('tool'), 60);
      return textResult('closed');
    }
    case 'fetch': {
      const url = args.url;
      if (!url) throw new Error('fetch requires url');
      const method = (args.method || 'GET').toUpperCase();
      const headers = Object.assign({}, args.headers || {});
      if (args.cookie && !('cookie' in headers) && !('Cookie' in headers)) headers['Cookie'] = args.cookie;
      const init = { method, headers, redirect: args.redirect || 'follow' };
      if (args.body && !['GET', 'HEAD'].includes(method)) init.body = args.body;
      const ctrl = new AbortController();
      const timer = setTimeout(() => ctrl.abort(), 30000);
      let res;
      try {
        res = await fetch(url, Object.assign({ signal: ctrl.signal }, init));
      } catch (e) {
        clearTimeout(timer);
        return textResult('fetch error: ' + e.message);
      }
      clearTimeout(timer);
      const status = res.status;
      const respHeaders = {};
      res.headers.forEach((v, k) => { respHeaders[k] = v; });
      let body = '';
      try { body = await res.text(); } catch (e) { body = '[body read error: ' + e.message + ']'; }
      const max = args.maxChars || 8000;
      const hdrStr = Object.keys(respHeaders).map(k => `${k}: ${respHeaders[k]}`).join('\n');
      return textResult(`status=${status}\nheaders:\n${hdrStr}\n\nbody:\n${body.slice(0, max)}`);
    }
    default:
      throw new Error('unknown tool: ' + name);
  }
}

let buf = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', chunk => {
  buf += chunk;
  let idx;
  while ((idx = buf.indexOf('\n')) >= 0) {
    const line = buf.slice(0, idx).trim();
    buf = buf.slice(idx + 1);
    if (line) handleLine(line);
  }
});
process.stdin.on('end', () => shutdown('stdin_end'));

function send(obj) { process.stdout.write(JSON.stringify(obj) + '\n'); }

function handleLine(line) {
  let msg;
  try { msg = JSON.parse(line); } catch (e) { return; }
  const id = msg.id;
  const method = msg.method;
  if (method === 'initialize') {
    send({ jsonrpc: '2.0', id, result: {
      protocolVersion: '2024-11-05',
      capabilities: { tools: {} },
      serverInfo: { name: 'thinkbot-browser', version: '0.1.0' },
    }});
    return;
  }
  if (method === 'notifications/initialized' || method === 'initialized') { return; }
  if (method === 'ping') { send({ jsonrpc: '2.0', id, result: {} }); return; }
  if (method === 'tools/list') {
    send({ jsonrpc: '2.0', id, result: { tools } });
    return;
  }
  if (method === 'tools/call') {
    const { name, arguments: args } = msg.params || {};
    (async () => {
      try {
        // fetch / close 不需要浏览器实例：fetch 应是轻量纯 HTTP，close 只做落盘，
        // 避免为它们拉起完整 chromium（既省资源，也避免 shutdown 时反复起浏览器）。
        if (name === 'fetch' || name === 'close') {
          const result = await callTool(name, args || {});
          send({ jsonrpc: '2.0', id, result });
          return;
        }
        if (!browser) await initBrowser();
        const result = await callTool(name, args || {});
        send({ jsonrpc: '2.0', id, result });
      } catch (e) {
        const hint = actionHint(name, args || {}, e.message);
        const message = hint ? `${e.message}\nHINT: ${hint}` : e.message;
        send({ jsonrpc: '2.0', id, error: { code: -32000, message } });
      }
    })();
    return;
  }
  // 其他通知忽略
}

// 启动：先不拉浏览器，首个 tools/call 时再初始化（懒加载，省资源）。
send({ jsonrpc: '2.0', method: 'log', params: { message: 'thinkbot-browser-mcp ready' } });
