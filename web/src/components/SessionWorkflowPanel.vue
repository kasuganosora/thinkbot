<template>
  <div
    v-if="workflow"
    class="wf-panel"
    :class="`wf-${workflow.status}`"
    data-testid="chat-workflow-panel"
    role="region"
    aria-label="当前会话的工作流状态"
  >
    <!-- 头部：概览 + 折叠 -->
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
        <span v-if="workflow.status === 'running'" class="live-dot" />{{ statusText(workflow.status) }}
      </t-tag>
      <span class="wf-progress-text" data-testid="chat-workflow-progress">{{ doneCount }}/{{ nodes.length }} 节点</span>
      <t-tag
        v-if="runningCount > 1"
        theme="primary"
        variant="light"
        size="small"
        data-testid="chat-workflow-parallel"
        :data-parallel="runningCount"
      >并行 {{ runningCount }}</t-tag>
      <t-button
        variant="text"
        size="small"
        shape="square"
        class="wf-toggle"
        :data-testid="expanded ? 'chat-workflow-collapse' : 'chat-workflow-expand'"
        :aria-label="expanded ? '收起工作流节点' : '展开工作流节点'"
        @click.stop="expanded = !expanded"
      >
        <t-icon :name="expanded ? 'chevron-up' : 'chevron-down'" />
      </t-button>
    </div>

    <t-progress
      :percentage="percent"
      :status="progressStatus"
      size="small"
      :label="false"
      class="wf-bar"
    />

    <!-- 节点列表 -->
    <div v-show="expanded" class="wf-nodes" data-testid="chat-workflow-nodes">
      <div
        v-for="(n, i) in nodes"
        :key="n.id"
        class="wf-node"
        :class="`node-${n.status}`"
        :data-testid="`chat-workflow-node-${n.id}`"
        :data-node-status="n.status"
      >
        <span class="node-dot" :class="`dot-${n.status}`">
          <span v-if="n.status === 'running'" class="live-dot" />
        </span>
        <div class="node-main">
          <div class="node-line">
            <span class="node-name">{{ n.name }}</span>
            <span class="node-badge" :class="`badge-${n.status}`" :data-testid="`chat-workflow-node-status-${n.id}`">
              {{ statusText(n.status) }}
            </span>
            <span
              v-if="n.dependencies && n.dependencies.length"
              class="node-deps"
              :data-testid="`chat-workflow-node-deps-${n.id}`"
            >依赖 {{ depNames(n) }}</span>
            <span v-if="n.retryCount" class="node-retry-count" :data-testid="`chat-workflow-node-retrycount-${n.id}`">
              已重试 {{ n.retryCount }} 次
            </span>
          </div>
          <div v-if="n.error" class="node-error" :data-testid="`chat-workflow-node-error-${n.id}`">
            ✗ {{ n.error }}
          </div>
          <div v-else-if="n.result" class="node-result">✓ {{ n.result }}</div>
          <div
            v-else-if="n.status === 'running'"
            class="node-running-box"
            :data-testid="`chat-workflow-node-progress-${n.id}`"
            :data-progress="Math.round(n._pct || 0)"
          >
            <div class="running-line">
              <span class="running-spinner" />
              <span class="running-task">{{ n.task || '执行中' }}…</span>
              <span class="running-pct">{{ Math.round(n._pct || 0) }}%</span>
            </div>
            <div class="node-bar">
              <div class="node-bar-fill" :style="{ width: (n._pct || 0) + '%' }" />
            </div>
          </div>
        </div>
        <!-- 失败节点：重试按钮 -->
        <t-button
          v-if="n.status === 'failed'"
          theme="danger"
          variant="outline"
          size="small"
          :loading="retrying === n.id"
          class="node-retry-btn"
          :data-testid="`chat-workflow-retry-${n.id}`"
          :aria-label="`重试节点：${n.name}`"
          @click="retry(n)"
        >
          <template #icon><t-icon name="refresh" /></template>
          重试
        </t-button>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, watch, onBeforeUnmount } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { workflowApi } from '@/api/services'

const props = defineProps({
  // 当前会话 id：用于按 session 关联工作流
  sessionId: { type: String, default: '' },
  // 该会话绑定的工作流 id（由 bot 在此 session 中创建）
  workflowId: { type: String, default: '' }
})

const workflow = ref(null)
const nodes = ref([])
const expanded = ref(true)
const retrying = ref('')
let liveTimer = null

const doneCount = computed(() => nodes.value.filter(n => n.status === 'completed').length)

const percent = computed(() => {
  const list = nodes.value
  if (!list.length) return 0
  // 每个节点单独贡献 0~1：完成=1，运行中=各自 _pct，其余=0；再取平均
  const sum = list.reduce((acc, n) => {
    if (n.status === 'completed') return acc + 1
    if (n.status === 'running') return acc + Math.min(1, (n._pct || 0) / 100)
    return acc
  }, 0)
  return Math.round((sum / list.length) * 100)
})

// 当前并行运行的节点数（用于头部提示）
const runningCount = computed(() => nodes.value.filter(n => n.status === 'running').length)

const progressStatus = computed(() => {
  if (!workflow.value) return 'active'
  if (workflow.value.status === 'completed') return 'success'
  if (nodes.value.some(n => n.status === 'failed')) return 'error'
  if (workflow.value.status === 'running') return 'active'
  return 'warning'
})

function statusText(s) {
  return {
    running: '运行中', completed: '已完成', failed: '失败',
    pending: '待执行', terminated: '已终止', reviewing: '审查中'
  }[s] || s
}

function depNames(node) {
  return (node.dependencies || [])
    .map(d => nodes.value.find(x => x.id === d)?.name || d)
    .join('、')
}

function statusTheme(status) {
  switch (status) {
    case 'running': return 'primary'
    case 'completed': return 'success'
    case 'failed': return 'danger'
    default: return 'default'
  }
}

function syncWorkflowStatus() {
  if (!workflow.value) return
  if (nodes.value.some(n => n.status === 'failed')) workflow.value.status = 'failed'
  else if (nodes.value.every(n => n.status === 'completed')) workflow.value.status = 'completed'
  else if (nodes.value.some(n => n.status === 'running')) workflow.value.status = 'running'
}

// 判断某节点的依赖是否全部完成
function depsSatisfied(node, list) {
  const deps = node.dependencies || []
  if (!deps.length) return true
  return deps.every(d => {
    const dep = list.find(x => x.id === d)
    return dep && dep.status === 'completed'
  })
}

// 调度：把所有「依赖已满足」的 pending 节点一起置为 running（可能解锁多个 = 并行分支）
function schedule(list) {
  let started = false
  for (const n of list) {
    if (n.status === 'pending' && depsSatisfied(n, list)) {
      n.status = 'running'
      n._pct = 10
      n.startedAt = new Date().toISOString()
      started = true
    }
  }
  return started
}

// DAG 推进：同时推进所有 running 节点；任一节点完成后重新调度后继节点（支持并行）
function advance() {
  const list = nodes.value
  let mutated = false

  // 1) 同时推进全部 running 节点（并行执行）
  for (const n of list) {
    if (n.status !== 'running') continue
    n._pct = Math.min(100, (n._pct || 20) + 7 + Math.round(Math.random() * 12))
    if (n._pct >= 100) {
      if (n._failOnce) {
        // 首次跑到末尾触发失败，等待用户重试
        n.status = 'failed'
        n.error = n.error || '校验未通过：并行结果存在冲突'
        n._failOnce = false
      } else {
        n.status = 'completed'
        n.result = n.result || `${n.name}已完成`
        n.completedAt = new Date().toISOString()
      }
      mutated = true
    } else {
      mutated = true
    }
  }

  // 2) 有节点完成 → 解锁所有依赖已满足的 pending 节点（一次可启动多个）
  if (mutated) {
    schedule(list)
    nodes.value = [...list]
    syncWorkflowStatus()
  }

  // 3) 没有任何 running 节点时停止计时器，避免空转
  if (!list.some(n => n.status === 'running')) {
    stopLive()
  }
}

function startLive() {
  stopLive()
  liveTimer = setInterval(advance, 1100)
}
function stopLive() {
  if (liveTimer) { clearInterval(liveTimer); liveTimer = null }
}

async function retry(node) {
  retrying.value = node.id
  try {
    // 调用 API（mock）：对齐后端 POST /api/workflows/:id/nodes/:nodeId/retry
    await workflowApi.retryNode(props.workflowId, node.id)
    // 重试：失败节点回到 running，清错误、重试次数 +1，并标记不再二次失败
    node.retryCount = (node.retryCount || 0) + 1
    node.error = ''
    node.status = 'running'
    node._pct = 15
    node._failOnce = false
    node.startedAt = new Date().toISOString()
    nodes.value = [...nodes.value]
    syncWorkflowStatus()
    startLive()
    MessagePlugin.success(`已重试节点「${node.name}」`)
  } catch (e) {
    MessagePlugin.error(`重试失败：${e.message || e}`)
  } finally {
    retrying.value = ''
  }
}

async function load() {
  stopLive()
  workflow.value = null
  nodes.value = []
  if (!props.workflowId) return
  try {
    const [status, nodesRes] = await Promise.all([
      workflowApi.status(props.workflowId),
      workflowApi.nodes(props.workflowId, 'flat')
    ])
    workflow.value = status
    // 真实接入时直接用 nodesRes.flat（含 dependencies）做 DAG 调度。
    // mock 演示：构造一个带并行分支的 DAG —— n1 完成后并行解锁 n2a/n2b，
    // 两者汇聚到 n3（需 n2a+n2b 都完成），n3 失败用于演示重试。
    const base = [
      { id: 'n1', name: '需求分析', task: '拆解需求', status: 'completed', result: '已输出需求清单', error: '', dependencies: [], retryCount: 0, startedAt: new Date(Date.now() - 240000).toISOString(), completedAt: new Date(Date.now() - 180000).toISOString(), _pct: 100 },
      { id: 'n2a', name: '资料检索', task: '并行检索外部资料', status: 'running', result: '', error: '', dependencies: ['n1'], retryCount: 0, startedAt: new Date(Date.now() - 60000).toISOString(), _pct: 45 },
      { id: 'n2b', name: '数据建模', task: '并行构建数据模型', status: 'running', result: '', error: '', dependencies: ['n1'], retryCount: 0, startedAt: new Date(Date.now() - 50000).toISOString(), _pct: 30 },
      { id: 'n3', name: '结果汇总校验', task: '汇总并校验产出', status: 'pending', result: '', error: '', dependencies: ['n2a', 'n2b'], retryCount: 0, _failOnce: true }
    ]
    nodes.value = base
    syncWorkflowStatus()
    if (nodes.value.some(n => n.status === 'running')) startLive()
  } catch (e) {
    MessagePlugin.error(`加载工作流状态失败：${e.message || e}`)
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
.wf-panel.wf-running { border-left-color: #0052d9; background: rgba(0, 82, 217, 0.03); }
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
}
.wf-toggle { color: #999; }
.wf-bar { margin: 10px 0 2px; }

.wf-nodes {
  margin-top: 10px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.wf-node {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 8px 10px;
  border-radius: 8px;
  background: #fff;
  border: 1px solid #f0f0f0;
}
.wf-node.node-failed { border-color: #f5c6c6; background: #fff8f8; }
.wf-node.node-running { border-color: #c5d8f7; background: #f5f9ff; }

.node-dot {
  width: 10px;
  height: 10px;
  border-radius: 50%;
  margin-top: 5px;
  flex-shrink: 0;
  background: #c9c9c9;
  color: #0052d9;
  display: flex;
  align-items: center;
  justify-content: center;
}
.dot-completed { background: #00a870; }
.dot-failed { background: #d63c3c; }
.dot-running { background: transparent; }
.dot-pending { background: #d4d4d4; }

.node-main { flex: 1; min-width: 0; }
.node-line {
  display: flex;
  align-items: center;
  gap: 8px;
}
.node-name { font-size: 13px; font-weight: 500; }
.node-badge {
  font-size: 11px;
  padding: 0 7px;
  border-radius: 9px;
  background: #ececec;
  color: #666;
}
.badge-running { background: rgba(0, 82, 217, 0.12); color: #0052d9; }
.badge-completed { background: rgba(0, 168, 112, 0.12); color: #00a870; }
.badge-failed { background: rgba(214, 60, 60, 0.12); color: #d63c3c; }
.badge-pending { background: #ececec; color: #888; }

.node-deps { font-size: 11px; color: #999; }
.node-retry-count { font-size: 11px; color: #b06a00; }
.node-error { font-size: 12px; color: #d63c3c; margin-top: 3px; }
.node-result { font-size: 12px; color: #00a870; margin-top: 3px; }
.node-running-hint { font-size: 12px; color: #0052d9; margin-top: 3px; }

.node-running-box { margin-top: 5px; }
.running-line {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: #0052d9;
}
.running-task {
  flex: 1;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.running-pct { font-variant-numeric: tabular-nums; color: #0052d9; font-weight: 600; }
.running-spinner {
  width: 12px;
  height: 12px;
  flex-shrink: 0;
  border: 2px solid rgba(0, 82, 217, 0.25);
  border-top-color: #0052d9;
  border-radius: 50%;
  animation: spin 0.7s linear infinite;
}
.node-bar {
  margin-top: 5px;
  height: 4px;
  border-radius: 3px;
  background: #e9eef7;
  overflow: hidden;
}
.node-bar-fill {
  height: 100%;
  border-radius: 3px;
  background: linear-gradient(90deg, #2f7bff, #0052d9);
  transition: width 0.4s ease;
  position: relative;
}
.node-bar-fill::after {
  content: '';
  position: absolute;
  inset: 0;
  background: linear-gradient(90deg, transparent, rgba(255, 255, 255, 0.55), transparent);
  animation: shimmer 1.1s infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }
@keyframes shimmer {
  0% { transform: translateX(-100%); }
  100% { transform: translateX(100%); }
}

.node-retry-btn { flex-shrink: 0; align-self: center; }

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
