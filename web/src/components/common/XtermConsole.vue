<template>
  <div class="xc-wrap" data-testid="xterm-console">
    <div v-if="showHeader" class="xc-head">
      <div class="xc-left">
        <span class="xc-title">{{ title }}</span>
        <span class="xc-status" :class="{ ok: connected && !busy, working: connecting || busy }" role="status">
          <i class="dot"></i>{{ connecting ? '连接中' : busy ? '执行中' : connected ? '已连接' : '未连接' }}
        </span>
        <span v-if="host" class="xc-host">{{ host }}</span>
      </div>
      <div class="xc-actions">
        <t-button v-if="busy && allowInterrupt" size="small" theme="danger" variant="outline" :disabled="interruptRequested" @click="requestInterrupt">{{ interruptRequested ? '已请求中断' : '请求中断' }}</t-button>
        <t-button size="small" variant="outline" :loading="connecting" :disabled="busy" @click="reconnect">重连</t-button>
        <t-button size="small" variant="outline" @click="clearScreen">清屏</t-button>
      </div>
    </div>

    <div ref="termEl" class="xc-body" :style="{ height: bodyHeight }"></div>

    <div v-if="lastError" class="xc-error" role="alert">{{ lastError }}</div>
    <div v-if="interruptNotice" class="xc-tip" role="status">{{ interruptNotice }}</div>
    <div v-if="tip || $slots.tip" class="xc-tip"><slot name="tip">{{ tip }}</slot></div>
  </div>
</template>

<script setup>
import { ref, onMounted, onBeforeUnmount, nextTick } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

const props = defineProps({
  // 建立连接：() => Promise<{ host, cwd, connected, banner }>
  connect: { type: Function, default: null },
  // 执行命令：(cmd) => Promise<{ output, cwd }>；output 为 '__CLEAR__' 时清屏
  exec: { type: Function, required: true },
  title: { type: String, default: '终端' },
  showHeader: { type: Boolean, default: true },
  tip: { type: String, default: '' },
  bodyHeight: { type: String, default: '440px' },
  // 未提供 connect 时使用的初始提示符 host（形如 root@host）
  initialHost: { type: String, default: 'root@host' },
  initialCwd: { type: String, default: '~' },
  autoConnect: { type: Boolean, default: true },
  allowInterrupt: { type: Boolean, default: false }
})

const termEl = ref(null)
const connected = ref(false)
const connecting = ref(false)
const host = ref('')
const busy = ref(false)
const lastError = ref('')
const interruptNotice = ref('')
const interruptRequested = ref(false)
let commandController = null

let term = null
let fitAddon = null
let ro = null
let disposed = false

let inputBuf = ''
let cwd = props.initialCwd
const inputQueue = []
let processingInput = false
const history = []
let histIdx = -1

const GREEN = '\x1b[32m'
const BLUE = '\x1b[34m'
const RESET = '\x1b[0m'

function prompt() {
  const h = (host.value || props.initialHost || 'root@host').replace(/^.*@/, '')
  return `${GREEN}root@${h}${RESET}:${BLUE}${cwd}${RESET}$ `
}
function writePrompt() { term?.write('\r\n' + prompt()) }

function clearScreen() {
  if (!term) return
  term.clear()
  term.write('\x1b[2J\x1b[H' + (busy.value ? '' : prompt() + inputBuf))
}

async function runCommand(cmd) {
  if (disposed || !term) return
  const trimmed = cmd.trim()
  if (!trimmed) { writePrompt(); return }
  history.push(trimmed)
  histIdx = history.length
  busy.value = true
  lastError.value = ''
  interruptNotice.value = ''
  interruptRequested.value = false
  const controller = new AbortController()
  commandController = controller
  try {
    const res = await props.exec(trimmed, { signal: controller.signal })
    if (disposed || !term) return
    const output = res?.output
    if (output === '__CLEAR__') {
      term.clear()
    } else if (output) {
      term.write('\r\n' + String(output).replace(/\n/g, '\r\n'))
    }
    if (res?.cwd) cwd = res.cwd
    if (res?.aborted || res?.reliable === false) {
      interruptNotice.value = res?.aborted ? '命令已被中断，输出可能不完整。' : '本次执行结果未能完整确认。'
    } else if (res?.exitCode != null && res.exitCode !== 0) {
      lastError.value = `命令以退出码 ${res.exitCode} 结束，请检查输出。`
    }
    if (res?.truncated) interruptNotice.value += (interruptNotice.value ? ' ' : '') + '输出过长，已截断。'
    if (Array.isArray(res?.warnings) && res.warnings.length) interruptNotice.value += (interruptNotice.value ? ' ' : '') + res.warnings.join('；')
  } catch (e) {
    if (disposed || !term) return
    if (controller.signal.aborted) {
      interruptNotice.value = '已请求中断，远端命令的最终状态尚未确认。'
      term.write('\r\n\x1b[33m[已请求中断，执行结果未确认]' + RESET)
    } else {
      lastError.value = e?.message || '执行失败，请检查命令后重试'
      term.write('\r\n' + `\x1b[31m${e?.message || '执行失败'}${RESET}`)
    }
  } finally {
    busy.value = false
    if (commandController === controller) commandController = null
    if (!disposed && term) writePrompt()
  }
}

function requestInterrupt() {
  if (!props.allowInterrupt || !busy.value || interruptRequested.value) return
  interruptRequested.value = true
  inputQueue.length = 0
  interruptNotice.value = '已请求中断，正在等待执行结束。'
  commandController?.abort()
}

function replaceLine(text) {
  term.write('\r' + '\x1b[K' + prompt())
  inputBuf = text
  term.write(text)
}

function handleData(data) {
  if (disposed || !term || connecting.value || !connected.value) return
  if (busy.value && data.charCodeAt(0) === 3) {
    inputQueue.length = 0
    if (props.allowInterrupt) requestInterrupt()
    else interruptNotice.value = '命令仍在执行，当前终端暂不支持中断。'
    return
  }
  inputQueue.push(data)
  drainInputQueue()
}

// A single consumer owns both pasted commands and keys typed while they run.
// This prevents concurrent executions from overwriting each other's controller.
async function drainInputQueue() {
  if (processingInput) return
  processingInput = true
  try {
    while (!disposed && inputQueue.length) await processInput(inputQueue.shift())
  } finally { processingInput = false }
}

async function processInput(data) {
  if (disposed || !term) return
  // 粘贴（多字符且含换行）：逐行顺序执行；单字符 \r 走下方回车逻辑
  if (data.length > 1 && (data.includes('\n') || data.includes('\r'))) {
    const lines = data.split(/\r\n|\r|\n/)
    // 将执行前已输入的缓冲拼到第一行，避免粘贴时丢失已打字内容
    if (inputBuf && lines.length) lines[0] = inputBuf + (lines[0] || '')
    inputBuf = ''
    for (const line of lines) {
      if (disposed) return
      if (line.length) await runCommand(line)
      if (interruptRequested.value) return
    }
    return
  }
  const code = data.charCodeAt(0)
  if (data === '\r') {
    const cmd = inputBuf
    inputBuf = ''
    await runCommand(cmd)
    return
  }
  if (code === 127 || code === 8) {
    if (inputBuf.length > 0) {
      inputBuf = inputBuf.slice(0, -1)
      term.write('\b \b')
    }
    return
  }
  if (code === 3) { // Ctrl+C
    inputBuf = ''
    term.write('^C')
    writePrompt()
    return
  }
  if (code === 12) { clearScreen(); return } // Ctrl+L
  if (data === '\x1b[A') { // ↑
    if (history.length && histIdx > 0) { histIdx--; replaceLine(history[histIdx]) }
    return
  }
  if (data === '\x1b[B') { // ↓
    if (histIdx < history.length - 1) { histIdx++; replaceLine(history[histIdx]) }
    else { histIdx = history.length; replaceLine('') }
    return
  }
  if (code === 27) return // 其它转义序列
  inputBuf += data
  term.write(data)
}

async function connect() {
  if (disposed || connecting.value || busy.value) return
  lastError.value = ''
  if (!props.connect) {
    // 无 connect：直接用初始提示符本地开跑
    host.value = props.initialHost
    cwd = props.initialCwd
    connected.value = true
    if (term) writePrompt()
    return
  }
  connecting.value = true
  try {
    const info = await props.connect()
    if (disposed) return
    host.value = info?.host || props.initialHost
    cwd = info?.cwd || props.initialCwd
    connected.value = info?.connected !== false
    if (term) {
      if (info?.banner) term.write(`\x1b[90m${info.banner}${RESET}`)
      writePrompt()
    }
  } catch (e) {
    if (disposed) return
    connected.value = false
    lastError.value = `连接失败：${e?.message || '请重试'}`
    if (term) term.write(`\r\n\x1b[31m连接失败：${e?.message || '未知错误'}${RESET}`)
  } finally {
    connecting.value = false
  }
}

function reconnect() {
  if (!term || busy.value || connecting.value) return
  // Retain previous output when reconnecting so failed commands remain inspectable.
  inputBuf = ''
  inputQueue.length = 0
  connect()
}

function fit() { try { if (termEl.value?.clientWidth && termEl.value?.clientHeight) fitAddon?.fit() } catch (_) {} }

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
  term.onData(handleData)
  ro = new ResizeObserver(() => fit())
  ro.observe(termEl.value)
  if (props.autoConnect) connect()
})

onBeforeUnmount(() => {
  disposed = true
  inputQueue.length = 0
  if (ro) { ro.disconnect(); ro = null }
  if (term) { term.dispose(); term = null }
})

defineExpose({ reconnect, clearScreen, fit })
</script>

<style scoped>
.xc-wrap { width: 100%; display: flex; flex-direction: column; min-width: 0; }

.xc-head {
  display: flex; align-items: center; justify-content: space-between;
  margin-bottom: 12px;
}
.xc-left { display: flex; align-items: center; gap: 12px; min-width: 0; }
.xc-title { font-size: 15px; font-weight: 600; color: var(--bp-label); }
.xc-status { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; color: var(--bp-label-tertiary); }
.xc-status .dot { width: 7px; height: 7px; border-radius: 50%; background: var(--bp-label-quaternary); display: inline-block; }
.xc-status.ok { color: var(--bp-success); }
.xc-status.ok .dot { background: var(--bp-success); }
.xc-status.working { color: var(--bp-accent); }
.xc-status.working .dot { background: var(--bp-accent); }
.xc-host { font-size: 12px; color: var(--bp-label-tertiary); font-family: Menlo, Monaco, monospace; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.xc-actions { display: flex; gap: 8px; flex-shrink: 0; }

.xc-body {
  background: #1e1e2e;
  border-radius: 10px;
  padding: 10px 12px;
  overflow: hidden;
}
.xc-body :deep(.xterm) { height: 100%; }
.xc-body :deep(.xterm-viewport) { background: transparent !important; }

.xc-tip { margin-top: 10px; font-size: 12px; color: var(--bp-label-tertiary); }
.xc-error { margin-top: 8px; font-size: 12px; line-height: 1.5; color: var(--bp-danger); overflow-wrap: anywhere; }
.xc-tip :deep(code) {
  background: var(--bp-surface-fill); padding: 1px 6px; border-radius: 4px;
  font-family: Menlo, Monaco, monospace; color: var(--bp-label);
}
</style>
