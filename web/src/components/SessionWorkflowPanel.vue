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
      <t-tag
        v-if="workflow && workflow.goalMode"
        theme="warning"
        variant="light"
        size="small"
        class="wf-goal-badge"
        data-testid="chat-workflow-goal"
      >
        🎯 目标模式<span v-if="goalLabel"> · {{ goalLabel }}</span>
      </t-tag>
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

    <!-- 进度条：节点已拆出时展示完成比例-->
    <div v-if="nodes.length" class="wf-progress-bar" data-testid="chat-workflow-progressbar">
      <div class="wf-progress-fill" :style="{ width: `${progressPercent}%` }" />
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
          <!-- 节点详情（结果/错误）：默认折叠为单行摘要，展开后按 markdown 渲染 -->
          <div
            v-if="n.detail.text"
            class="todo-detail"
            :class="n.detail.isError ? 'detail-error' : 'detail-ok'"
            :data-testid="n.detail.isError ? `chat-workflow-node-error-${n.id}` : `chat-workflow-node-result-${n.id}`"
          >
            <div
              class="detail-head"
              :class="{ foldable: n.detail.foldable }"
              :role="n.detail.foldable ? 'button' : null"
              :tabindex="n.detail.foldable ? 0 : null"
              :aria-expanded="n.detail.foldable ? isDetailOpen(n.id) : null"
              @click.stop="n.detail.foldable && toggleDetail(n.id)"
              @keydown.enter.stop.prevent="n.detail.foldable && toggleDetail(n.id)"
              @keydown.space.stop.prevent="n.detail.foldable && toggleDetail(n.id)"
            >
              <span class="detail-mark">{{ n.detail.isError ? '✗' : '✓' }}</span>
              <span class="detail-summary" :class="{ open: isDetailOpen(n.id) }">{{ n.detail.summary }}</span>
              <template v-if="n.detail.foldable">
                <span class="detail-size">{{ n.detail.sizeLabel }}</span>
                <t-icon
                  class="detail-caret"
                  :name="isDetailOpen(n.id) ? 'chevron-up' : 'chevron-down'"
                  :data-testid="`chat-workflow-detail-toggle-${n.id}`"
                />
              </template>
            </div>

            <div v-if="n.detail.foldable && isDetailOpen(n.id)" class="detail-wrap">
              <div
                class="detail-body markdown-body"
                :data-testid="`chat-workflow-detail-body-${n.id}`"
                v-html="renderMarkdown(n.detail.text)"
              />
              <div class="detail-actions">
                <t-button variant="text" size="small" @click.stop="copyDetail(n.detail.text)">
                  <template #icon><t-icon name="file-copy" /></template>
                  复制全文
                </t-button>
                <t-button variant="text" size="small" @click.stop="toggleDetail(n.id)">
                  <template #icon><t-icon name="chevron-up" /></template>
                  收起
                </t-button>
              </div>
            </div>
          </div>
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
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { workflowApi } from '@/api/services'
import { useBotStore } from '@/stores/bot'

const props = defineProps({
  // 当前会话 id
  sessionId: { type: String, default: '' },
  // 该会话绑定的工作流 id（由 bot 在此 session 中通过 task 工具创建）
  workflowId: { type: String, default: '' }
})

const workflow = ref(null)
const rawNodes = ref([])
const expanded = ref(true)
const retrying = ref('')
let pollTimer = null

// ---- 节点详情（结果/错误）折叠与 markdown 渲染 ----
// 节点 result 往往是模型产出的长篇 markdown 审查报告（数千字），
// 直接内联成纯文本会把卡片撑爆且完全不可读。这里默认折叠成单行摘要，
// 展开后才按 markdown 渲染并限高滚动。
marked.setOptions({ breaks: true, gfm: true })

// markdown 渲染缓存：面板每 1.5s 轮询整体替换节点列表，若不缓存则会对
// 数千字的报告反复做 parse + sanitize，展开态下明显卡顿。
const _mdCache = new Map()
function renderMarkdown(text) {
  if (!text) return ''
  const hit = _mdCache.get(text)
  if (hit !== undefined) return hit
  const html = DOMPurify.sanitize(marked.parse(text))
  // 只缓存当前可见节点量级的内容，避免长轮询下无限增长
  if (_mdCache.size > 40) _mdCache.clear()
  _mdCache.set(text, html)
  return html
}

// 超过该长度即视为「长文」，需要折叠
const DETAIL_FOLD_THRESHOLD = 120
// 摘要最大字数
const SUMMARY_MAX = 90

// 展开中的节点详情 id 集合
const openDetails = ref(new Set())
const isDetailOpen = (id) => openDetails.value.has(id)
function toggleDetail(id) {
  const next = new Set(openDetails.value)
  next.has(id) ? next.delete(id) : next.add(id)
  openDetails.value = next // 替换引用以触发响应式更新（Set 原地增删不触发）
}

/**
 * 从 markdown 长文里抽一行可读摘要：
 * 跳过标题标记/列表符号/代码块围栏，取第一句有内容的话。
 */
function summarize(text) {
  const firstMeaningful = String(text)
    .split('\n')
    .map(l => l.trim())
    .find(l => l && !/^```/.test(l) && !/^[-*_]{3,}$/.test(l))
    || String(text).trim()
  const clean = firstMeaningful
    .replace(/^#{1,6}\s*/, '')          // 标题标记
    .replace(/^[-*+]\s+/, '')            // 列表符号
    .replace(/^>\s*/, '')                // 引用符号
    .replace(/`{1,3}/g, '')              // 行内代码反引号
    .replace(/\*\*|__/g, '')             // 加粗标记
    .replace(/\s+/g, ' ')
    .trim()
  return clean.length > SUMMARY_MAX ? `${clean.slice(0, SUMMARY_MAX)}…` : clean
}

/** 统一节点详情：error 优先于 result（与原逻辑一致） */
function buildDetail(n) {
  const isError = !!n.error
  const text = isError ? n.error : (n.result || '')
  const len = text.length
  const foldable = len > DETAIL_FOLD_THRESHOLD
  return {
    isError,
    text,
    foldable,
    summary: foldable ? summarize(text) : text,
    sizeLabel: len >= 1000 ? `${(len / 1000).toFixed(1)}k 字` : `${len} 字`
  }
}

// 给每个节点附加预计算的 detail，避免模板里多次调用导致重复 summarize
const nodes = computed(() => rawNodes.value.map(n => ({ ...n, detail: buildDetail(n) })))

async function copyDetail(text) {
  try {
    await navigator.clipboard.writeText(text)
    MessagePlugin.success('已复制到剪贴板')
  } catch (e) {
    MessagePlugin.error('复制失败，请手动选择文本复制')
  }
}

// 接收 store 中 SSE 推送的最新工作流快照（task_status 工具结果）
const botStore = useBotStore()

const doneCount = computed(() => nodes.value.filter(n => n.status === 'completed').length)

// 进度百分比（供顶部细进度条使用）
const progressPercent = computed(() => {
  if (!nodes.value.length) return 0
  return Math.round((doneCount.value / nodes.value.length) * 100)
})

// 进度计数文案：分析阶段尚未生成子任务时显示分析进度（或"分析中…"）而非"0/0"，避免误判卡死
// 注意：只有 status 真的是 analyzing 才显示分析文案。曾经用 isLive（含 running）判断，
// 导致 running 且节点已拆出但列表尚未拉到时，仍渲染残留的 analyzeMessage → 假「分析中」。
const progressLabel = computed(() => {
  if (nodes.value.length) return `${doneCount.value}/${nodes.value.length}`
  if (workflow.value?.status === 'analyzing') return workflow.value?.analyzeMessage || '分析中…'
  return '0/0'
})

// 进行中（需轮询刷新）
const isLive = computed(() => {
  const s = workflow.value?.status
  return s === 'running' || s === 'analyzing' || s === 'interrupted'
})

// 目标模式闭环进度文案：第 N/M 轮（M=0 时回退到引擎默认 5）
const goalLabel = computed(() => {
  const wf = workflow.value
  if (!wf || !wf.goalMode) return ''
  if (!wf.goalIteration || wf.goalIteration <= 0) return '待闭环'
  const max = wf.goalMaxIterations && wf.goalMaxIterations > 0 ? wf.goalMaxIterations : 5
  return `第 ${wf.goalIteration}/${max} 轮`
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
  _lastSseTs = 0 // 重置 SSE 时间戳，防止上一轮的残留脏数据影响本轮
  workflow.value = null
  rawNodes.value = []
  openDetails.value = new Set() // 切换工作流时清空详情展开态
  _mdCache.clear()
  if (!props.workflowId) return
  try {
    const res = await fetchState()
    if (!res) return
    workflow.value = res.status
    rawNodes.value = res.flat
    // 只要不是终态就必须轮询。
    // 曾用 isLive（running/analyzing/interrupted 白名单）判断，于是面板挂载时若工作流
    // 恰好是 pending 等白名单外的瞬态，轮询永不启动 → UI 永久停在首帧快照，
    // 叠加 Pinia 里分析阶段的 SSE 残留，表现为「节点已拆出但 UI 仍在分析中」。
    if (!isTerminal.value) startLive()
  } catch (e) {
    MessagePlugin.error(`加载工作流失败：${e.message || e}`)
  }
}

// ---- SSE 实时同步：task_status 工具结果推送的最新状态直接合并到面板 ----
// 解决头部卡片（轮询）与下方工具调用结果（SSE 推流）不同步的问题
let _lastSseTs = 0 // 最近一次 SSE 快照的时间戳，用于丢弃乱序到达的旧快照

// 工作流状态的阶段序：数值只表示「推进程度」，用于拒绝让状态倒退的快照。
// task_status 的返回是 bot 调用那一刻的时点快照，可能比本地轮询结果更旧；
// 若不设防，一个分析阶段的迟到快照会把已经 running 的 UI 打回「分析中」。
const WF_PHASE = { analyzing: 1, interrupted: 2, running: 3, completed: 4, failed: 4, terminated: 4 }
const wfPhase = (s) => WF_PHASE[s] || 0

watch(() => botStore.activeWorkflowStatus, (snap) => {
  if (!snap || !props.workflowId) return
  // 只合并当前工作流的快照（防止多会话串扰）
  if (snap.ID !== props.workflowId && snap.id !== props.workflowId) return
  // 时间戳守卫：忽略比当前已知的 SSE 数据更旧的快照（防止 Pinia 残留脏数据覆盖新拉取的 API 数据）
  if (snap._ts && snap._ts <= _lastSseTs) return
  // 阶段守卫：只接受「不比当前更早」的状态，避免 UI 从 running 倒退回 analyzing。
  if (wfPhase(snap.status) < wfPhase(workflow.value?.status)) return
  _lastSseTs = snap._ts || Date.now()
  // 合并关键字段到本地状态，立即反映在 UI 上
  //
  // analyzeMessage 只在 analyzing 阶段有意义：分析阶段取到的文案会随快照一路带到
  // running 之后，故按快照自身的 status 归零，避免「已拆出节点却仍显示第 N/5 次尝试」。
  workflow.value = {
    ...(workflow.value || {}),
    status: snap.status,
    requirement: snap.requirement || workflow.value?.requirement,
    analyzeMessage: snap.status === 'analyzing' ? snap.analyzeMessage : '',
    goalMode: snap.goalMode,
    goalIteration: snap.goalIteration,
    goalMaxIterations: snap.goalMaxIterations,
    error: snap.error,
  }
  // 若快照显示尚未结束但轮询已停，重启轮询保证持续刷新节点列表
  if (!isTerminal.value && !pollTimer) startLive()
})

// 运行态轮询：每 1.5s 拉取最新节点状态
// 终态判定：只有真正结束的状态才停止轮询，避免瞬态非 live 导致轮询永久死亡
const isTerminal = computed(() => {
  const s = workflow.value?.status
  return s === 'completed' || s === 'failed' || s === 'terminated'
})

async function tick() {
  if (!props.workflowId) return
  try {
    const res = await fetchState()
    if (!res) return
    // 节点列表与状态一律以 API 为准：它是权威数据源，且 1.5s 轮询比 SSE 快照更新。
    //
    // 这里曾经做过「5s 内优先保留 SSE 数据」的处理，但那个前提是错的：
    // task_status 工具的返回是 bot 调用那一刻的**时点快照**，不是实时流。用它压制
    // 轮询结果，会让工作流早已 running（节点都跑完两个了）而头部仍显示分析阶段的
    // status='analyzing' + 「第 N/5 次尝试」——即后端在推进、UI 永久停在分析中。
    // SSE 的价值是让状态变化「更早」出现，而不是「更久」留存，所以它只在 watch 里
    // 即时合并一次，不参与与轮询的优先级竞争。
    rawNodes.value = res.flat
    workflow.value = res.status
    if (isTerminal.value) {
      stopLive()
      _lastSseTs = 0
      // 后端已在阻塞等待方超时/取消场景下注入续跑消息（traceID=sessionID），
      // 这里按会话 resume 接收 agent 续跑的流式回复。needsContinuation 由后端
      // 注入后通过 status 接口给出，前端消费一次即清除，不会重复触发。
      if (res.status && res.status.needsContinuation) {
        botStore.resumeContinuation(props.sessionId)
      }
    }
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
    // 乐观更新原始数据源（nodes 是派生 computed，不能直接写）
    rawNodes.value = rawNodes.value.map(x => x.id === node.id
      ? { ...x, retryCount: (x.retryCount || 0) + 1, error: '', status: 'running' }
      : x)
    // 错误已清空，收起该节点残留的详情展开态
    if (openDetails.value.has(node.id)) toggleDetail(node.id)
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
/* 面板作为内嵌卡片：宽度由外层容器（ChatWindow 的 .wf-inline）约束，
   自身不再设 max-width/auto margin，否则与外层重复约束。 */
.wf-panel {
  position: relative;
  overflow: hidden;
  border: 1px solid #e8eaed;
  border-radius: 12px;
  background: #fff;
  box-shadow: 0 1px 2px rgba(0, 0, 0, 0.03);
  transition: box-shadow 0.2s ease, border-color 0.2s ease;
}
.wf-panel:hover { box-shadow: 0 2px 8px rgba(0, 0, 0, 0.05); }

/* 顶部状态色条替代原来的左侧粗边框：更轻、且不挤压内容 */
.wf-panel::before {
  content: '';
  position: absolute;
  inset: 0 0 auto;
  height: 2px;
  background: #d9dbde;
}
.wf-panel.wf-running::before,
.wf-panel.wf-analyzing::before {
  background: linear-gradient(90deg, #0052d9, #4b8ef0, #0052d9);
  background-size: 200% 100%;
  animation: wf-flow 2.2s linear infinite;
}
.wf-panel.wf-completed::before { background: #00a870; }
.wf-panel.wf-failed::before { background: #d63c3c; }
.wf-panel.wf-terminated::before { background: #b8b8b8; }
@keyframes wf-flow {
  0% { background-position: 100% 0; }
  100% { background-position: -100% 0; }
}

.wf-head {
  display: flex;
  align-items: center;
  gap: 10px;
  cursor: pointer;
  padding: 12px 14px;
  border-radius: 12px 12px 0 0;
  transition: background 0.15s ease;
}
.wf-head:hover { background: #fafbfc; }
.wf-icon {
  font-size: 15px;
  width: 26px;
  height: 26px;
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: 7px;
  background: #f2f3f5;
}
.wf-panel.wf-running .wf-icon,
.wf-panel.wf-analyzing .wf-icon { background: rgba(0, 82, 217, 0.09); }
.wf-panel.wf-completed .wf-icon { background: rgba(0, 168, 112, 0.1); }
.wf-panel.wf-failed .wf-icon { background: rgba(214, 60, 60, 0.09); }
.wf-title {
  font-weight: 600;
  font-size: 13.5px;
  color: #1d1d1f;
  line-height: 1.5;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  flex: 1;
  min-width: 0;
}
.wf-progress-text {
  font-size: 12px;
  color: #86888c;
  font-variant-numeric: tabular-nums;
  flex-shrink: 0;
}
.wf-toggle { color: #9a9c9f; flex-shrink: 0; }

.wf-goal-badge { font-variant-numeric: tabular-nums; flex-shrink: 0; }

/* 进度条：节点已拆出时展示完成比例，长任务里比"3/10" 更直观 */
.wf-progress-bar {
  height: 2px;
  background: #f0f1f3;
  overflow: hidden;
}
.wf-progress-fill {
  height: 100%;
  border-radius: 0 2px 2px 0;
  background: #0052d9;
  transition: width 0.35s ease;
}
.wf-panel.wf-completed .wf-progress-fill { background: #00a870; }
.wf-panel.wf-failed .wf-progress-fill { background: #d63c3c; }

/* 一维 TODO 清单 */
.wf-todo {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 10px 14px 12px;
  background: #fcfcfd;
  border-top: 1px solid #f2f3f5;
}
.todo-item {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 8px 10px;
  border-radius: 8px;
  background: #fff;
  border: 1px solid #eef0f2;
  transition: border-color 0.15s ease, background 0.15s ease;
}
.todo-item.todo-failed { border-color: #f3cccc; background: #fffafa; }
.todo-item.todo-running { border-color: #cbdcf8; background: #f8fbff; }
.todo-item.todo-reviewing { border-color: #f5dcb8; background: #fffcf7; }
.todo-item.todo-completed { border-color: #f0f2f4; background: #fff; }
.todo-item.todo-pending { opacity: 0.72; }

/* 左侧状态圆 */
.todo-check {
  width: 19px;
  height: 19px;
  border-radius: 50%;
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 11px;
  font-weight: 700;
  color: #fff;
  background: #dcdee0;
  /* todo-item 已改为顶部对齐（详情展开后可能很高），此处微调与首行文字视觉对齐 */
  margin-top: 1px;
}
.check-completed { background: #00a870; }
.check-failed { background: #d63c3c; }
.check-running {
  background: transparent;
  color: #0052d9;
  border: 2px solid #0052d9;
  box-shadow: 0 0 0 3px rgba(0, 82, 217, 0.09);
}
.check-reviewing { background: #e8820a; }
.check-terminated,
.check-skipped { background: #c4c6c9; }

.todo-main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 7px;
  /* 与顶部对齐的状态圆保持首行视觉基线一致 */
  padding-top: 1px;
}
.todo-name {
  font-size: 13px;
  font-weight: 500;
  color: #26282b;
  line-height: 1.5;
}
.todo-name.done { color: #6f7276; text-decoration: line-through; opacity: 0.8; }
.todo-status {
  font-size: 10.5px;
  font-weight: 500;
  line-height: 17px;
  padding: 0 7px;
  border-radius: 9px;
  background: #f0f1f3;
  color: #6f7276;
  flex-shrink: 0;
}
.st-running { background: rgba(0, 82, 217, 0.1); color: #0052d9; }
.st-completed { background: rgba(0, 168, 112, 0.1); color: #00926a; }
.st-failed { background: rgba(214, 60, 60, 0.1); color: #d63c3c; }
.st-reviewing { background: rgba(232, 130, 10, 0.12); color: #c2700a; }
.st-terminated,
.st-skipped { background: #f0f1f3; color: #9a9c9f; }
.todo-retry-count {
  font-size: 10.5px;
  color: #b06a00;
  background: rgba(224, 158, 0, 0.1);
  padding: 0 6px;
  border-radius: 9px;
  line-height: 17px;
  flex-shrink: 0;
}

/* ---- 节点详情：折叠头 + 展开体 ---- */
.todo-detail {
  flex-basis: 100%;
  min-width: 0;
  margin-top: 2px;
}
.detail-head {
  display: flex;
  align-items: baseline;
  gap: 6px;
  font-size: 12px;
  line-height: 1.6;
  border-radius: 6px;
  padding: 2px 4px;
  margin-left: -4px;
}
.detail-head.foldable { cursor: pointer; }
.detail-head.foldable:hover { background: rgba(0, 0, 0, 0.035); }
.detail-head.foldable:focus-visible {
  outline: 2px solid #0052d9;
  outline-offset: 1px;
}
.detail-mark { flex-shrink: 0; font-weight: 700; }
.detail-summary {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
/* 展开时摘要淡化，视觉重心交给正文 */
.detail-summary.open { opacity: 0.6; }
.detail-size {
  flex-shrink: 0;
  font-size: 11px;
  color: #999;
  font-variant-numeric: tabular-nums;
}
.detail-caret { flex-shrink: 0; color: #999; font-size: 14px; }

.detail-error { color: #d63c3c; }
.detail-ok { color: #2c8c6f; }

.detail-wrap {
  margin-top: 6px;
  border: 1px solid #ebedf0;
  border-radius: 8px;
  background: #fff;
  overflow: hidden;
}
.detail-body {
  max-height: 340px;
  overflow-y: auto;
  padding: 10px 12px;
  font-size: 12.5px;
  line-height: 1.75;
  color: #37383a;
  overflow-wrap: break-word;
}
.detail-actions {
  display: flex;
  justify-content: flex-end;
  gap: 2px;
  padding: 2px 6px;
  border-top: 1px solid #f2f3f5;
  background: #fcfcfd;
}

/* markdown 渲染内容样式（v-html 需 :deep 穿透 scoped） */
.markdown-body :deep(h1),
.markdown-body :deep(h2),
.markdown-body :deep(h3),
.markdown-body :deep(h4) {
  margin: 12px 0 6px;
  font-weight: 600;
  line-height: 1.4;
  color: #1f2225;
}
.markdown-body :deep(h1) { font-size: 15px; }
.markdown-body :deep(h2) {
  font-size: 14px;
  padding-bottom: 4px;
  border-bottom: 1px solid #f0f1f3;
}
.markdown-body :deep(h3) { font-size: 13px; }
.markdown-body :deep(h4) { font-size: 12.5px; }
.markdown-body :deep(> :first-child) { margin-top: 0; }
.markdown-body :deep(p) { margin: 6px 0; }
.markdown-body :deep(ul),
.markdown-body :deep(ol) { margin: 6px 0; padding-left: 20px; }
.markdown-body :deep(li) { margin: 2px 0; }
.markdown-body :deep(li > p) { margin: 0; }
.markdown-body :deep(strong) { font-weight: 600; color: #1f2225; }
.markdown-body :deep(code) {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 11.5px;
  padding: 1px 5px;
  border-radius: 4px;
  background: #f2f3f5;
  color: #c4372a;
  overflow-wrap: break-word;
}
.markdown-body :deep(pre) {
  margin: 8px 0;
  padding: 10px 12px;
  border-radius: 6px;
  background: #f7f8fa;
  border: 1px solid #eff0f2;
  overflow-x: auto;
}
.markdown-body :deep(pre code) {
  padding: 0;
  background: none;
  color: #37383a;
  font-size: 11.5px;
  line-height: 1.65;
  white-space: pre;
}
.markdown-body :deep(blockquote) {
  margin: 8px 0;
  padding: 2px 0 2px 10px;
  border-left: 3px solid #e3e5e8;
  color: #6b6d70;
}
.markdown-body :deep(table) {
  width: 100%;
  margin: 8px 0;
  border-collapse: collapse;
  font-size: 11.5px;
}
.markdown-body :deep(th),
.markdown-body :deep(td) {
  border: 1px solid #ebedf0;
  padding: 4px 8px;
  text-align: left;
}
.markdown-body :deep(th) { background: #f7f8fa; font-weight: 600; }
.markdown-body :deep(hr) {
  margin: 12px 0;
  border: none;
  border-top: 1px solid #eff0f2;
}
.markdown-body :deep(a) { color: #0052d9; }

.todo-retry-btn { flex-shrink: 0; margin-top: -2px; }
.todo-empty {
  font-size: 12px;
  color: #9a9c9f;
  padding: 10px;
  text-align: center;
  border-radius: 8px;
  background: #fff;
  border: 1px dashed #e8eaed;
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
