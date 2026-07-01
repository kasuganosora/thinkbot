<template>
  <div
    class="tool-call"
    :class="`tc-${state}`"
    :data-testid="`chat-toolcall-${call.id}`"
    :data-tool-name="call.name"
    :data-tool-status="state"
    role="group"
    :aria-label="`工具调用：${call.title || call.name}`"
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
        <template v-else>{{ call.title || call.name }}</template>
      </span>

      <span v-if="state !== 'running' && call.summary" class="tc-summary">{{ call.summary }}</span>
      <span v-if="state !== 'running' && hasDiff" class="tc-diff">
        <span class="add">+{{ call.added || 0 }}</span>
        <span class="del">-{{ call.removed || 0 }}</span>
      </span>
      <span v-if="state === 'running'" class="tc-pct">{{ Math.round(pct) }}%</span>

      <div class="tc-actions">
        <t-tooltip :content="call.reversible ? '撤销（待接入）' : '该操作不可撤销'">
          <t-button
            variant="text"
            size="small"
            :disabled="!call.reversible || state === 'running'"
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

    <!-- 执行中进度条 -->
    <div v-if="state === 'running'" class="tc-progress" :data-testid="`chat-toolcall-progress-${call.id}`" :data-progress="Math.round(pct)">
      <div class="tc-progress-fill" :style="{ width: pct + '%' }" />
    </div>

    <!-- 展开内容 -->
    <div v-show="expanded" class="tc-body" :data-testid="`chat-toolcall-body-${call.id}`">
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
      <div v-if="call.command" class="tc-cmd" :data-testid="`chat-toolcall-cmd-${call.id}`">
        <div class="cmd-line"><span class="cmd-prompt">$</span> {{ call.command }}</div>
        <div v-if="state === 'running'" class="cmd-output running">执行中<span class="dots"><i>.</i><i>.</i><i>.</i></span></div>
        <div v-else-if="call.output" class="cmd-output">{{ call.output }}</div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'

const props = defineProps({
  call: { type: Object, required: true }
})

const expanded = ref(true)
// 运行态：初始取自 call.status；running 时本地推进
const state = ref(props.call.status || 'success')
const pct = ref(props.call.status === 'running' ? 6 : 100)
let timer = null

const runningLabel = computed(() => props.call.runningText || props.call.title || props.call.name || '执行中')

// 已完成的文件数（按进度比例推进，制造逐个写入的观感）
const doneFileCount = computed(() => {
  const total = (props.call.files || []).length
  if (!total) return 0
  if (state.value !== 'running') return total
  return Math.min(total, Math.floor((pct.value / 100) * total))
})

const hasDiff = computed(() =>
  typeof props.call.added === 'number' || typeof props.call.removed === 'number'
)

const headIcon = computed(() => {
  switch (state.value) {
    case 'success': return 'check-circle'
    case 'error': return 'error-circle'
    default: return 'tools'
  }
})

function tick() {
  pct.value = Math.min(100, pct.value + 6 + Math.random() * 12)
  if (pct.value >= 100) {
    pct.value = 100
    stop()
    // 切换到最终态（真实接入时由后端 SSE 推送，无需 _finalStatus）
    state.value = props.call._finalStatus || 'success'
  }
}
function start() {
  stop()
  timer = setInterval(tick, 480)
}
function stop() {
  if (timer) { clearInterval(timer); timer = null }
}

function toggle() {
  expanded.value = !expanded.value
}

function fileStatusText(s) {
  return { modified: '已修改', added: '新增', deleted: '删除', renamed: '重命名' }[s] || s || ''
}

function onUndo() {
  MessagePlugin.info('撤销功能待后端接入')
}

onMounted(() => {
  if (state.value === 'running') start()
})
onBeforeUnmount(stop)
</script>

<style scoped>
.tool-call {
  border: 1px solid #e7e7e7;
  border-radius: 10px;
  background: #fbfbfc;
  overflow: hidden;
  margin-top: 8px;
}
.tc-success { border-color: #e3e8ef; }
.tc-error { border-color: #f3c9c9; background: #fff8f8; }
.tc-running { border-color: #c5d8f7; background: #f5f9ff; }

.tc-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 9px 12px;
  cursor: pointer;
  font-size: 13px;
}
.tc-head:hover { background: rgba(0, 0, 0, 0.02); }
.tc-icon { display: flex; align-items: center; font-size: 15px; }
.icon-success { color: #00a870; }
.icon-error { color: #d63c3c; }

.tc-spinner, .file-spinner {
  width: 13px;
  height: 13px;
  border: 2px solid rgba(0, 82, 217, 0.25);
  border-top-color: #0052d9;
  border-radius: 50%;
  display: inline-block;
  animation: spin 0.7s linear infinite;
}
.file-spinner { width: 12px; height: 12px; flex-shrink: 0; }

.tc-title { font-weight: 600; color: #1d1d1f; }
.tc-running-text {
  color: #0052d9;
  background: linear-gradient(90deg, #0052d9 25%, #7aa7ff 50%, #0052d9 75%);
  background-size: 200% 100%;
  -webkit-background-clip: text;
  background-clip: text;
  -webkit-text-fill-color: transparent;
  animation: flow 1.6s linear infinite;
}
.tc-summary { color: #666; }
.tc-pct { color: #0052d9; font-weight: 600; font-variant-numeric: tabular-nums; }
.tc-diff { font-size: 12px; font-variant-numeric: tabular-nums; }
.tc-diff .add, .file-diff .add { color: #00a870; margin-right: 6px; }
.tc-diff .del, .file-diff .del { color: #d63c3c; }

.tc-actions {
  margin-left: auto;
  display: flex;
  align-items: center;
  gap: 4px;
}
.tc-undo { color: #888; }
.tc-chevron { color: #999; cursor: pointer; }

.tc-progress {
  height: 3px;
  background: #e3ecfb;
  overflow: hidden;
}
.tc-progress-fill {
  height: 100%;
  background: linear-gradient(90deg, #2f7bff, #0052d9);
  transition: width 0.4s ease;
  position: relative;
}
.tc-progress-fill::after {
  content: '';
  position: absolute;
  inset: 0;
  background: linear-gradient(90deg, transparent, rgba(255, 255, 255, 0.6), transparent);
  animation: shimmer 1s infinite;
}

.tc-body {
  border-top: 1px solid #f0f0f0;
  padding: 6px 8px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.tc-file {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 8px;
  border-radius: 6px;
  font-size: 13px;
}
.tc-file:hover { background: rgba(0, 0, 0, 0.03); }
.tc-file.file-doing { opacity: 0.85; }
.file-icon { color: #8a94a6; flex-shrink: 0; }
.file-icon.pending { color: #c0c6d0; }
.file-path {
  flex: 1;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  color: #344054;
}
.file-status { font-size: 11px; padding: 0 6px; border-radius: 8px; background: #eef0f3; color: #667085; }
.fs-added { background: rgba(0, 168, 112, 0.12); color: #00a870; }
.fs-deleted { background: rgba(214, 60, 60, 0.12); color: #d63c3c; }
.fs-doing { background: rgba(0, 82, 217, 0.12); color: #0052d9; }
.file-diff { font-size: 12px; font-variant-numeric: tabular-nums; flex-shrink: 0; }
.file-jump { color: #b0b6c0; cursor: pointer; flex-shrink: 0; }
.file-jump:hover { color: #0052d9; }

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
.cmd-output { color: #9aa0a6; margin-top: 4px; white-space: pre-wrap; }
.cmd-output.running { color: #7aa7ff; }
.dots i { animation: blink 1.2s infinite; }
.dots i:nth-child(2) { animation-delay: 0.2s; }
.dots i:nth-child(3) { animation-delay: 0.4s; }

@keyframes spin { to { transform: rotate(360deg); } }
@keyframes flow { to { background-position: -200% 0; } }
@keyframes shimmer { 0% { transform: translateX(-100%); } 100% { transform: translateX(100%); } }
@keyframes blink { 0%, 100% { opacity: 0.2; } 50% { opacity: 1; } }
</style>
