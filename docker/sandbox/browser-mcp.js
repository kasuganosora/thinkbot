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
    description: '导航到指定 URL。返回最终 URL、标题、HTTP 状态、可访问性树摘要。',
    inputSchema: { type: 'object', properties: { url: { type: 'string', description: '目标 URL' }, waitUntil: { type: 'string', enum: ['load','domcontentloaded','networkidle'], default: 'domcontentloaded' } }, required: ['url'] },
  },
  {
    name: 'click',
    description: '按 CSS 选择器点击元素。',
    inputSchema: { type: 'object', properties: { selector: { type: 'string' }, timeout: { type: 'number', default: 15000 } }, required: ['selector'] },
  },
  {
    name: 'fill',
    description: '在输入框内填写文本（先清空）。注意：禁止用于登录表单自动登录，账号导入走 Web 面板。',
    inputSchema: { type: 'object', properties: { selector: { type: 'string' }, value: { type: 'string' }, timeout: { type: 'number', default: 15000 } }, required: ['selector', 'value'] },
  },
  {
    name: 'screenshot',
    description: '对当前页面截图，写入 /data/browser-screenshots 并返回路径、标题、URL。截图仅供人工/存证查看；对 LLM 优先用 browser_get_text。',
    inputSchema: { type: 'object', properties: { full: { type: 'boolean', default: false } }, required: [] },
  },
  {
    name: 'get_text',
    description: '返回当前页面 body 的纯文本（innerText，截断到 8000 字）。适合让 LLM 读取页面内容。',
    inputSchema: { type: 'object', properties: { maxChars: { type: 'number', default: 8000 } }, required: [] },
  },
  {
    name: 'wait',
    description: '等待指定 CSS 选择器出现（或超时）。',
    inputSchema: { type: 'object', properties: { selector: { type: 'string' }, timeout: { type: 'number', default: 15000 } }, required: ['selector'] },
  },
  {
    name: 'back',
    description: '浏览器后退。',
    inputSchema: { type: 'object', properties: {}, required: [] },
  },
  {
    name: 'forward',
    description: '浏览器前进。',
    inputSchema: { type: 'object', properties: {}, required: [] },
  },
  {
    name: 'cookies_list',
    description: '列出当前上下文的 cookie 域名与名称（不含值，防止泄露）。用于确认登录态是否生效。',
    inputSchema: { type: 'object', properties: {}, required: [] },
  },
  {
    name: 'close',
    description: '关闭当前页面并保存 cookie 状态后退出本 MCP 进程。',
    inputSchema: { type: 'object', properties: {}, required: [] },
  },
  {
    name: 'fetch',
    description: '轻量 HTTP 抓取（纯请求，不启完整浏览器渲染，快速取静态页/API/JSON）。返回 status、响应头、body 文本。不走代理；登录态用 cookie 参数或随 headers 传入。',
    inputSchema: {
      type: 'object',
      properties: {
        url: { type: 'string', description: '目标 URL' },
        method: { type: 'string', enum: ['GET','POST','PUT','DELETE','HEAD','PATCH'], default: 'GET' },
        headers: { type: 'object', description: '额外请求头（key/value 字符串）' },
        body: { type: 'string', description: '请求体（POST/PUT/PATCH 时）' },
        cookie: { type: 'string', description: 'Cookie 字符串，注入请求头（headers 未显式给 Cookie 时生效）' },
        maxChars: { type: 'number', description: 'body 截断字符数', default: 8000 },
        redirect: { type: 'string', enum: ['follow','manual'], default: 'follow' },
      },
      required: ['url'],
    },
  },
];

function textResult(s) { return { content: [{ type: 'text', text: String(s) }] }; }

async function callTool(name, args) {
  switch (name) {
    case 'navigate': {
      if (!page) throw new Error('page not ready');
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
        send({ jsonrpc: '2.0', id, error: { code: -32000, message: e.message } });
      }
    })();
    return;
  }
  // 其他通知忽略
}

// 启动：先不拉浏览器，首个 tools/call 时再初始化（懒加载，省资源）。
send({ jsonrpc: '2.0', method: 'log', params: { message: 'thinkbot-browser-mcp ready' } });
