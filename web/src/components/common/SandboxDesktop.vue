<template>
  <div class="desk" data-testid="sandbox-desktop">
    <div class="desk-bar">
      <button type="button" class="desk-btn" @click="toggle">
        {{ connected ? '关闭桌面' : '打开桌面' }}
      </button>
      <span class="desk-status" :class="{ ok: connected }">{{ status }}</span>
    </div>
    <p v-if="!connected" class="desk-note">
      打开后看到的是这个 Bot 沙箱里的画面，点击和键盘会送到那一块屏幕，不会连到别的 Bot。
    </p>
    <canvas
      v-show="connected"
      ref="canvasRef"
      class="desk-canvas"
      tabindex="0"
      @pointerdown="onPointer"
      @pointermove="onPointer"
      @pointerup="onPointer"
      @pointerleave="onPointer"
      @keydown="onKey($event, true)"
      @keyup="onKey($event, false)"
      @contextmenu.prevent
    />
  </div>
</template>

<script setup>
import { onBeforeUnmount, ref } from 'vue'

const props = defineProps({
  botId: { type: String, required: true },
  // session routes share the chat workspace auth; bot routes are the settings page
  scope: { type: String, default: 'bot' }
})

const canvasRef = ref(null)
const connected = ref(false)
const status = ref('未连接')
let ws = null
let buf = new Uint8Array(0)
let stage = 'version'
let width = 0
let height = 0
let buttons = 0

function urlFor(id) {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const path = props.scope === 'session'
    ? `/api/sessions/${encodeURIComponent(id)}/desktop`
    : `/api/bots/${encodeURIComponent(id)}/desktop`
  return `${proto}//${location.host}${path}`
}

function toggle() {
  if (connected.value) disconnect()
  else connect()
}

function connect() {
  disconnect()
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
      status.value = '画面解析失败'
      disconnect()
    }
  }
  ws.onerror = () => { status.value = '桌面连接失败' }
  ws.onclose = () => {
    connected.value = false
    if (status.value === '正在连接' || status.value === '已连接') status.value = '已断开'
  }
}

function disconnect() {
  if (ws) {
    ws.onclose = null
    try { ws.close() } catch (_) {}
    ws = null
  }
  connected.value = false
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
      width = u16(head, 0)
      height = u16(head, 2)
      const n = u32(head, 20)
      const name = take(n)
      if (!name) { buf = concat(head, buf); return }
      const canvas = canvasRef.value
      if (canvas) {
        canvas.width = width
        canvas.height = height
      }
      connected.value = true
      status.value = '已连接'
      requestFrame()
      stage = 'msg'
      canvasRef.value?.focus()
      continue
    }
    if (buf.length < 1) return
    const t = buf[0]
    if (t === 0) {
      if (buf.length < 16) return
      const n = u16(buf, 2)
      let need = 4
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
        need = o
      }
      const frame = buf.slice(0, need)
      buf = buf.slice(need)
      draw(frame, rects)
      requestFrame()
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
  msg[6] = (width >> 8) & 255
  msg[7] = width & 255
  msg[8] = (height >> 8) & 255
  msg[9] = height & 255
  send(msg)
}

function onPointer(e) {
  const canvas = canvasRef.value
  if (!canvas || !connected.value) return
  const rect = canvas.getBoundingClientRect()
  const x = Math.max(0, Math.min(width - 1, Math.round((e.clientX - rect.left) * (width / rect.width))))
  const y = Math.max(0, Math.min(height - 1, Math.round((e.clientY - rect.top) * (height / rect.height))))
  if (e.type === 'pointerdown') {
    canvas.setPointerCapture?.(e.pointerId)
    canvas.focus()
    buttons |= e.button === 2 ? 4 : e.button === 1 ? 2 : 1
  } else if (e.type === 'pointerup' || e.type === 'pointerleave') {
    buttons &= e.button === 2 ? ~4 : e.button === 1 ? ~2 : ~1
  }
  const msg = new Uint8Array(6)
  msg[0] = 5
  msg[1] = buttons & 0xff
  msg[2] = (x >> 8) & 255
  msg[3] = x & 255
  msg[4] = (y >> 8) & 255
  msg[5] = y & 255
  send(msg)
}

const KEYS = {
  Enter: 0xff0d, Backspace: 0xff08, Tab: 0xff09, Escape: 0xff1b,
  ArrowLeft: 0xff51, ArrowUp: 0xff52, ArrowRight: 0xff53, ArrowDown: 0xff54,
  Delete: 0xffff, Home: 0xff50, End: 0xff57
}

function onKey(e, down) {
  if (!connected.value) return
  e.preventDefault()
  let sym = KEYS[e.key]
  if (!sym && e.key.length === 1) sym = e.key.charCodeAt(0)
  if (!sym) return
  const msg = new Uint8Array(8)
  msg[0] = 4
  msg[1] = down ? 1 : 0
  msg[4] = (sym >>> 24) & 255
  msg[5] = (sym >>> 16) & 255
  msg[6] = (sym >>> 8) & 255
  msg[7] = sym & 255
  send(msg)
}

onBeforeUnmount(disconnect)
</script>

<style scoped>
.desk { display: flex; flex-direction: column; gap: 8px; min-height: 0; height: 100%; }
.desk-bar { display: flex; align-items: center; gap: 10px; }
.desk-btn {
  border: 1px solid var(--bp-line, #d7d8dc);
  background: var(--bp-surface, #fff);
  color: var(--bp-label, #1c1d1f);
  border-radius: 8px;
  padding: 6px 12px;
  cursor: pointer;
}
.desk-btn:focus-visible { outline: 2px solid var(--bp-accent, #3b6cff); outline-offset: 2px; }
.desk-status { font-size: 12px; color: var(--bp-label-secondary, #667); }
.desk-status.ok { color: var(--bp-success, #1b7f4a); }
.desk-note { margin: 0; font-size: 13px; line-height: 1.45; color: var(--bp-label-secondary, #667); }
.desk-canvas {
  width: 100%;
  height: min(480px, 70vh);
  background: #111;
  border-radius: 8px;
  cursor: crosshair;
  touch-action: none;
}
.desk-canvas:focus-visible { outline: 2px solid var(--bp-accent, #3b6cff); outline-offset: 2px; }
@media (prefers-reduced-motion: reduce) {
  .desk-btn { transition: none; }
}
</style>
