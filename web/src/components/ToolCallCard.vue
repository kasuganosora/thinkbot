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
            执行超时：连接可能已中断，结果未回传。请刷新对话或重新执行。
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
            <div v-if="genericHits.length" class="tc-hits">
              <div v-for="(hit, i) in genericHits" :key="i" class="tc-hit">
                <span class="tc-hit-title">{{ hit.title }}</span>
                <span v-if="hit.url" class="tc-hit-url">{{ hit.url }}</span>
              </div>
              <div v-if="genericHitsTruncated" class="cmd-note">（已截断）</div>
            </div>
            <div v-else-if="genericLines.length" class="tc-lines">
              <div
                v-for="(line, i) in genericLines"
                :key="i"
                class="tc-line"
                :class="{ 'is-error': genericIsError }"
              >{{ line }}</div>
            </div>
            <pre
              v-else-if="genericText"
              class="tc-kv-val"
              :class="{ 'is-error': genericIsError }"
            >{{ genericText }}</pre>
            <div v-else-if="state === 'running'" class="cmd-output running">执行中<span class="dots"><i>.</i><i>.</i><i>.</i></span></div>
            <div v-else class="tc-empty">无输出</div>
            <div v-if="genericTruncated && (genericText || genericLines.length)" class="cmd-note">（已截断）</div>
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

const HINT_KEYS = [
  'query', 'q', 'keyword', 'search',
  'url', 'href', 'finalURL', 'title',
  'path', 'name', 'expression', 'prompt', 'requirement', 'text',
]

const COMMAND_TOOLS = new Set(['exec', 'shell', 'run_command', 'bg_exec'])
const FILE_LIKE_TOOLS = new Set([
  'read_file', 'write_file', 'edit_file', 'replace_in_file',
  'list_dir', 'list_files', 'delete_file', 'move_file', 'search_content',
])

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

function looksLikeJsonBlob(s) {
  const t = String(s || '').trim()
  return (t.startsWith('{') && t.endsWith('}')) || (t.startsWith('[') && t.endsWith(']'))
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

function firstString(obj, keys) {
  if (!obj || typeof obj !== 'object' || Array.isArray(obj)) return ''
  for (const k of keys) {
    const v = obj[k]
    if (typeof v === 'string') {
      const t = v.trim()
      if (t && !looksLikeJsonBlob(t)) return t
    }
    if (typeof v === 'number' && Number.isFinite(v)) return String(v)
  }
  return ''
}

function isBrowserName(name) {
  return String(name || '').includes('browser__')
}

function isSearchName(name) {
  const k = toolKey(name)
  return k === 'web_search' || k === 'search_memory' || k === 'search_content' || k.endsWith('_search')
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
  const key = toolKey(props.call.name)
  const isShell = COMMAND_TOOLS.has(key) || key.endsWith('_exec')
  if (!isShell) return ''
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

const hasStructured = computed(() => {
  if (isCommand.value || hasFiles.value) return false
  if (isChoiceTool.value && !hasInlineChoice.value) return true
  const key = toolKey(props.call.name)
  if (!FILE_LIKE_TOOLS.has(key)) return false
  return !!structuredPath.value || !!contentPreview.value
})

const showGeneric = computed(() =>
  !isCommand.value && !hasFiles.value && !hasStructured.value && !isChoiceTool.value
)

const inputPretty = computed(() => prettyText(props.call.input))
const outputPretty = computed(() => prettyText(props.call.output))
const outputShown = computed(() => truncateText(compactText(props.call.output)))

const showDetailsToggle = computed(() =>
  (state.value === 'error' || state.value === 'timeout') && !!(inputPretty.value || outputPretty.value)
)

function collectHits(obj) {
  if (!obj || typeof obj !== 'object') return []
  const arr = obj.results || obj.entries || obj.hits || obj.items
  if (!Array.isArray(arr)) return []
  return arr.slice(0, 3).map((r) => {
    if (r == null) return null
    if (typeof r === 'string') return { title: r, url: '' }
    if (typeof r !== 'object') return { title: String(r), url: '' }
    const title = r.title || r.name || r.content || r.snippet || r.text || ''
    const url = r.url || r.href || r.link || ''
    if (!title && !url) return null
    return { title: String(title), url: String(url) }
  }).filter(Boolean)
}

function scalarLine(obj, keys) {
  if (!obj || typeof obj !== 'object') return ''
  for (const k of keys) {
    const v = obj[k]
    if (typeof v === 'string' && v.trim() && !looksLikeJsonBlob(v)) return v.trim()
    if (typeof v === 'number' && Number.isFinite(v)) return String(v)
    if (typeof v === 'boolean') return v ? 'true' : 'false'
  }
  return ''
}

const genericView = computed(() => {
  const out = outputObj.value
  const inp = inputObj.value
  const key = toolKey(props.call.name)
  const name = String(props.call.name || '')
  const empty = { kind: 'empty', hits: [], lines: [], text: '', truncated: false, hitsTruncated: false }

  const errStr = firstString(out, ['error', 'message', 'stderr'])
    || (typeof props.call.error === 'string' ? props.call.error : '')
  if ((state.value === 'error' || state.value === 'killed') && errStr) {
    const cut = truncateText(errStr)
    return { kind: 'error', hits: [], lines: cut.text.split('\n'), text: '', truncated: cut.truncated, hitsTruncated: false }
  }

  const rawHits = collectHits(out)
  const searchLike = isSearchName(name) || key === 'memory' || key === 'web_search'
    || Array.isArray(out?.results) || Array.isArray(out?.entries)
  if (rawHits.length && searchLike) {
    const src = out?.results || out?.entries || out?.hits || out?.items
    return {
      kind: 'hits',
      hits: rawHits,
      lines: [],
      text: '',
      truncated: false,
      hitsTruncated: Array.isArray(src) && src.length > 3,
    }
  }

  if (key === 'web_fetch' || name.endsWith('__fetch') || (out && (out.statusCode != null || out.finalURL || (out.status != null && (out.body != null || out.contentType))))) {
    const lines = []
    const title = firstString(out, ['title']) || firstString(inp, ['title'])
    const url = firstString(out, ['finalURL', 'url', 'href']) || firstString(inp, ['url', 'href'])
    const code = out?.statusCode != null ? String(out.statusCode) : (out?.status != null && String(out.status).match(/^\d+$/) ? String(out.status) : '')
    if (title) lines.push(title)
    if (url) lines.push(url)
    if (code) lines.push('HTTP ' + code)
    else if (out?.status != null && !code) lines.push(String(out.status))
    if (lines.length) return { kind: 'lines', hits: [], lines, text: '', truncated: false, hitsTruncated: false }
  }

  if (key === 'calculate' || (out && ('expression' in out || (inp && inp.expression != null && 'result' in out)))) {
    if (out?.error) {
      const cut = truncateText(String(out.error))
      return { kind: 'error', hits: [], lines: [cut.text], text: '', truncated: cut.truncated, hitsTruncated: false }
    }
    const expr = firstString(out, ['expression']) || firstString(inp, ['expression'])
    if (out && 'result' in out && out.result != null) {
      const line = expr ? `${expr} = ${out.result}` : String(out.result)
      return { kind: 'lines', hits: [], lines: [line], text: '', truncated: false, hitsTruncated: false }
    }
  }

  if (key === 'task' || key === 'task_status' || key === 'task_detail' || key === 'task_control') {
    const lines = []
    const st = out?.status || out?.Status
    const progress = out?.progress || out?.Progress
    const completed = progress?.completed ?? progress?.Completed ?? out?.completed
    const total = out?.nodeCount ?? out?.NodeCount ?? progress?.total ?? progress?.Total
    if (st) lines.push(String(st))
    if (completed != null && total != null) lines.push(`${completed}/${total}`)
    else if (completed != null) lines.push(String(completed))
    if (out?.timedOut || out?.TimedOut) lines.push('仍在后台运行')
    if (out?.error) lines.push(String(out.error))
    if (out?.hint) lines.push(String(out.hint))
    if (lines.length) return { kind: 'lines', hits: [], lines: lines.slice(0, PREVIEW_LINES), text: '', truncated: lines.length > PREVIEW_LINES, hitsTruncated: false }
  }

  if (key === 'cron') {
    const lines = [
      firstString(inp, ['action']),
      firstString(out, ['name']) || firstString(inp, ['name']),
      firstString(out, ['message', 'status']),
    ].filter(Boolean)
    if (lines.length) return { kind: 'lines', hits: [], lines, text: '', truncated: false, hitsTruncated: false }
  }

  if (key === 'memory' || key === 'search_memory') {
    const msg = firstString(out, ['message', 'error'])
    if (msg) return { kind: 'lines', hits: [], lines: [msg], text: '', truncated: false, hitsTruncated: false }
  }

  if (typeof props.call.output === 'string' && props.call.output && !looksLikeJsonBlob(props.call.output)) {
    const cut = truncateText(props.call.output)
    return { kind: 'text', hits: [], lines: [], text: cut.text, truncated: cut.truncated, hitsTruncated: false }
  }
  if (typeof props.call.output === 'number') {
    return { kind: 'lines', hits: [], lines: [String(props.call.output)], text: '', truncated: false, hitsTruncated: false }
  }

  const short = scalarLine(out, [
    'message', 'text', 'result', 'uuid', 'value', 'now', 'time', 'datetime',
    'hash', 'encoded', 'diff', 'hint', 'output',
  ])
  if (short) {
    const cut = truncateText(short)
    return { kind: 'text', hits: [], lines: [], text: cut.text, truncated: cut.truncated, hitsTruncated: false }
  }

  const mcpText = (() => {
    if (!out) return ''
    if (Array.isArray(out.content)) {
      return out.content.map((x) => {
        if (typeof x === 'string') return x
        if (x && typeof x === 'object') return x.text || x.title || ''
        return ''
      }).filter(Boolean).join('\n')
    }
    return ''
  })()
  if (mcpText && !looksLikeJsonBlob(mcpText)) {
    const cut = truncateText(mcpText)
    return { kind: 'text', hits: [], lines: [], text: cut.text, truncated: cut.truncated, hitsTruncated: false }
  }

  if (state.value === 'running') return empty
  return empty
})

const genericHits = computed(() => genericView.value.hits || [])
const genericHitsTruncated = computed(() => !!genericView.value.hitsTruncated)
const genericLines = computed(() => genericView.value.lines || [])
const genericText = computed(() => genericView.value.text || '')
const genericIsError = computed(() => genericView.value.kind === 'error')
const genericTruncated = computed(() => !!genericView.value.truncated)

const headHintFull = computed(() => {
  if (isChoiceTool.value) return ''
  if (structuredPath.value) return structuredPath.value
  if (cmdText.value) return cmdText.value
  if (props.call.summary && !looksLikeJsonBlob(props.call.summary)) return String(props.call.summary)

  const inp = inputObj.value
  const out = outputObj.value
  const key = toolKey(props.call.name)
  const name = String(props.call.name || '')
  const prefer = []

  if (isSearchName(name) || key === 'memory' || key === 'search_memory') {
    prefer.push('query', 'q', 'keyword', 'search')
  }
  if (isBrowserName(name) || key === 'web_fetch') {
    prefer.push('url', 'href', 'finalURL', 'title')
  }
  if (key === 'calculate') prefer.push('expression', 'result')
  if (key === 'task' || key === 'task_detail' || key === 'task_control' || key === 'task_status') {
    prefer.push('requirement', 'taskId', 'status')
  }
  if (key === 'cron') prefer.push('name', 'prompt', 'schedule', 'action')
  if (key === 'use_skill' || key === 'skill_trigger') prefer.push('command', 'name')
  if (key === 'spawn') prefer.push('system_prompt')
  if (key === 'send' || key === 'reply') prefer.push('text', 'content', 'message')
  prefer.push(...HINT_KEYS)

  const fromTasks = Array.isArray(inp?.tasks) && inp.tasks[0] != null ? String(inp.tasks[0]) : ''
  const picked = firstString(inp, prefer) || firstString(out, prefer) || fromTasks
  if (picked && !looksLikeJsonBlob(picked)) return picked
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
  padding: 4px 8px 6px;
  line-height: 1.45;
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

.tc-generic { padding: 4px 6px; display: flex; flex-direction: column; gap: 6px; }
.tc-hits { display: flex; flex-direction: column; gap: 6px; }
.tc-hit { display: flex; flex-direction: column; gap: 1px; min-width: 0; }
.tc-hit-title {
  font-size: 13px;
  color: var(--bp-label);
  line-height: 1.4;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tc-hit-url {
  font-size: 11.5px;
  color: var(--bp-label-tertiary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tc-lines { display: flex; flex-direction: column; gap: 2px; }
.tc-line {
  font-size: 13px;
  color: var(--bp-label-secondary);
  line-height: 1.45;
  word-break: break-word;
}
.tc-line.is-error { color: var(--bp-danger); }
.tc-kv { display: flex; flex-direction: column; gap: 3px; }
.tc-kv-label {
  font-size: 12px;
  font-weight: 500;
  color: var(--bp-label-secondary);
  letter-spacing: var(--bp-tracking-caption);
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
