<template>
  <div class="sc-wrap" data-testid="session-console">
    <div class="sc-head">
      <div class="sc-left">
        <span class="sc-title">交互式会话</span>
        <span class="sc-sid" :title="sessionId">{{ shortId }}</span>
        <span class="sc-status" :class="statusClass" role="status">
          <i class="dot"></i>{{ statusText }}
        </span>
        <span v-if="orphaned" class="sc-orphan" title="agent 回合已结束，但会话进程仍在后台运行">已脱离回合</span>
      </div>
      <div class="sc-actions">
        <t-button size="small" variant="outline" :loading="connecting" @click="reconnect">重连</t-button>
        <t-button size="small" variant="outline" @click="clearScreen">清屏</t-button>
        <t-button size="small" theme="danger" variant="outline" :loading="closing" @click="requestClose">关闭会话</t-button>
      </div>
    </div>

    <div ref="termEl" class="sc-body"></div>

    <div v-if="lastError" class="sc-error" role="alert">{{ lastError }}</div>
    <div class="sc-tip">
      人工接管：输入会直接发给后台 PTY，与 agent 共用同一屏幕。关闭标签页不会结束会话，只有「关闭会话」才会。
    </div>
  </div>
</template>

<script setup>
import { ref, computed, watch, onMounted, onBeforeUnmount, nextTick } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { botPtySessionApi } from '@/api/services'

// 与 XtermConsole.vue 刻意分开：后者是「一条命令一次 HTTP」的本地行编辑器
// （本地回显 / 自造提示符 / 假 Ctrl+C），接到真 PTY 会双回显并吞掉方向键。
const props = defineProps({
  botId: { type: String, required: true },
  sessionId: { type: String, required: true },
  orphaned: { type: Boolean, default: false }
})
const emit = defineEmits(['closed'])

const termEl = ref(null)
const connected = ref(false)
const connecting = ref(false)
const closing = ref(false)
const lastError = ref('')
const termState = ref('running')

const shortId = computed(() => (props.sessionId || '').slice(0, 12))
const statusText = computed(() => {
  if (connecting.value) return '连接中'
  if (!connected.value) return termState.value === 'running' ? '未连接（会话仍在后台运行）' : '连接已断开'
  if (termState.value === 'exited') return '已退出'
  if (termState.value === 'lost') return '已丢失'
  return '已连接'
})
const statusClass = computed(() => ({
  ok: connected.value && termState.value === 'running',
  working: connecting.value || termState.value === 'lost'
}))

let term = null
let fitAddon = null
let ws = null
let ro = null
let disposed = false
let lastSentCols = 0
let lastSentRows = 0

function toBase64(text) {
  const bytes = new TextEncoder().encode(text)
  let bin = ''
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000))
  }
  return btoa(bin)
}

function sendFrame(obj) {
  if (!ws || ws.readyState !== WebSocket.OPEN) return false
  try {
    ws.send(JSON.stringify(obj))
    return true
  } catch (_) {
    return false
  }
}

function onTermData(data) {
  // Ctrl+C 就是 \x03 字节，由 PTY 行规转成前台进程组 SIGINT；
  // 绝不能映射成 sandbox_session_signal(INT)——实测那会直接终止会话。
  sendFrame({ type: 'input', data: toBase64(data) })
}

function clearScreen() {
  if (term) term.clear()
}

function fit() {
  try {
    if (termEl.value?.clientWidth && termEl.value?.clientHeight) fitAddon?.fit()
  } catch (_) { /* 容器尚未布局完成时忽略 */ }
}

// ResizeObserver fires for every layout tick, and fit() itself changes xterm's
// DOM. Without a change guard this becomes a feedback loop (observed in a real
// browser: 4434 frames, rows growing 31 -> 3286). Only emit when the terminal
// grid actually changed; on (re)connect force one frame because the server
// tracks last-applied size per socket, not per session.
function sendResize(force = false) {
  fit()
  if (!term) return
  const cols = term.cols
  const rows = term.rows
  if (!force && cols === lastSentCols && rows === lastSentRows) return
  if (sendFrame({ type: 'resize', cols, rows })) {
    lastSentCols = cols
    lastSentRows = rows
  }
}

function onResize() {
  sendResize()
}

function handleControl(text) {
  let f
  try { f = JSON.parse(text) } catch (_) { return }
  if (f.type === 'reset') {
    term?.reset()
  } else if (f.type === 'state') {
    termState.value = f.state || 'running'
  } else if (f.type === 'error') {
    lastError.value = f.message || '会话流出错'
  }
}

function disconnect() {
  if (ws) {
    // 只关闭 socket：会话由 SessionRegistry 拥有，不能随标签页一起结束。
    ws.onopen = null
    ws.onmessage = null
    ws.onerror = null
    ws.onclose = null
    try { ws.close() } catch (_) { /* already closed */ }
    ws = null
  }
  connected.value = false
  lastSentCols = 0
  lastSentRows = 0
}

function connect() {
  if (disposed || !props.botId || !props.sessionId) return
  disconnect()
  connecting.value = true
  lastError.value = ''
  let socket
  try {
    socket = new WebSocket(botPtySessionApi.streamUrl(props.botId, props.sessionId))
  } catch (e) {
    connecting.value = false
    lastError.value = `无法建立连接：${e?.message || '未知错误'}`
    return
  }
  ws = socket
  socket.binaryType = 'arraybuffer'
  socket.onopen = () => {
    if (disposed || ws !== socket) return
    connecting.value = false
    connected.value = true
    term?.reset() // 服务端会从 seq=0 重放，先清空避免叠加
    sendResize(true)
  }
  socket.onmessage = (ev) => {
    if (disposed || ws !== socket || !term) return
    if (typeof ev.data === 'string') {
      handleControl(ev.data)
    } else {
      term.write(new Uint8Array(ev.data))
    }
  }
  socket.onerror = () => {
    if (disposed || ws !== socket) return
    lastError.value = '连接异常，可点「重连」继续观察（会话进程不受影响）'
  }
  socket.onclose = () => {
    if (disposed || ws !== socket) return
    connected.value = false
    connecting.value = false
  }
}

function reconnect() {
  connect()
}

async function requestClose() {
  if (closing.value) return
  closing.value = true
  lastError.value = ''
  try {
    await botPtySessionApi.remove(props.botId, props.sessionId)
    termState.value = 'exited'
    disconnect()
    emit('closed', props.sessionId)
  } catch (e) {
    lastError.value = `关闭失败：${e?.message || '未知错误'}`
  } finally {
    closing.value = false
  }
}

watch(() => props.sessionId, () => {
  termState.value = 'running'
  lastError.value = ''
  connect()
})

onMounted(async () => {
  await nextTick()
  if (disposed) return
  term = new Terminal({
    cursorBlink: true,
    fontSize: 13,
    fontFamily: 'Menlo, Monaco, "Courier New", monospace',
    theme: {
      background: '#1e1e2e',
      foreground: '#e4e4e7',
      cursor: '#e4e4e7',
      selectionBackground: '#3b3b52'
    },
    convertEol: false,
    scrollback: 2000
  })
  fitAddon = new FitAddon()
  term.loadAddon(fitAddon)
  term.open(termEl.value)
  fit()
  term.onData(onTermData)
  ro = new ResizeObserver(() => onResize())
  ro.observe(termEl.value)
  connect()
})

onBeforeUnmount(() => {
  disposed = true
  disconnect()
  if (ro) { ro.disconnect(); ro = null }
  if (term) { term.dispose(); term = null }
})

defineExpose({ reconnect, fit, clearScreen })
</script>

<style scoped>
.sc-wrap { width: 100%; display: flex; flex-direction: column; min-width: 0; }
.sc-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 12px; gap: 12px; }
.sc-left { display: flex; align-items: center; gap: 10px; min-width: 0; }
.sc-title { font-size: 15px; font-weight: 600; color: var(--bp-label); }
.sc-sid { font-size: 12px; color: var(--bp-label-tertiary); font-family: Menlo, Monaco, monospace; }
.sc-status { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; color: var(--bp-label-tertiary); white-space: nowrap; }
.sc-status .dot { width: 7px; height: 7px; border-radius: 50%; background: var(--bp-label-quaternary); display: inline-block; }
.sc-status.ok { color: var(--bp-success); }
.sc-status.ok .dot { background: var(--bp-success); }
.sc-status.working { color: var(--bp-accent); }
.sc-status.working .dot { background: var(--bp-accent); }
.sc-orphan { font-size: 12px; color: var(--bp-warning, #d9821b); white-space: nowrap; }
.sc-actions { display: flex; gap: 8px; flex-shrink: 0; }
.sc-body {
  background: #1e1e2e;
  border-radius: 10px;
  padding: 10px 12px;
  /* MUST be a fixed height. With height:auto + .xterm{height:100%} the
     container grows with every extra terminal row, fit() then asks for one
     more row and ResizeObserver fires again -> unbounded resize feedback
     loop (observed in browser: rows 31 -> 3286). */
  height: 460px;
  box-sizing: border-box;
  overflow: hidden;
}
.sc-body :deep(.xterm) { height: 100%; }
.sc-body :deep(.xterm-viewport) { background: transparent !important; }
.sc-error { margin-top: 8px; font-size: 12px; color: var(--bp-danger, #d54941); }
.sc-tip { margin-top: 8px; font-size: 12px; color: var(--bp-label-tertiary); line-height: 1.6; }
</style>
