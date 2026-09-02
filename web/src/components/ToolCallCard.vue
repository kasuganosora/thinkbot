<template>
  <div
    class="tool-call"
    :class="`tc-${state}`"
    :data-testid="`chat-toolcall-${call.id}`"
    :data-tool-name="call.name"
    :data-tool-status="state"
    :data-invocation-id="call.invocationId"
    role="group"
    :aria-label="`工具调用：${call.title || toolLabel(call.name) || call.name}`"
  >
    <!-- 头部摘要 -->
    <div class="tc-head" :data-testid="`chat-toolcall-head-${call.id}`" @click="toggle">
      <span class="tc-icon" :class="`icon-${state}`">
        <span v-if="state === 'running'" class="tc-spinner" />
        <t-icon v-else :name="headIcon" />
      </span>

      <!-- 执行中：标题用进行态文案 + 流动文字 -->
      <span class="tc-title" :data-testid="`chat-toolcall-title-${call.id}`">
        <template v-if="state === 'running'">
          <span class="tc-running-text">{{ runningLabel }}…</span>
        </template>
        <template v-else>{{ call.title || toolLabel(call.name) || call.name }}</template>
      </span>

      <span v-if="invocationShort" class="tc-inv" :data-testid="`chat-toolcall-inv-${call.id}`" :title="`调用标识 ${call.invocationId}`">#{{ invocationShort }}</span>
      <span v-if="state !== 'running' && call.summary" class="tc-summary">{{ call.summary }}</span>
      <span v-if="state !== 'running' && hasDiff" class="tc-diff">
        <span class="add">+{{ call.added || 0 }}</span>
        <span class="del">-{{ call.removed || 0 }}</span>
      </span>

      <div class="tc-actions">
        <t-tooltip v-if="call.reversible" content="撤销（待接入）">
          <t-button
            variant="text"
            size="small"
            :disabled="state === 'running'"
            class="tc-undo"
            :data-testid="`chat-toolcall-undo-${call.id}`"
            aria-label="撤销此操作（预留）"
            @click.stop="onUndo"
          >撤销<t-icon name="rollback" /></t-button>
        </t-tooltip>
        <t-icon
          :name="expanded ? 'chevron-up' : 'chevron-down'"
          class="tc-chevron"
          :data-testid="`chat-toolcall-toggle-${call.id}`"
          @click.stop="toggle"
        />
      </div>
    </div>

    <!-- 展开内容 -->
    <div v-show="expanded" class="tc-body" :data-testid="`chat-toolcall-body-${call.id}`">
      <!-- 卡死提示：running 超过阈值无更新，连接可能已中断 -->
      <div v-if="state === 'timeout'" class="tc-interrupt">
        ⚠️ 执行超时：连接可能已中断，结果未回传。请刷新对话或重新执行该命令。
      </div>
      <!-- 文件类工具 -->
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
        <span v-if="state !== 'running' || i < doneFileCount" class="file-diff">
          <span class="add">+{{ f.added || 0 }}</span>
          <span class="del">-{{ f.removed || 0 }}</span>
        </span>
        <t-icon name="jump" class="file-jump" aria-label="跳转到文件（预留）" />
      </div>

      <!-- 命令类工具 -->
      <div v-if="isCommand" class="tc-cmd" :data-testid="`chat-toolcall-cmd-${call.id}`">
        <div class="cmd-line"><span class="cmd-prompt">$</span> {{ cmdText }}</div>
        <div v-if="state === 'running' && !hasShellOutput" class="cmd-output running">执行中<span class="dots"><i>.</i><i>.</i><i>.</i></span></div>
        <template v-else-if="shellResult">
          <div v-if="shellResult.stdout" class="cmd-output">{{ shellResult.stdout }}</div>
          <div v-if="shellResult.stderr" class="cmd-output cmd-err">{{ shellResult.stderr }}</div>
          <div v-if="shellResult.truncated" class="cmd-note">（输出已截断）</div>
          <div v-if="state !== 'running' || shellResult.exitCode !== null || shellResult.workdir" class="cmd-meta">
            <span v-if="shellResult.exitCode !== null" :class="shellResult.exitCode === 0 ? 'exit-ok' : 'exit-fail'">exit {{ shellResult.exitCode }}</span>
            <span v-if="shellResult.workdir" class="cmd-cwd">cwd: {{ shellResult.workdir }}</span>
          </div>
        </template>
        <div v-else-if="outputText" class="cmd-output">{{ outputText }}</div>
      </div>

      <!-- 结构化文件类工具（read_file / write_file 等） -->
      <div v-else-if="hasStructured" class="tc-structured" :data-testid="`chat-toolcall-structured-${call.id}`">
        <div v-if="inputFields.length" class="tc-fields">
          <div v-for="f in inputFields" :key="'i-' + f.key" class="tc-field">
            <span class="tc-field-label">{{ f.label }}</span>
            <span class="tc-field-value">{{ f.value }}</span>
          </div>
        </div>

        <!-- user_choice 选项（防御性降级：ChoiceCard 未渲染时用户仍可见选项） -->
        <div v-if="choiceOptions.length && !hasInlineChoice" class="tc-choice-opts" data-testid="toolcall-choice-options">
          <div class="tc-field-label">选项</div>
          <div v-for="opt in choiceOptions" :key="opt.id" class="tc-choice-opt">
            <span class="tc-opt-id">{{ opt.id }}</span>
            <span class="tc-opt-label">{{ opt.label }}</span>
            <span v-if="opt.description" class="tc-opt-desc">{{ opt.description }}</span>
          </div>
        </div>

        <div v-if="contentPreview" class="tc-content">
          <div class="tc-kv-label">内容</div>
          <pre class="tc-kv-val">{{ contentPreview }}</pre>
        </div>

        <div v-if="state !== 'running' && resultFields.length" class="tc-fields tc-result-fields">
          <div v-for="f in resultFields" :key="'o-' + f.key" class="tc-field">
            <span class="tc-field-label">{{ f.label }}</span>
            <span class="tc-field-value" :class="{ 'is-ok': f.key === 'success' && f.value === '成功' }">{{ f.value }}</span>
          </div>
        </div>
        <div v-else-if="state === 'running'" class="cmd-output running">执行中<span class="dots"><i>.</i><i>.</i><i>.</i></span></div>

        <button v-if="outputText || inputText" class="tc-raw-toggle" @click="showRaw = !showRaw">
          {{ showRaw ? '隐藏原始数据' : '查看原始数据' }}
        </button>
        <div v-if="showRaw" class="tc-raw">
          <div v-if="inputText" class="tc-kv"><div class="tc-kv-label">原始参数</div><pre class="tc-kv-val">{{ inputText }}</pre></div>
          <div v-if="outputText" class="tc-kv"><div class="tc-kv-label">原始结果</div><pre class="tc-kv-val">{{ outputText }}</pre></div>
        </div>
      </div>

      <!-- 通用兜底：无法结构化时展示原始 JSON -->
      <div
        v-else-if="showGeneric"
        class="tc-generic"
        :data-testid="`chat-toolcall-generic-${call.id}`"
      >
        <div v-if="inputText" class="tc-kv">
          <div class="tc-kv-label">参数</div>
          <pre class="tc-kv-val">{{ inputText }}</pre>
        </div>
        <div v-if="state !== 'running' && outputText" class="tc-kv">
          <div class="tc-kv-label" :class="{ 'is-error': state === 'error' }">{{ state === 'error' ? '错误' : '结果' }}</div>
          <pre class="tc-kv-val" :class="{ 'is-error': state === 'error' }">{{ outputText }}</pre>
        </div>
        <div v-else-if="state === 'running'" class="cmd-output running">执行中<span class="dots"><i>.</i><i>.</i><i>.</i></span></div>
        <div v-if="!inputText && !outputText && state !== 'running'" class="tc-empty">无输出</div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import toolLabels from '@/i18n/toolLabels'
import { useBotStore } from '@/stores/bot'

const props = defineProps({
  call: { type: Object, required: true }
})
const store = useBotStore()
const hasInlineChoice = computed(() => !!store.choiceIdByToolCallId(props.call.id))

/** Resolve a tool name to its display label (Chinese). Returns null if unknown. */
function toolLabel(name) {
  // Strip sandbox_ prefix for unified labels (sandbox_read_file → read_file)
  const key = name?.replace(/^sandbox_/, '')
  return toolLabels[name] || (key !== name ? toolLabels[key] : null)
}

const expanded = ref(true)

// 卡死看门狗：后端流式连接断开时，工具调用会永久停在 running（"执行中"）。
// 若 running 状态超过阈值仍无更新，本地降级为 timeout，避免 UI 永久假死。
//
// 阈值不能一刀切：读文件几百毫秒，而容器里编译/跑测试、跑子工作流动辄几十分钟。
// 早期固定 3 分钟，导致长任务明明还在跑却被标成 timeout（纯前端误判）。
// 现在的判据是「多久没有任何活动」——每收到一次进度更新都会重置计时（见下面的
// watch），因此阈值只需覆盖「两次输出之间的最长静默」，可以给得比较宽松。
const DEFAULT_STUCK_TIMEOUT_MS = 15 * 60 * 1000
// 已知的长耗时工具：允许更长的静默期
const LONG_RUNNING_TIMEOUTS = {
  exec: 60 * 60 * 1000,
  bg_exec: 60 * 60 * 1000,
  task: 4 * 60 * 60 * 1000,
  task_status: 4 * 60 * 60 * 1000,
}

/** 本次调用允许的最长静默时间（可由 call.timeoutMs 显式指定） */
function stuckTimeoutMs() {
  const explicit = Number(props.call.timeoutMs)
  if (Number.isFinite(explicit) && explicit > 0) return explicit
  // sandbox_exec 与 exec 同源，去前缀后统一查表
  const key = String(props.call.name || '').replace(/^sandbox_/, '')
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
onMounted(armStuckTimer)
onUnmounted(clearStuckTimer)
// 每次收到新的 call 更新（流式 chunk / 最终 part）都重置计时器
watch(() => props.call, armStuckTimer)

const state = computed(() => {
  if (stuck.value && props.call.status === 'running') return 'timeout'
  // superseded = 被后续同名调用取代的 phantom call（LLM 流式中间态），不显示为错误
  return props.call.status || 'success'
})

// 执行标识短码：取 invocationId 去掉连字符后的前 8 位，用于在多步循环里
// 一眼区分「这是第几次 exec 调用」。完整值见 title 与 data-invocation-id。
const invocationShort = computed(() => {
  const id = props.call.invocationId
  if (!id) return ''
  return id.replace(/-/g, '').slice(0, 8)
})

const runningLabel = computed(() => {
  if (props.call.status === 'superseded') return '已取代'
  return props.call.runningText || props.call.title || toolLabel(props.call.name) || props.call.name || '执行中'
})

// 已完成的文件数（running 时全部显示为待处理/执行中，完成后全标完成）
const doneFileCount = computed(() => {
  const total = (props.call.files || []).length
  if (!total) return 0
  if (state.value !== 'running') return total
  return 0 // running 时全部显示"写入中"/pending，不做假进度
})

const hasDiff = computed(() =>
  typeof props.call.added === 'number' || typeof props.call.removed === 'number'
)

// --- 语义化回显：把后端返回的 input/output 解析成人类可读结构 ---

// 尝试把值解析为对象（兼容 JSON 字符串 / 已是对象）
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

const inputObj = computed(() => asObject(props.call.input))
const outputObj = computed(() => asObject(props.call.output))

// 命令行文本：优先 call.command，其次 shell 类工具的 input.command
const cmdText = computed(() => {
  if (props.call.command) return props.call.command
  const inp = inputObj.value
  if (inp) {
    if (typeof inp.command === 'string') return inp.command
    if (typeof inp.cmd === 'string') return inp.cmd
  }
  return ''
})

// shell 结果结构化：stdout / stderr / exitCode / workdir
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

// 原始输出文本（兜底 / 查看原文用）
const outputText = computed(() => {
  const o = props.call.output
  if (o == null) return ''
  if (typeof o === 'string') return o
  try { return JSON.stringify(o, null, 2) } catch { return String(o) }
})

// 文件类结果的人类可读摘要行（read_file / write_file 等）
const resultFields = computed(() => {
  const o = outputObj.value
  if (!o || shellResult.value) return []
  const labels = {
    path: '路径', size: '大小', lines: '行数', success: '状态',
    exists: '存在', name: '名称', count: '数量', total: '总数', mtime: '修改时间',
  }
  const fmt = (k, v) => {
    if (k === 'size' && typeof v === 'number') return v >= 1024 ? (v / 1024).toFixed(1) + ' KB' : v + ' B'
    if (k === 'success' || k === 'exists') return v ? '成功' : '否'
    return String(v)
  }
  const rows = []
  for (const [k, v] of Object.entries(o)) {
    if (v == null || typeof v === 'object') continue
    if (k === 'content' || k === 'data') continue
    rows.push({ key: k, label: labels[k] || k, value: fmt(k, v) })
  }
  return rows
})

// 文件内容预览（read/write 的 content）
const contentPreview = computed(() => {
  const from = (obj) => (obj && typeof obj.content === 'string') ? obj.content
    : (obj && typeof obj.data === 'string') ? obj.data : ''
  return from(outputObj.value) || from(inputObj.value) || ''
})

// 入参：结构化字段（排除已单独展示的 command / content）
const inputFields = computed(() => {
  const o = inputObj.value
  if (!o) return []
  const labels = { path: '路径', recursive: '递归', encoding: '编码', mode: '模式', timeout: '超时' }
  const rows = []
  for (const [k, v] of Object.entries(o)) {
    if (v == null || typeof v === 'object') continue
    if (k === 'command' || k === 'cmd' || k === 'content' || k === 'data') continue
    rows.push({ key: k, label: labels[k] || k, value: String(v) })
  }
  return rows
})

// user_choice 工具的选项列表（防御性降级：即使 ChoiceCard 因任何原因未渲染，
// 用户仍能在 ToolCallCard 中看到可选项。inputFields 跳过数组/对象，
// 导致 options 完全不可见——这是用户"这个卡片没有选项"投诉的直接根因之一。）
const choiceOptions = computed(() => {
  if (props.call.name !== 'user_choice') return []
  const opts = inputObj.value?.options
  if (!Array.isArray(opts)) return []
  return opts.filter(o => o && typeof o === 'object').map((o, i) => ({
    id: o.id ?? `o${i}`,
    label: o.label ?? `(选项 ${i + 1})`,
    description: o.description || '',
  }))
})

// 原始入参文本（兜底 / 无法结构化时）
const inputText = computed(() => {
  const inp = props.call.input
  if (inp == null || inp === '') return ''
  if (typeof inp === 'string') return inp
  try { return JSON.stringify(inp, null, 2) } catch { return String(inp) }
})

// 是否走命令块（shell）
const isCommand = computed(() => !!cmdText.value)

// 是否走结构化文件块（有可读字段或内容预览，且不是命令/文件列表）
const hasStructured = computed(() =>
  !isCommand.value && !(props.call.files && props.call.files.length) &&
  (inputFields.value.length > 0 || resultFields.value.length > 0 || !!contentPreview.value)
)

// 通用兜底：以上都不适用时，展示原始 JSON
const showGeneric = computed(() =>
  !isCommand.value && !(props.call.files && props.call.files.length) && !hasStructured.value
)

// 原始数据折叠状态
const showRaw = ref(false)

const headIcon = computed(() => {
  switch (state.value) {
    case 'success': return 'check-circle'
    case 'error': return 'error-circle'
    case 'killed':
    case 'timeout':
      return 'stop-circle'
    case 'superseded':
      return 'minus-circle' // 被取代的 phantom call
    default: return 'tools'
  }
})

function toggle() {
  expanded.value = !expanded.value
}

function fileStatusText(s) {
  return { modified: '已修改', added: '新增', deleted: '删除', renamed: '重命名' }[s] || s || ''
}

function onUndo() {
  MessagePlugin.info('撤销功能待后端接入')
}

</script>

<style scoped>
.tool-call {
  border: none;
  border-radius: var(--bp-radius-md);
  background: var(--bp-surface);
  overflow: hidden;
  margin-top: 8px;
}
.tc-success { }
.tc-error { background: var(--bp-danger-soft); }
.tc-running { background: var(--bp-accent-soft); }
.tc-killed, .tc-timeout { background: var(--bp-warning-soft); }
.tc-superseded { background: var(--bp-bg-subtle); }

.tc-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 9px 12px;
  cursor: pointer;
  font-size: 13px;
  transition: background var(--bp-duration) var(--bp-ease-out), transform var(--bp-duration) var(--bp-ease-out);
}
.tc-head:hover { background: var(--bp-surface-fill-hover); }
.tc-head:active { transform: scale(var(--bp-press-scale)); }
.tc-icon { display: flex; align-items: center; font-size: 15px; }
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

.tc-title { font-weight: 600; color: var(--bp-label); }
.tc-running-text {
  color: var(--bp-accent);
  background: linear-gradient(90deg, var(--bp-accent) 25%, var(--bp-accent-hover) 50%, var(--bp-accent) 75%);
  background-size: 200% 100%;
  -webkit-background-clip: text;
  background-clip: text;
  -webkit-text-fill-color: transparent;
  animation: flow 1.6s linear infinite;
}
.tc-summary { color: var(--bp-label-secondary); }
.tc-inv {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 11px;
  color: var(--bp-label-tertiary);
  background: var(--bp-surface-fill);
  border: none;
  border-radius: 4px;
  padding: 1px 5px;
  cursor: default;
  user-select: all;
}
.tc-diff { font-size: 12px; font-variant-numeric: tabular-nums; }
.tc-diff .add, .file-diff .add { color: var(--bp-success); margin-right: 6px; }
.tc-diff .del, .file-diff .del { color: var(--bp-danger); }

.tc-actions {
  margin-left: auto;
  display: flex;
  align-items: center;
  gap: 4px;
}
.tc-undo {
  color: var(--bp-label-tertiary);
  display: inline-flex;
  align-items: center;
}
.tc-undo :deep(.t-button__text) {
  display: inline-flex;
  align-items: center;
  gap: 3px;
}
.tc-undo :deep(.t-icon) {
  font-size: 14px;
  line-height: 1;
}
.tc-chevron {
  color: var(--bp-label-tertiary);
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  font-size: 16px;
  line-height: 1;
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
  border-radius: 6px;
  padding: 6px 10px;
  margin: 2px 4px 6px;
}
.tc-file {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 8px;
  border-radius: 6px;
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
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  color: var(--bp-label);
}
.file-status { font-size: 11px; padding: 0 6px; border-radius: 8px; background: var(--bp-surface-fill); color: var(--bp-label-secondary); }
.fs-added { background: var(--bp-success-soft); color: var(--bp-success); }
.fs-deleted { background: var(--bp-danger-soft); color: var(--bp-danger); }
.fs-doing { background: var(--bp-accent-soft); color: var(--bp-accent); }
.file-diff { font-size: 12px; font-variant-numeric: tabular-nums; flex-shrink: 0; }
.file-jump { color: var(--bp-label-quaternary); cursor: pointer; flex-shrink: 0; }
.file-jump:hover { color: var(--bp-accent); }

.tc-cmd {
  background: #1e1e22;
  border-radius: 6px;
  padding: 8px 10px;
  margin: 4px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
}
.cmd-line { color: #e6e6e6; }
.cmd-prompt { color: #00a870; margin-right: 6px; }
.cmd-output { color: #9aa0a6; margin-top: 4px; white-space: pre-wrap; word-break: break-word; max-height: 320px; overflow: auto; }
.cmd-output.running { color: #7aa7ff; }
.cmd-err { color: #ff9b9b; }
.cmd-note { color: #c9a24a; margin-top: 4px; font-size: 11px; }
.cmd-meta {
  margin-top: 6px;
  padding-top: 6px;
  border-top: 1px solid rgba(255, 255, 255, 0.08);
  display: flex;
  gap: 12px;
  font-size: 11px;
  color: #8a8f98;
}
.cmd-meta .exit-ok { color: #4caf82; }
.cmd-meta .exit-fail { color: #ff7b7b; }
.cmd-cwd { color: #7f8894; }
.dots i { animation: blink 1.2s infinite; }
.dots i:nth-child(2) { animation-delay: 0.2s; }
.dots i:nth-child(3) { animation-delay: 0.4s; }

/* 结构化文件类工具 */
.tc-structured { padding: 6px 8px; display: flex; flex-direction: column; gap: 10px; }
.tc-fields { display: flex; flex-direction: column; gap: 4px; }
.tc-field { display: flex; gap: 10px; font-size: 13px; align-items: baseline; }
.tc-field-label {
  flex-shrink: 0;
  min-width: 48px;
  color: var(--bp-label-secondary);
  font-size: 12px;
}
.tc-field-value {
  color: var(--bp-label);
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  word-break: break-word;
}
.tc-field-value.is-ok { color: var(--bp-success); font-weight: 600; }
.tc-result-fields { border-top: 1px dashed var(--bp-separator); padding-top: 8px; }
.tc-content { display: flex; flex-direction: column; gap: 3px; }
.tc-raw-toggle {
  align-self: flex-start;
  background: none;
  border: none;
  padding: 0;
  color: var(--bp-accent);
  font-size: 12px;
  cursor: pointer;
}
.tc-raw-toggle:hover { text-decoration: underline; }
.tc-raw { display: flex; flex-direction: column; gap: 8px; }

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
  border: none;
  border-radius: 6px;
  padding: 8px 10px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
  line-height: 1.5;
  color: var(--bp-label);
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 320px;
  overflow: auto;
}
.tc-kv-val.is-error { background: var(--bp-danger-soft); color: var(--bp-danger); }
.tc-empty { font-size: 12px; color: var(--bp-label-tertiary); padding: 4px 2px; }

/* user_choice 选项（ToolCallCard 防御性降级） */
.tc-choice-opts {
  margin-top: 4px;
}
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
  border-radius: 6px;
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
</style>
