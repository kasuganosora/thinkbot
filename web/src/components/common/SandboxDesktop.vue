<template>
  <div class="desk" data-testid="sandbox-desktop">
    <div class="desk-bar">
      <button type="button" class="desk-btn" @click="toggle">
        {{ phase === 'connected' ? '关闭桌面' : '打开桌面' }}
      </button>
      <button type="button" class="desk-btn" :disabled="phase !== 'connected'" @click="copyOut">复制</button>
      <button type="button" class="desk-btn" :disabled="phase !== 'connected'" @click="pasteIn">粘贴</button>
      <button type="button" class="desk-btn" :disabled="!width" @click="fit = !fit">
        {{ fit ? '原始大小' : '适应窗口' }}
      </button>
      <span class="desk-status" :class="phase">{{ status }}</span>
    </div>
    <div class="desk-stage" :class="{ fit, actual: !fit }">
      <canvas
        v-show="width > 0"
        ref="canvasRef"
        class="desk-canvas"
        :class="{ actual: !fit }"
        tabindex="0"
        @pointerdown="onPointer"
        @pointermove="onPointer"
        @pointerup="onPointer"
        @pointercancel="onPointer"
        @pointerleave="onPointer"
        @wheel.prevent="onWheel"
        @keydown="onKey($event, true)"
        @keyup="onKey($event, false)"
        @paste="onPaste"
        @copy="onCopy"
        @compositionend="onCompose"
        @focus="focused = true"
        @blur="focused = false"
        @contextmenu.prevent
      />
      <div v-if="phase !== 'connected'" class="desk-overlay">{{ status }}</div>
    </div>
    <p v-if="phase === 'connected' && !focused" class="desk-note">点击画面后键盘才生效。</p>
  </div>
</template>

<script setup>
import { onBeforeUnmount, ref } from 'vue'

const props = defineProps({
  botId: { type: String, required: true },
  scope: { type: String, default: 'bot' }
})

const canvasRef = ref(null)
const connected = ref(false)
const focused = ref(false)
const fit = ref(true)
const status = ref('未连接')
const phase = ref('idle')
const width = ref(0)
const height = ref(0)
let ws = null
let buf = new Uint8Array(0)
let stage = 'version'
let buttons = 0
let remoteClip = ''

function urlFor(id) {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const path = props.scope === 'session'
    ? `/api/sessions/${encodeURIComponent(id)}/desktop`
    : `/api/bots/${encodeURIComponent(id)}/desktop`
  return `${proto}//${location.host}${path}`
}

function toggle() {
  if (phase.value === 'connected' || phase.value === 'connecting') disconnect(true)
  else connect()
}

function connect() {
  disconnect(false)
  phase.value = 'connecting'
  status.value = '正在连接'
  stage = 'version'
  buf = new Uint8Array(0)
  buttons = 0
  ws = new WebSocket(urlFor(props.botId))
  ws.binaryType = 'arraybuffer'
  ws.onopen = () => {
    ws.send(new TextEncoder().encode('RFB 003.008\n'))
  }
  ws.onmessage = (ev) => {
    const chunk = new Uint8Array(ev.data)
    const next = new Uint8Array(buf.length + chunk.length)
    next.set(buf, 0)
    next.set(chunk, buf.length)
    buf = next
    try { pump() } catch (err) {
      phase.value = 'failed'
      status.value = '画面解析失败'
      disconnect(false)
    }
  }
  ws.onerror = () => {
    phase.value = 'failed'
    status.value = '桌面连接失败'
  }
  ws.onclose = () => {
    connected.value = false
    focused.value = false
    if (phase.value === 'connecting' || phase.value === 'failed') {
      phase.value = 'failed'
      status.value = '桌面连接失败'
    } else if (phase.value === 'connected') {
      phase.value = 'disconnected'
      status.value = '已断开'
    }
  }
}

function disconnect(manual) {
  if (ws) {
    ws.onclose = null
    try { ws.close() } catch (_) {}
    ws = null
  }
  connected.value = false
  focused.value = false
  if (manual) {
    phase.value = 'idle'
    status.value = '未连接'
  }
}

function take(n) {
  if (buf.length < n) return null
  const out = buf.slice(0, n)
  buf = buf.slice(n)
  return out
}

function u16(b, o) { return (b[o] << 8) | b[o + 1] }
function u32(b, o) { return ((b[o] << 24) | (b[o + 1] << 16) | (b[o + 2] << 8) | b[o + 3]) >>> 0 }

function pump() {
  for (;;) {
    if (stage === 'version') {
      const v = take(12)
      if (!v) return
      send(new Uint8Array([1]))
      stage = 'security'
      continue
    }
    if (stage === 'security') {
      const s = take(2)
      if (!s) return
      const ok = take(4)
      if (!ok) { buf = concat(s, buf); return }
      send(new Uint8Array([1]))
      stage = 'init'
      continue
    }
    if (stage === 'init') {
      const head = take(24)
      if (!head) return
      const w = u16(head, 0)
      const h = u16(head, 2)
      const n = u32(head, 20)
      const name = take(n)
      if (!name) { buf = concat(head, buf); return }
      width.value = w
      height.value = h
      const canvas = canvasRef.value
      if (canvas) {
        canvas.width = w
        canvas.height = h
      }
      connected.value = true
      phase.value = 'connected'
      status.value = '已连接'
      requestFrame()
      stage = 'msg'
      canvasRef.value?.focus()
      continue
    }
    if (buf.length < 1) return
    const t = buf[0]
    if (t === 0) {
      if (buf.length < 4) return
      const n = u16(buf, 2)
      const rects = []
      let o = 4
      for (let i = 0; i < n; i++) {
        if (buf.length < o + 12) return
        const rw = u16(buf, o + 4)
        const rh = u16(buf, o + 6)
        const enc = u32(buf, o + 8)
        if (enc !== 0) throw new Error('encoding')
        const bytes = rw * rh * 4
        if (buf.length < o + 12 + bytes) return
        rects.push({ x: u16(buf, o), y: u16(buf, o + 2), rw, rh, o: o + 12, bytes })
        o += 12 + bytes
      }
      const frame = buf.slice(0, o)
      buf = buf.slice(o)
      draw(frame, rects)
      requestFrame()
      continue
    }
    if (t === 2) {
      buf = buf.slice(1)
      continue
    }
    if (t === 3) {
      if (buf.length < 8) return
      const n = u32(buf, 4)
      if (buf.length < 8 + n) return
      remoteClip = new TextDecoder().decode(buf.slice(8, 8 + n))
      buf = buf.slice(8 + n)
      status.value = '桌面剪贴板已更新'
      continue
    }
    return
  }
}

function concat(a, b) {
  const n = new Uint8Array(a.length + b.length)
  n.set(a, 0)
  n.set(b, a.length)
  return n
}

function draw(frame, rects) {
  const canvas = canvasRef.value
  if (!canvas) return
  const ctx = canvas.getContext('2d')
  for (const r of rects) {
    const img = ctx.createImageData(r.rw, r.rh)
    const src = frame.subarray(r.o, r.o + r.bytes)
    for (let i = 0; i < src.length; i += 4) {
      img.data[i] = src[i + 2]
      img.data[i + 1] = src[i + 1]
      img.data[i + 2] = src[i]
      img.data[i + 3] = 255
    }
    ctx.putImageData(img, r.x, r.y)
  }
}

function send(bytes) {
  if (ws && ws.readyState === 1) ws.send(bytes)
}

function requestFrame() {
  const msg = new Uint8Array(10)
  msg[0] = 3
  msg[6] = (width.value >> 8) & 255
  msg[7] = width.value & 255
  msg[8] = (height.value >> 8) & 255
  msg[9] = height.value & 255
  send(msg)
}

function pos(e) {
  const canvas = canvasRef.value
  const rect = canvas.getBoundingClientRect()
  const x = Math.max(0, Math.min(width.value - 1, Math.round((e.clientX - rect.left) * (width.value / rect.width))))
  const y = Math.max(0, Math.min(height.value - 1, Math.round((e.clientY - rect.top) * (height.value / rect.height))))
  return { x, y }
}

function sendPointer(mask, x, y) {
  const msg = new Uint8Array(6)
  msg[0] = 5
  msg[1] = mask & 0xff
  msg[2] = (x >> 8) & 255
  msg[3] = x & 255
  msg[4] = (y >> 8) & 255
  msg[5] = y & 255
  send(msg)
}

function onPointer(e) {
  const canvas = canvasRef.value
  if (!canvas || phase.value !== 'connected' || !width.value) return
  const { x, y } = pos(e)
  if (e.type === 'pointerdown') {
    canvas.setPointerCapture?.(e.pointerId)
    canvas.focus()
    buttons |= e.button === 2 ? 4 : e.button === 1 ? 2 : 1
  } else if (e.type === 'pointerup' || e.type === 'pointercancel' || e.type === 'pointerleave') {
    if (e.type === 'pointerleave' && buttons === 0) {
      sendPointer(0, x, y)
      return
    }
    buttons &= e.button === 2 ? ~4 : e.button === 1 ? ~2 : ~1
  }
  sendPointer(buttons, x, y)
}

function onWheel(e) {
  if (phase.value !== 'connected' || !width.value) return
  const { x, y } = pos(e)
  const bit = e.deltaY < 0 ? 8 : 16
  sendPointer(buttons | bit, x, y)
  sendPointer(buttons, x, y)
}

const KEYS = {
  Enter: 0xff0d, Backspace: 0xff08, Tab: 0xff09, Escape: 0xff1b,
  ArrowLeft: 0xff51, ArrowUp: 0xff52, ArrowRight: 0xff53, ArrowDown: 0xff54,
  Delete: 0xffff, Home: 0xff50, End: 0xff57, PageUp: 0xff55, PageDown: 0xff56,
  Shift: 0xffe1, Control: 0xffe3, Alt: 0xffe9, Meta: 0xffeb, ' ': 0x20
}

function sendKey(sym, down) {
  const msg = new Uint8Array(8)
  msg[0] = 4
  msg[1] = down ? 1 : 0
  msg[4] = (sym >>> 24) & 255
  msg[5] = (sym >>> 16) & 255
  msg[6] = (sym >>> 8) & 255
  msg[7] = sym & 255
  send(msg)
}

function sendCut(text) {
  const bytes = new TextEncoder().encode(text)
  const msg = new Uint8Array(8 + bytes.length)
  msg[0] = 6
  const n = bytes.length
  msg[4] = (n >>> 24) & 255
  msg[5] = (n >>> 16) & 255
  msg[6] = (n >>> 8) & 255
  msg[7] = n & 255
  msg.set(bytes, 8)
  send(msg)
}

function pasteText(text) {
  if (!text || phase.value !== 'connected') return
  sendCut(text)
  sendKey(0xffe3, true)
  sendKey(0x76, true)
  sendKey(0x76, false)
  sendKey(0xffe3, false)
  status.value = '已写入桌面剪贴板'
}

function onKey(e, down) {
  if (phase.value !== 'connected') return
  if (e.isComposing) return
  if ((e.ctrlKey || e.metaKey) && (e.key === 'c' || e.key === 'v' || e.key === 'C' || e.key === 'V')) return
  let sym = KEYS[e.key]
  if (!sym && e.key.length === 1) {
    const code = e.key.charCodeAt(0)
    if (code > 255) {
      status.value = '中文请用输入法或「粘贴」，这个屏幕没有中文按键码'
      return
    }
    sym = code
  }
  if (!sym) return
  e.preventDefault()
  sendKey(sym, down)
}

function onPaste(e) {
  const text = e.clipboardData?.getData('text/plain')
  if (!text) return
  e.preventDefault()
  pasteText(text)
}

function onCopy(e) {
  if (!remoteClip) return
  e.preventDefault()
  e.clipboardData?.setData('text/plain', remoteClip)
  status.value = '已复制桌面文字'
}

function onCompose(e) {
  if (e.data) pasteText(e.data)
}

async function copyOut() {
  if (!remoteClip) {
    status.value = '桌面剪贴板还是空的'
    return
  }
  try {
    await navigator.clipboard.writeText(remoteClip)
    status.value = '已复制到本机剪贴板'
  } catch (err) {
    status.value = '浏览器没有允许写入剪贴板'
  }
}

async function pasteIn() {
  try {
    const text = await navigator.clipboard.readText()
    pasteText(text)
  } catch (err) {
    status.value = '浏览器没有允许读取剪贴板，可在画面里直接粘贴'
  }
}

onBeforeUnmount(() => disconnect(false))
</script>

<style scoped>
.desk { display: flex; flex-direction: column; gap: 8px; min-height: 360px; height: 100%; }
.desk-bar { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.desk-btn {
  border: 1px solid var(--bp-line, #d7d8dc);
  background: var(--bp-surface, #fff);
  color: var(--bp-label, #1c1d1f);
  border-radius: 8px;
  padding: 6px 12px;
  cursor: pointer;
  transition: background 120ms ease, color 120ms ease, transform 120ms ease;
}
.desk-btn:hover { background: var(--bp-surface-fill, #f3f4f6); }
.desk-btn:active { transform: translateY(1px); }
.desk-btn:disabled { opacity: 0.45; cursor: default; transform: none; }
.desk-btn:focus-visible { outline: 2px solid var(--bp-accent, #3b6cff); outline-offset: 2px; }
.desk-status { font-size: 12px; color: var(--bp-label-secondary, #667); }
.desk-status.connected { color: var(--bp-success, #1b7f4a); }
.desk-status.failed, .desk-status.disconnected { color: var(--bp-danger, #b42318); }
.desk-note { margin: 0; font-size: 13px; line-height: 1.45; color: var(--bp-label-secondary, #667); }
.desk-stage {
  position: relative;
  flex: 1;
  min-height: 280px;
  overflow: auto;
  border-radius: 8px;
  background: #1c1d1f;
}
.desk-canvas {
  display: block;
  width: 100%;
  height: auto;
  cursor: crosshair;
  touch-action: none;
  background: #111;
}
.desk-canvas.actual { width: auto; height: auto; }
.desk-canvas:focus-visible { outline: 2px solid var(--bp-accent, #3b6cff); outline-offset: 2px; }
.desk-overlay {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #f4f5f7;
  font-size: 14px;
  pointer-events: none;
}
@media (prefers-reduced-motion: reduce) {
  .desk-btn, .desk-btn:active { transition: none; transform: none; }
}
</style>
