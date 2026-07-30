<template>
  <div
    v-if="workflow"
    class="wf-panel"
    :class="`wf-${workflow.status}`"
    data-testid="chat-workflow-panel"
    role="region"
    aria-label="当前工作流任务清单"
  >
    <!-- 头部：需求概览 + 折叠 -->
    <div class="wf-head" data-testid="chat-workflow-head" @click="expanded = !expanded">
      <span class="wf-icon">🧩</span>
      <span class="wf-title" data-testid="chat-workflow-title">{{ workflow.requirement }}</span>
      <t-tag
        :theme="statusTheme(workflow.status)"
        variant="light"
        size="small"
        data-testid="chat-workflow-status"
        :data-status="workflow.status"
      >
        <span v-if="isLive" class="live-dot" />{{ statusText(workflow.status) }}
      </t-tag>
      <span class="wf-progress-text" data-testid="chat-workflow-progress">{{ progressLabel }}</span>
      <t-button
        variant="text"
        size="small"
        shape="square"
        class="wf-toggle"
        :data-testid="expanded ? 'chat-workflow-collapse' : 'chat-workflow-expand'"
        :aria-label="expanded ? '收起任务清单' : '展开任务清单'"
        @click.stop="expanded = !expanded"
      >
        <t-icon :name="expanded ? 'chevron-up' : 'chevron-down'" />
      </t-button>
    </div>

    <!-- 一维 TODO 清单 -->
    <div v-show="expanded" class="wf-todo" data-testid="chat-workflow-nodes">
      <div
        v-for="(n, i) in nodes"
        :key="n.id"
        class="todo-item"
        :class="`todo-${n.status}`"
        :data-testid="`chat-workflow-node-${n.id}`"
        :data-node-status="n.status"
      >
        <span class="todo-check" :class="`check-${n.status}`">
          <span v-if="n.status === 'running'" class="live-dot" />
          <span v-else-if="n.status === 'completed'">✓</span>
          <span v-else-if="n.status === 'failed'">✗</span>
          <span v-else-if="n.status === 'reviewing'">◐</span>
          <span v-else-if="n.status === 'terminated' || n.status === 'skipped'">–</span>
          <span v-else>{{ i + 1 }}</span>
        </span>

        <div class="todo-main">
          <span class="todo-name" :class="{ done: n.status === 'completed' }">{{ n.name }}</span>
          <span class="todo-status" :class="`st-${n.status}`">{{ statusText(n.status) }}</span>
          <span v-if="n.retryCount" class="todo-retry-count">已重试 {{ n.retryCount }} 次</span>
          <div v-if="n.error" class="todo-error" :data-testid="`chat-workflow-node-error-${n.id}`">✗ {{ n.error }}</div>
          <div v-else-if="n.result" class="todo-result">✓ {{ n.result }}</div>
        </div>

        <t-button
          v-if="n.status === 'failed'"
          theme="danger"
          variant="outline"
          size="small"
          :loading="retrying === n.id"
          class="todo-retry-btn"
          :data-testid="`chat-workflow-retry-${n.id}`"
          :aria-label="`重试任务：${n.name}`"
          @click="retry(n)"
        >
          <template #icon><t-icon name="refresh" /></template>
          重试
        </t-button>
      </div>

      <div v-if="!nodes.length" class="todo-empty" data-testid="chat-workflow-empty">
        <template v-if="isLive">
          <span v-if="workflow && workflow.analyzeMessage">{{ workflow.analyzeMessage }}</span>
          <span v-else>正在分析需求并拆解任务…</span>
        </template>
        <template v-else-if="workflow && workflow.status === 'failed'">
          <div class="todo-fail-title">任务执行失败，未生成子任务清单</div>
          <div v-if="workflow.error" class="todo-fail-reason" data-testid="chat-workflow-error">
            原因：{{ workflow.error }}
          </div>
        </template>
        <template v-else>暂无子任务</template>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, watch, onBeforeUnmount } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { workflowApi } from '@/api/services'

const props = defineProps({
  // 当前会话 id
  sessionId: { type: String, default: '' },
  // 该会话绑定的工作流 id（由 bot 在此 session 中通过 task 工具创建）
  workflowId: { type: String, default: '' }
})

const workflow = ref(null)
const nodes = ref([])
const expanded = ref(true)
const retrying = ref('')
let pollTimer = null

const doneCount = computed(() => nodes.value.filter(n => n.status === 'completed').length)

// 进度计数文案：分析阶段尚未生成子任务时显示分析进度（或"分析中…"）而非"0/0"，避免误判卡死
const progressLabel = computed(() => {
  if (!nodes.value.length) return isLive.value ? (workflow.value?.analyzeMessage || '分析中…') : '0/0'
  return `${doneCount.value}/${nodes.value.length}`
})

// 进行中（需轮询刷新）
const isLive = computed(() => {
  const s = workflow.value?.status
  return s === 'running' || s === 'analyzing' || s === 'interrupted'
})

function statusText(s) {
  return {
    analyzing: '分析中', running: '运行中', completed: '已完成', failed: '失败',
    pending: '待执行', terminated: '已终止', reviewing: '审查中',
    interrupted: '已中断', skipped: '已跳过'
  }[s] || s
}

function statusTheme(status) {
  switch (status) {
    case 'running':
    case 'analyzing':
    case 'interrupted': return 'primary'
    case 'completed': return 'success'
    case 'failed': return 'danger'
    default: return 'default'
  }
}

async function fetchState() {
  if (!props.workflowId) return null
  const [status, nodesRes] = await Promise.all([
    workflowApi.status(props.workflowId),
    workflowApi.nodes(props.workflowId, 'flat')
  ])
  return { status, flat: nodesRes.flat || [] }
}

async function load() {
  stopLive()
  workflow.value = null
  nodes.value = []
  if (!props.workflowId) return
  try {
    const res = await fetchState()
    if (!res) return
    workflow.value = res.status
    nodes.value = res.flat
    if (isLive.value) startLive()
  } catch (e) {
    MessagePlugin.error(`加载工作流失败：${e.message || e}`)
  }
}

// 运行态轮询：每 1.5s 拉取最新节点状态，直到进入终态
async function tick() {
  if (!props.workflowId) return
  try {
    const res = await fetchState()
    if (!res) return
    workflow.value = res.status
    nodes.value = res.flat
    if (!isLive.value) stopLive()
  } catch (e) {
    // 瞬态错误忽略，下次轮询重试
  }
}

function startLive() {
  stopLive()
  pollTimer = setInterval(tick, 1500)
}
function stopLive() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null }
}

async function retry(node) {
  retrying.value = node.id
  try {
    await workflowApi.retryNode(props.workflowId, node.id)
    const n = nodes.value.find(x => x.id === node.id)
    if (n) { n.retryCount = (n.retryCount || 0) + 1; n.error = ''; n.status = 'running' }
    nodes.value = [...nodes.value]
    startLive()
    MessagePlugin.success(`已重试任务「${node.name}」`)
  } catch (e) {
    MessagePlugin.error(`重试失败：${e.message || e}`)
  } finally {
    retrying.value = ''
  }
}

watch(() => props.workflowId, load, { immediate: true })

onBeforeUnmount(stopLive)
</script>

<style scoped>
.wf-panel {
  max-width: 820px;
  margin: 0 auto 4px;
  border: 1px solid #e7e7e7;
  border-left: 3px solid #c9c9c9;
  border-radius: 12px;
  padding: 12px 16px;
  background: #fafbfc;
}
.wf-panel.wf-running,
.wf-panel.wf-analyzing { border-left-color: #0052d9; background: rgba(0, 82, 217, 0.03); }
.wf-panel.wf-completed { border-left-color: #00a870; }
.wf-panel.wf-failed { border-left-color: #d63c3c; background: rgba(214, 60, 60, 0.03); }

.wf-head {
  display: flex;
  align-items: center;
  gap: 10px;
  cursor: pointer;
}
.wf-icon { font-size: 16px; }
.wf-title {
  font-weight: 600;
  font-size: 14px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  flex: 1;
  min-width: 0;
}
.wf-progress-text {
  font-size: 12px;
  color: #888;
  font-variant-numeric: tabular-nums;
}
.wf-toggle { color: #999; }

/* 一维 TODO 清单 */
.wf-todo {
  margin-top: 10px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.todo-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 7px 10px;
  border-radius: 8px;
  background: #fff;
  border: 1px solid #f0f0f0;
}
.todo-item.todo-failed { border-color: #f5c6c6; background: #fff8f8; }
.todo-item.todo-running { border-color: #c5d8f7; background: #f5f9ff; }
.todo-item.todo-completed { background: #fbfdfc; }

/* 左侧状态圆 */
.todo-check {
  width: 20px;
  height: 20px;
  border-radius: 50%;
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 12px;
  font-weight: 700;
  color: #fff;
  background: #d4d4d4;
}
.check-completed { background: #00a870; }
.check-failed { background: #d63c3c; }
.check-running { background: transparent; color: #0052d9; border: 2px solid #0052d9; }
.check-reviewing { background: #e8820a; }
.check-terminated,
.check-skipped { background: #b8b8b8; }

.todo-main { flex: 1; min-width: 0; display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
.todo-name { font-size: 13px; font-weight: 500; }
.todo-name.done { color: #2c8c6f; text-decoration: line-through; opacity: 0.75; }
.todo-status {
  font-size: 11px;
  padding: 0 7px;
  border-radius: 9px;
  background: #ececec;
  color: #666;
}
.st-running { background: rgba(0, 82, 217, 0.12); color: #0052d9; }
.st-completed { background: rgba(0, 168, 112, 0.12); color: #00a870; }
.st-failed { background: rgba(214, 60, 60, 0.12); color: #d63c3c; }
.st-reviewing { background: rgba(232, 130, 10, 0.14); color: #c2700a; }
.st-terminated,
.st-skipped { background: #eee; color: #999; }
.todo-retry-count { font-size: 11px; color: #b06a00; }
.todo-error { flex-basis: 100%; font-size: 12px; color: #d63c3c; }
.todo-result { flex-basis: 100%; font-size: 12px; color: #00a870; }

.todo-retry-btn { flex-shrink: 0; }
.todo-empty {
  font-size: 12px;
  color: #999;
  padding: 6px 10px;
  text-align: center;
}
.todo-fail-title { color: #d63c3c; font-weight: 500; }
.todo-fail-reason {
  margin-top: 4px;
  color: #b06a00;
  font-size: 12px;
  text-align: left;
  word-break: break-word;
}

.live-dot {
  display: inline-block;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: currentColor;
  vertical-align: middle;
  animation: pulse 1.1s infinite ease-in-out;
}
@keyframes pulse {
  0% { opacity: 1; transform: scale(1); }
  50% { opacity: 0.35; transform: scale(0.7); }
  100% { opacity: 1; transform: scale(1); }
}
</style>
