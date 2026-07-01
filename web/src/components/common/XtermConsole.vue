<template>
  <div class="xc-wrap" data-testid="xterm-console">
    <div v-if="showHeader" class="xc-head">
      <div class="xc-left">
        <span class="xc-title">{{ title }}</span>
        <span class="xc-status" :class="{ ok: connected }">
          <i class="dot"></i>{{ connected ? '已连接' : '未连接' }}
        </span>
        <span v-if="host" class="xc-host">{{ host }}</span>
      </div>
      <div class="xc-actions">
        <t-button size="small" variant="outline" :loading="connecting" @click="reconnect">重连</t-button>
        <t-button size="small" variant="outline" @click="clearScreen">清屏</t-button>
      </div>
    </div>

    <div ref="termEl" class="xc-body" :style="{ height: bodyHeight }"></div>

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
  autoConnect: { type: Boolean, default: true }
})

const termEl = ref(null)
const connected = ref(false)
const connecting = ref(false)
const host = ref('')

let term = null
let fitAddon = null
let ro = null

let inputBuf = ''
let cwd = props.initialCwd
let busy = false
const history = []
let histIdx = -1

const GREEN = '\x1b[32m'
const BLUE = '\x1b[34m'
const RESET = '\x1b[0m'

function prompt() {
  const h = (host.value || props.initialHost || 'root@host').replace(/^.*@/, '')
  return `${GREEN}root@${h}${RESET}:${BLUE}${cwd}${RESET}$ `
}
function writePrompt() { term.write('\r\n' + prompt()) }

function clearScreen() {
  if (!term) return
  term.clear()
  term.write(prompt())
}

async function runCommand(cmd) {
  const trimmed = cmd.trim()
  if (!trimmed) { writePrompt(); return }
  history.push(trimmed)
  histIdx = history.length
  busy = true
  try {
    const res = await props.exec(trimmed)
    const output = res?.output
    if (output === '__CLEAR__') {
      term.clear()
    } else if (output) {
      term.write('\r\n' + String(output).replace(/\n/g, '\r\n'))
    }
    if (res?.cwd) cwd = res.cwd
  } catch (e) {
    term.write('\r\n' + `\x1b[31m${e?.message || '执行失败'}${RESET}`)
  } finally {
    busy = false
    writePrompt()
  }
}

function replaceLine(text) {
  term.write('\r' + '\x1b[K' + prompt())
  inputBuf = text
  term.write(text)
}

function handleData(data) {
  if (busy) return
  const code = data.charCodeAt(0)
  if (data === '\r') {
    const cmd = inputBuf
    inputBuf = ''
    runCommand(cmd)
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
    host.value = info?.host || props.initialHost
    cwd = info?.cwd || props.initialCwd
    connected.value = info?.connected !== false
    if (term) {
      if (info?.banner) term.write(`\x1b[90m${info.banner}${RESET}`)
      writePrompt()
    }
  } catch (e) {
    connected.value = false
    if (term) term.write(`\r\n\x1b[31m连接失败：${e?.message || '未知错误'}${RESET}`)
  } finally {
    connecting.value = false
  }
}

function reconnect() {
  if (!term) return
  term.clear()
  inputBuf = ''
  connect()
}

function fit() { try { fitAddon && fitAddon.fit() } catch (_) {} }

onMounted(async () => {
  await nextTick()
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
.xc-title { font-size: 15px; font-weight: 600; color: #1d1d1f; }
.xc-status { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; color: #999; }
.xc-status .dot { width: 7px; height: 7px; border-radius: 50%; background: #c0c4cc; display: inline-block; }
.xc-status.ok { color: #00a870; }
.xc-status.ok .dot { background: #00a870; }
.xc-host { font-size: 12px; color: #8a8a8a; font-family: Menlo, Monaco, monospace; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.xc-actions { display: flex; gap: 8px; flex-shrink: 0; }

.xc-body {
  background: #1e1e2e;
  border-radius: 10px;
  padding: 10px 12px;
  overflow: hidden;
}
.xc-body :deep(.xterm) { height: 100%; }
.xc-body :deep(.xterm-viewport) { background: transparent !important; }

.xc-tip { margin-top: 10px; font-size: 12px; color: #999; }
.xc-tip :deep(code) {
  background: #f2f3f5; padding: 1px 6px; border-radius: 4px;
  font-family: Menlo, Monaco, monospace; color: #444;
}
</style>
