<template>
  <div
    ref="rootRef"
    class="tool-call"
    :class="[`tc-${state}`, { 'tc-quiet': isQuietChip }]"
    :data-testid="`chat-toolcall-${call.id}`"
    :data-tool-name="call.name"
    :data-tool-status="state"
    :data-invocation-id="call.invocationId"
    role="group"
    :aria-label="`工具调用：${displayTitle}`"
  >
    <div
      class="tc-head"
      :class="{ 'is-static': !canExpand }"
      :data-testid="`chat-toolcall-head-${call.id}`"
      :role="canExpand ? 'button' : undefined"
      :tabindex="canExpand ? 0 : undefined"
      :aria-expanded="canExpand ? expanded : undefined"
      @click="onHeadClick"
      @pointerdown="onHeadPointerDown"
      @keydown.enter.prevent="onHeadClick"
      @keydown.space.prevent="onHeadClick"
    >
      <span class="tc-icon" :class="`icon-${state}`">
        <span v-if="state === 'running'" class="tc-spinner" />
        <t-icon v-else :name="headIcon" />
      </span>

      <span class="tc-title" :data-testid="`chat-toolcall-title-${call.id}`">
        <template v-if="state === 'running'">
          <span class="tc-running-text">{{ displayTitle }}</span>
        </template>
        <template v-else>{{ displayTitle }}</template>
      </span>

      <span v-if="headHint" class="tc-summary" :title="headHintFull">{{ headHint }}</span>
      <span v-if="state !== 'running' && hasDiff" class="tc-diff">
        <span v-if="diffAdded" class="add">+{{ call.added }}</span>
        <span v-if="diffRemoved" class="del">-{{ call.removed }}</span>
      </span>

      <div v-if="canExpand" class="tc-actions">
        <t-icon
          :name="expanded ? 'chevron-up' : 'chevron-down'"
          class="tc-chevron"
          :data-testid="`chat-toolcall-toggle-${call.id}`"
          aria-hidden="true"
        />
      </div>
    </div>

    <div
      v-if="canExpand"
      class="tc-body-wrap"
      :class="{ 'is-open': expanded }"
      :data-testid="`chat-toolcall-body-${call.id}`"
    >
      <div class="tc-body-inner">
        <div class="tc-body">
          <div v-if="state === 'timeout'" class="tc-interrupt">
            ⚠️ 执行超时：连接可能已中断，结果未回传。请刷新对话或重新执行该命令。
          </div>

          <div
            v-for="(f, i) in (call.files || [])"
            :key="i"
            class="tc-file"
            :class="{ 'file-doing': state === 'running' && i >= doneFileCount }"
            :data-testid="`chat-toolcall-file-${call.id}-${i}`"
            :data-file-path="f.path"
          >
            <span v-if="state === 'running' && i === doneFileCount" class="file-spinner" />
            <t-icon v-else-if="state === 'running' && i > doneFileCount" name="time" class="file-icon pending" />
            <t-icon v-else name="file" class="file-icon" />
            <span class="file-path">{{ f.path }}</span>
            <span v-if="state !== 'running' || i < doneFileCount" class="file-status" :class="`fs-${f.status}`">{{ fileStatusText(f.status) }}</span>
            <span v-else-if="i === doneFileCount" class="file-status fs-doing">写入中…</span>
            <span v-if="(state !== 'running' || i < doneFileCount) && fileHasDiff(f)" class="file-diff">
              <span v-if="typeof f.added === 'number' && f.added !== 0" class="add">+{{ f.added }}</span>
              <span v-if="typeof f.removed === 'number' && f.removed !== 0" class="del">-{{ f.removed }}</span>
            </span>
          </div>

          <div v-if="isCommand" class="tc-cmd" :data-testid="`chat-toolcall-cmd-${call.id}`">
            <div class="cmd-line"><span class="cmd-prompt">$</span> {{ cmdText }}</div>
            <div v-if="state === 'running' && !hasShellOutput" class="cmd-output running">执行中<span class="dots"><i>.</i><i>.</i><i>.</i></span></div>
            <template v-else-if="shellResult">
              <div v-if="stdoutView.text" class="cmd-output">{{ stdoutView.text }}</div>
              <div v-if="stderrView.text" class="cmd-output cmd-err">{{ stderrView.text }}</div>
              <div v-if="stdoutView.truncated || stderrView.truncated || shellResult.truncated" class="cmd-note">（输出已截断）</div>
              <div v-if="shellResult.exitCode !== null || cmdCwd" class="cmd-meta">
                <span v-if="shellResult.exitCode !== null" :class="shellResult.exitCode === 0 ? 'exit-ok' : 'exit-fail'">exit {{ shellResult.exitCode }}</span>
                <span v-if="cmdCwd" class="cmd-cwd">cwd: {{ cmdCwd }}</span>
              </div>
            </template>
            <div v-else-if="outputShown.text" class="cmd-output">{{ outputShown.text }}</div>
          </div>

          <div v-else-if="hasStructured" class="tc-structured" :data-testid="`chat-toolcall-structured-${call.id}`">
            <div v-if="structuredPath" class="tc-field">
              <span class="tc-field-label">路径</span>
              <span class="tc-field-value" :title="structuredPath">{{ structuredPath }}</span>
            </div>

            <div v-if="choiceOptions.length && !hasInlineChoice" class="tc-choice-opts" data-testid="toolcall-choice-options">
              <div class="tc-field-label">选项</div>
              <div v-for="opt in choiceOptions" :key="opt.id" class="tc-choice-opt">
                <span class="tc-opt-id">{{ opt.id }}</span>
                <span class="tc-opt-label">{{ opt.label }}</span>
                <span v-if="opt.description" class="tc-opt-desc">{{ opt.description }}</span>
              </div>
            </div>

            <div v-if="contentShown.text" class="tc-content">
              <div class="tc-kv-label">内容</div>
              <pre class="tc-kv-val">{{ contentShown.text }}</pre>
              <div v-if="contentShown.truncated" class="cmd-note">（内容已截断）</div>
            </div>

            <div v-if="state === 'running' && !structuredPath && !contentShown.text" class="cmd-output running">执行中<span class="dots"><i>.</i><i>.</i><i>.</i></span></div>

          </div>

          <div
            v-else-if="showGeneric"
            class="tc-generic"
            :data-testid="`chat-toolcall-generic-${call.id}`"
          >
            <div v-if="inputShown.text" class="tc-kv">
              <div class="tc-kv-label">参数</div>
              <pre class="tc-kv-val">{{ inputShown.text }}</pre>
              <div v-if="inputShown.truncated" class="cmd-note">（已截断）</div>
            </div>
            <div v-if="state !== 'running' && outputShown.text" class="tc-kv">
              <div class="tc-kv-label" :class="{ 'is-error': state === 'error' }">{{ state === 'error' ? '错误' : '结果' }}</div>
              <pre class="tc-kv-val" :class="{ 'is-error': state === 'error' }">{{ outputShown.text }}</pre>
              <div v-if="outputShown.truncated" class="cmd-note">（已截断）</div>
            </div>
            <div v-else-if="state === 'running'" class="cmd-output running">执行中<span class="dots"><i>.</i><i>.</i><i>.</i></span></div>
            <div v-if="!inputShown.text && !outputShown.text && state !== 'running'" class="tc-empty">无输出</div>

          </div>

          <button
            v-if="showDetailsToggle"
            type="button"
            class="tc-raw-toggle"
            @click.stop="showRaw = !showRaw"
          >{{ showRaw ? '收起' : '详情' }}</button>
          <div v-if="showRaw" class="tc-raw">
            <div v-if="inputPretty" class="tc-kv"><div class="tc-kv-label">原始参数</div><pre class="tc-kv-val">{{ inputPretty }}</pre></div>
            <div v-if="outputPretty" class="tc-kv"><div class="tc-kv-label">原始结果</div><pre class="tc-kv-val">{{ outputPretty }}</pre></div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import toolLabels from '@/i18n/toolLabels'
import { useBotStore } from '@/stores/bot'
import { animateSpring, prefersReducedMotion } from '@/utils/spring'

const props = defineProps({
  call: { type: Object, required: true }
})
const store = useBotStore()
const rootRef = ref(null)
const hasInlineChoice = computed(() => !!store.choiceIdByToolCallId(props.call.id))

function toolKey(name) {
  return String(name || '').replace(/^sandbox_/, '')
}

function toolLabel(name) {
  const key = toolKey(name)
  return toolLabels[name] || (key !== name ? toolLabels[key] : null)
}

const PREVIEW_CHARS = 800
const PREVIEW_LINES = 12
const HINT_CHARS = 48

function asObject(v) {
  if (v == null) return null
  if (typeof v === 'object') return v
  if (typeof v === 'string') {
    const s = v.trim()
    if ((s.startsWith('{') && s.endsWith('}')) || (s.startsWith('[') && s.endsWith(']'))) {
      try { return JSON.parse(s) } catch { return null }
    }
  }
  return null
}

function prettyText(v) {
  if (v == null || v === '') return ''
  if (typeof v === 'string') return v
  try { return JSON.stringify(v, null, 2) } catch { return String(v) }
}

function compactText(v) {
  if (v == null || v === '') return ''
  if (typeof v === 'string') return v
  try { return JSON.stringify(v) } catch { return String(v) }
}

function truncateText(s, maxChars = PREVIEW_CHARS, maxLines = PREVIEW_LINES) {
  if (!s) return { text: '', truncated: false }
  const raw = String(s)
  const lines = raw.split('\n')
  let truncated = false
  let text = raw
  if (lines.length > maxLines) {
    text = lines.slice(0, maxLines).join('\n')
    truncated = true
  }
  if (text.length > maxChars) {
    text = text.slice(0, maxChars)
    truncated = true
  }
  return { text, truncated }
}

function truncateOneLine(s, n = HINT_CHARS) {
  if (!s) return ''
  const t = String(s).replace(/\s+/g, ' ').trim()
  return t.length > n ? t.slice(0, n - 1) + '…' : t
}

function fileHasDiff(f) {
  if (!f) return false
  return (typeof f.added === 'number' && f.added !== 0) || (typeof f.removed === 'number' && f.removed !== 0)
}

function fileStatusText(s) {
  return { modified: '已修改', added: '新增', deleted: '删除', renamed: '重命名' }[s] || s || ''
}

const expanded = ref(false)
const userToggled = ref(false)
const showRaw = ref(false)

const DEFAULT_STUCK_TIMEOUT_MS = 15 * 60 * 1000
const LONG_RUNNING_TIMEOUTS = {
  exec: 60 * 60 * 1000,
  bg_exec: 60 * 60 * 1000,
  task: 4 * 60 * 60 * 1000,
  task_status: 4 * 60 * 60 * 1000,
}

function stuckTimeoutMs() {
  const explicit = Number(props.call.timeoutMs)
  if (Number.isFinite(explicit) && explicit > 0) return explicit
  const key = toolKey(props.call.name)
  return LONG_RUNNING_TIMEOUTS[key] || DEFAULT_STUCK_TIMEOUT_MS
}

const stuck = ref(false)
let stuckTimer = null
function clearStuckTimer() {
  if (stuckTimer) { clearTimeout(stuckTimer); stuckTimer = null }
  stuck.value = false
}
function armStuckTimer() {
  clearStuckTimer()
  if (props.call.status === 'running') {
    stuckTimer = setTimeout(() => { stuck.value = true }, stuckTimeoutMs())
  }
}

const state = computed(() => {
  if (stuck.value && props.call.status === 'running') return 'timeout'
  return props.call.status || 'success'
})

const isChoiceTool = computed(() => toolKey(props.call.name) === 'user_choice')
const isQuietChip = computed(() => isChoiceTool.value && hasInlineChoice.value)
const canExpand = computed(() => !isQuietChip.value)

const runningLabel = computed(() => {
  if (props.call.status === 'superseded') return '已取代'
  return props.call.runningText || props.call.title || toolLabel(props.call.name) || props.call.name || '执行中'
})

const displayTitle = computed(() => {
  if (isChoiceTool.value) {
    if (state.value === 'running') return '正在提问'
    if (state.value === 'timeout') return '提问超时'
    if (state.value === 'error' || state.value === 'killed') return '提问失败'
    if (state.value === 'success' || state.value === 'superseded') return '已提问'
    return '向你提问'
  }
  if (state.value === 'running') return runningLabel.value + '…'
  return props.call.title || toolLabel(props.call.name) || props.call.name
})

const doneFileCount = computed(() => {
  const total = (props.call.files || []).length
  if (!total) return 0
  if (state.value !== 'running') return total
  return 0
})

const diffAdded = computed(() => typeof props.call.added === 'number' && props.call.added !== 0)
const diffRemoved = computed(() => typeof props.call.removed === 'number' && props.call.removed !== 0)
const hasDiff = computed(() => diffAdded.value || diffRemoved.value)

const inputObj = computed(() => asObject(props.call.input))
const outputObj = computed(() => asObject(props.call.output))

const cmdText = computed(() => {
  if (props.call.command) return props.call.command
  const inp = inputObj.value
  if (inp) {
    if (typeof inp.command === 'string') return inp.command
    if (typeof inp.cmd === 'string') return inp.cmd
  }
  return ''
})

const shellResult = computed(() => {
  const o = outputObj.value
  if (!o) return null
  const has = ['stdout', 'stderr', 'exitCode'].some(k => k in o)
  if (!has) return null
  return {
    stdout: typeof o.stdout === 'string' ? o.stdout : '',
    stderr: typeof o.stderr === 'string' ? o.stderr : '',
    exitCode: typeof o.exitCode === 'number' ? o.exitCode : null,
    workdir: typeof o.workdir === 'string' ? o.workdir : '',
    truncated: o.truncated === true,
  }
})
const hasShellOutput = computed(() => !!(shellResult.value && (shellResult.value.stdout || shellResult.value.stderr)))
const stdoutView = computed(() => truncateText(shellResult.value?.stdout || ''))
const stderrView = computed(() => truncateText(shellResult.value?.stderr || ''))
const cmdCwd = computed(() => {
  const w = shellResult.value?.workdir
  if (!w || typeof w !== 'string') return ''
  const t = w.trim()
  return t && t !== '.' ? t : ''
})

const isCommand = computed(() => !!cmdText.value)
const hasFiles = computed(() => !!(props.call.files && props.call.files.length))

const LIVE_OUTPUT_TOOLS = new Set([
  'exec', 'shell', 'run_command', 'bg_exec',
  'read_file', 'write_file', 'edit_file', 'replace_in_file', 'delete_file',
])
const isLiveOutput = computed(() => {
  if (isCommand.value || hasFiles.value) return true
  return LIVE_OUTPUT_TOOLS.has(toolKey(props.call.name))
})

function shouldAutoExpand() {
  if (isChoiceTool.value) return false
  if (state.value === 'running') return true
  if (state.value === 'timeout' && isLiveOutput.value) return true
  return false
}

watch(
  () => [state.value, isChoiceTool.value, isLiveOutput.value],
  () => {
    if (userToggled.value) return
    expanded.value = shouldAutoExpand()
  },
  { immediate: true }
)

const structuredPath = computed(() => {
  const pick = (obj) => {
    if (!obj || typeof obj !== 'object') return ''
    for (const k of ['path', 'file', 'filepath', 'filename', 'dir', 'directory']) {
      if (typeof obj[k] === 'string' && obj[k]) return obj[k]
    }
    return ''
  }
  return pick(inputObj.value) || pick(outputObj.value) || ''
})

const contentPreview = computed(() => {
  if (isChoiceTool.value) return ''
  const from = (obj) => (obj && typeof obj.content === 'string') ? obj.content
    : (obj && typeof obj.data === 'string') ? obj.data : ''
  return from(outputObj.value) || from(inputObj.value) || ''
})
const contentShown = computed(() => truncateText(contentPreview.value))

const choiceOptions = computed(() => {
  if (!isChoiceTool.value) return []
  const opts = inputObj.value?.options
  if (!Array.isArray(opts)) return []
  return opts.filter(o => o && typeof o === 'object').map((o, i) => ({
    id: o.id ?? `o${i}`,
    label: o.label ?? `(选项 ${i + 1})`,
    description: o.description || '',
  }))
})

const hasStructured = computed(() =>
  !isCommand.value && !hasFiles.value &&
  (!!structuredPath.value || !!contentPreview.value || (isChoiceTool.value && !hasInlineChoice.value))
)

const showGeneric = computed(() =>
  !isCommand.value && !hasFiles.value && !hasStructured.value && !isChoiceTool.value
)

const inputPretty = computed(() => prettyText(props.call.input))
const outputPretty = computed(() => prettyText(props.call.output))
const inputShown = computed(() => truncateText(compactText(props.call.input)))
const outputShown = computed(() => truncateText(compactText(props.call.output)))

const showDetailsToggle = computed(() =>
  (state.value === 'error' || state.value === 'timeout') && !!(inputPretty.value || outputPretty.value)
)

const headHintFull = computed(() => {
  if (isChoiceTool.value) return ''
  if (structuredPath.value) return structuredPath.value
  if (cmdText.value) return cmdText.value
  if (props.call.summary) return String(props.call.summary)
  return ''
})
const headHint = computed(() => truncateOneLine(headHintFull.value, HINT_CHARS))

const headIcon = computed(() => {
  switch (state.value) {
    case 'success': return 'check-circle'
    case 'error': return 'error-circle'
    case 'killed':
    case 'timeout':
      return 'stop-circle'
    case 'superseded':
      return 'minus-circle'
    default: return 'tools'
  }
})

function onHeadClick() {
  if (!canExpand.value) return
  userToggled.value = true
  expanded.value = !expanded.value
}

const pressing = ref(false)
let stopSpring = null
function scaleTo(el, to, response) {
  if (!el) return
  if (stopSpring) stopSpring()
  stopSpring = animateSpring({
    el,
    from: 1,
    to,
    damping: 1.0,
    response,
    disabled: prefersReducedMotion(),
    onUpdate: (v) => { el.style.transform = 'scale(' + v + ')' },
  })
}
function onHeadPointerDown(e) {
  if (prefersReducedMotion()) return
  if (!canExpand.value) return
  if (e.button != null && e.button !== 0) return
  const el = rootRef.value
  if (!el) return
  pressing.value = true
  scaleTo(el, 0.97, 0.16)
}
function onGlobalPointerUp() {
  if (!pressing.value) return
  pressing.value = false
  const el = rootRef.value
  if (el) scaleTo(el, 1, 0.4)
}

onMounted(() => {
  armStuckTimer()
  window.addEventListener('pointerup', onGlobalPointerUp, { passive: true })
  window.addEventListener('pointercancel', onGlobalPointerUp, { passive: true })
})
onUnmounted(() => {
  clearStuckTimer()
  if (stopSpring) stopSpring()
  window.removeEventListener('pointerup', onGlobalPointerUp)
  window.removeEventListener('pointercancel', onGlobalPointerUp)
})
watch(() => props.call, armStuckTimer)
</script>

<style scoped>
.tool-call {
  border: none;
  border-radius: var(--bp-radius-md);
  background: var(--bp-surface);
  overflow: hidden;
  margin-top: 8px;
  will-change: transform;
}
.tc-success { }
.tc-error { background: var(--bp-danger-soft); }
.tc-running { background: var(--bp-accent-soft); }
.tc-killed, .tc-timeout { background: var(--bp-warning-soft); }
.tc-superseded { background: var(--bp-bg-subtle); }
.tool-call.tc-quiet {
  background: var(--bp-surface-fill);
  margin-top: 8px;
  margin-bottom: 4px;
}

.tc-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 9px 12px;
  cursor: pointer;
  font-size: 13px;
  user-select: none;
  transition: background var(--bp-duration) var(--bp-ease-out);
}
.tc-head:hover { background: var(--bp-surface-fill-hover); }
.tc-head.is-static {
  cursor: default;
}
.tc-quiet .tc-head {
  padding: 6px 10px;
}
.tc-quiet .tc-head:hover { background: transparent; }
.tc-icon { display: flex; align-items: center; font-size: 15px; flex-shrink: 0; }
.icon-success { color: var(--bp-success); }
.icon-error { color: var(--bp-danger); }
.icon-superseded { color: var(--bp-label-tertiary); }

.tc-spinner, .file-spinner {
  width: 13px;
  height: 13px;
  border: 2px solid var(--bp-accent-soft-strong);
  border-top-color: var(--bp-accent);
  border-radius: 50%;
  display: inline-block;
  animation: spin 0.7s linear infinite;
}
.file-spinner { width: 12px; height: 12px; flex-shrink: 0; }

.tc-title {
  font-weight: 600;
  color: var(--bp-label);
  flex-shrink: 0;
  letter-spacing: var(--bp-tracking-body);
}
.tc-running-text {
  color: var(--bp-accent);
  background: linear-gradient(90deg, var(--bp-accent) 25%, var(--bp-accent-hover) 50%, var(--bp-accent) 75%);
  background-size: 200% 100%;
  -webkit-background-clip: text;
  background-clip: text;
  -webkit-text-fill-color: transparent;
  animation: flow 1.6s linear infinite;
}
.tc-summary {
  color: var(--bp-label-secondary);
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 12.5px;
  font-weight: 400;
}
.tc-diff { font-size: 12px; font-variant-numeric: tabular-nums; flex-shrink: 0; }
.tc-diff .add, .file-diff .add { color: var(--bp-success); margin-right: 6px; }
.tc-diff .del, .file-diff .del { color: var(--bp-danger); }

.tc-actions {
  margin-left: auto;
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
}
.tc-chevron {
  color: var(--bp-label-tertiary);
  display: inline-flex;
  align-items: center;
  font-size: 16px;
  line-height: 1;
  pointer-events: none;
}

.tc-body-wrap {
  display: grid;
  grid-template-rows: 0fr;
  overflow: hidden;
  transition: grid-template-rows var(--bp-duration) var(--bp-ease-out);
}
.tc-body-wrap.is-open { grid-template-rows: 1fr; }
.tc-body-inner {
  min-height: 0;
  overflow: hidden;
}

.tc-body {
  border-top: var(--bp-hairline);
  padding: 6px 8px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.tc-interrupt {
  font-size: 12px;
  color: var(--bp-warning);
  background: var(--bp-warning-soft);
  border: none;
  border-radius: var(--bp-radius-sm);
  padding: 6px 10px;
  margin: 2px 4px 6px;
}
.tc-file {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 8px;
  border-radius: var(--bp-radius-sm);
  font-size: 13px;
}
.tc-file:hover { background: var(--bp-surface-fill-hover); }
.tc-file.file-doing { opacity: 0.85; }
.file-icon { color: var(--bp-label-tertiary); flex-shrink: 0; }
.file-icon.pending { color: var(--bp-label-quaternary); }
.file-path {
  flex: 1;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  font-family: var(--bp-font-mono);
  color: var(--bp-label);
}
.file-status { font-size: 11px; padding: 0 6px; border-radius: 8px; background: var(--bp-surface-fill); color: var(--bp-label-secondary); }
.fs-added { background: var(--bp-success-soft); color: var(--bp-success); }
.fs-deleted { background: var(--bp-danger-soft); color: var(--bp-danger); }
.fs-doing { background: var(--bp-accent-soft); color: var(--bp-accent); }
.file-diff { font-size: 12px; font-variant-numeric: tabular-nums; flex-shrink: 0; }

.tc-cmd {
  background: var(--bp-surface-fill);
  border: var(--bp-hairline);
  border-radius: var(--bp-radius-sm);
  padding: 8px 10px;
  margin: 4px;
  font-family: var(--bp-font-mono);
  font-size: 12px;
  color: var(--bp-label);
}
.cmd-line { color: var(--bp-label); word-break: break-word; }
.cmd-prompt { color: var(--bp-label-secondary); margin-right: 6px; }
.cmd-output {
  color: var(--bp-label-secondary);
  margin-top: 4px;
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 18em;
  overflow: auto;
}
.cmd-output.running { color: var(--bp-accent); }
.cmd-err { color: var(--bp-danger); }
.cmd-note { color: var(--bp-label-tertiary); margin-top: 4px; font-size: 11px; }
.cmd-meta {
  margin-top: 6px;
  padding-top: 6px;
  border-top: var(--bp-hairline);
  display: flex;
  gap: 12px;
  font-size: 11px;
  color: var(--bp-label-tertiary);
}
.cmd-meta .exit-ok { color: var(--bp-success); }
.cmd-meta .exit-fail { color: var(--bp-danger); }
.cmd-cwd { color: var(--bp-label-tertiary); }
.dots i { animation: blink 1.2s infinite; }
.dots i:nth-child(2) { animation-delay: 0.2s; }
.dots i:nth-child(3) { animation-delay: 0.4s; }

.tc-structured { padding: 6px 8px; display: flex; flex-direction: column; gap: 10px; }
.tc-field { display: flex; gap: 10px; font-size: 13px; align-items: baseline; }
.tc-field-label {
  flex-shrink: 0;
  min-width: 48px;
  color: var(--bp-label-secondary);
  font-size: 12px;
}
.tc-field-value {
  color: var(--bp-label);
  font-family: var(--bp-font-mono);
  word-break: break-word;
}
.tc-content { display: flex; flex-direction: column; gap: 3px; }
.tc-raw-toggle {
  align-self: flex-start;
  background: none;
  border: none;
  padding: 0;
  color: var(--bp-label-tertiary);
  font-size: 12px;
  cursor: pointer;
  font-family: inherit;
  margin: 4px 8px 6px;
}
.tc-raw-toggle:hover { color: var(--bp-accent); }
.tc-raw { display: flex; flex-direction: column; gap: 8px; padding: 0 8px 8px; }

.tc-generic { padding: 4px 6px; display: flex; flex-direction: column; gap: 8px; }
.tc-kv { display: flex; flex-direction: column; gap: 3px; }
.tc-kv-label {
  font-size: 11px;
  font-weight: 600;
  color: var(--bp-label-secondary);
  text-transform: uppercase;
  letter-spacing: 0.03em;
}
.tc-kv-label.is-error { color: var(--bp-danger); }
.tc-kv-val {
  margin: 0;
  background: var(--bp-surface-fill);
  border: var(--bp-hairline);
  border-radius: var(--bp-radius-sm);
  padding: 8px 10px;
  font-family: var(--bp-font-mono);
  font-size: 12px;
  line-height: 1.5;
  color: var(--bp-label);
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 18em;
  overflow: auto;
}
.tc-kv-val.is-error { background: var(--bp-danger-soft); color: var(--bp-danger); }
.tc-empty { font-size: 12px; color: var(--bp-label-tertiary); padding: 4px 2px; }

.tc-choice-opts { margin-top: 4px; }
.tc-choice-opt {
  display: flex;
  align-items: baseline;
  gap: 8px;
  padding: 3px 0;
  font-size: 13px;
  line-height: 1.45;
}
.tc-opt-id {
  flex-shrink: 0;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 22px;
  height: 22px;
  border-radius: var(--bp-radius-xs);
  background: var(--bp-accent-soft);
  color: var(--bp-accent);
  font-size: 11.5px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}
.tc-opt-label {
  color: var(--bp-label);
  font-weight: 500;
}
.tc-opt-desc {
  color: var(--bp-label-tertiary);
  font-size: 12px;
  margin-left: auto;
}

@keyframes spin { to { transform: rotate(360deg); } }
@keyframes flow { to { background-position: -200% 0; } }
@keyframes blink { 0%, 100% { opacity: 0.2; } 50% { opacity: 1; } }

@media (prefers-reduced-motion: reduce) {
  .tc-body-wrap { transition: none; }
  .tc-head { transition: none; }
  .tc-spinner, .file-spinner { animation: none; border-top-color: var(--bp-accent); opacity: 0.7; }
  .tc-running-text {
    animation: none;
    background: none;
    -webkit-text-fill-color: var(--bp-accent);
    color: var(--bp-accent);
  }
  .dots i { animation: none; opacity: 1; }
}
</style>
