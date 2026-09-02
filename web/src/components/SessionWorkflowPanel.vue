<template>
  <div
    v-if="workflow"
    ref="rootRef"
    class="wf-panel"
    :class="`wf-${workflow.status}`"
    data-testid="chat-workflow-panel"
    role="region"
    aria-label="当前工作流任务清单"
  >
    <div
      class="wf-head"
      data-testid="chat-workflow-head"
      role="button"
      tabindex="0"
      :aria-expanded="expanded"
      @click="toggle"
      @pointerdown="onHeadPointerDown"
      @keydown.enter.prevent="toggle"
      @keydown.space.prevent="toggle"
    >
      <span class="wf-title" data-testid="chat-workflow-title">{{ workflow.requirement }}</span>
      <span
        class="wf-status"
        :class="`wf-st-${workflow.status || ''}`"
        data-testid="chat-workflow-status"
        :data-status="workflow.status"
      >
        <span v-if="isLive" class="live-dot" />{{ statusText(workflow.status) }}
      </span>
      <span class="wf-progress-text" data-testid="chat-workflow-progress">{{ progressLabel }}</span>
      <span
        v-if="workflow && workflow.goalMode"
        class="wf-goal"
        data-testid="chat-workflow-goal"
      >{{ goalLabel ? '目标 · ' + goalLabel : '目标模式' }}</span>
      <t-icon
        :name="expanded ? 'chevron-up' : 'chevron-down'"
        class="wf-chevron"
        :data-testid="expanded ? 'chat-workflow-collapse' : 'chat-workflow-expand'"
        aria-hidden="true"
      />
    </div>

    <!-- 进度条：节点已拆出时展示完成比例-->
    <div v-if="nodes.length" class="wf-progress-bar" data-testid="chat-workflow-progressbar">
      <div class="wf-progress-fill" :style="{ width: `${progressPercent}%` }" />
    </div>

    <!-- 一维 TODO 清单 -->
    <div class="wf-body-wrap" :class="{ 'is-open': expanded }">
      <div class="wf-body-inner">
    <div class="wf-todo" data-testid="chat-workflow-nodes">
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
          <!-- 结果类别徽标：区分「跑完了」与「做成了」。
               没有它，一个 completed 但 missing_tool 的节点看起来就是普通的 ✓。 -->
          <span
            v-if="n.badge"
            class="todo-outcome"
            :class="`outcome-${n.badge.theme}`"
            :title="n.badge.title"
            :data-testid="`chat-workflow-node-outcome-${n.id}`"
            :data-outcome="n.outcome"
          >{{ n.badge.text }}</span>
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
    </div>
  </div>
</template>

<script setup>
import { ref, computed, watch, onMounted, onBeforeUnmount } from 'vue'
import { animateSpring, prefersReducedMotion } from '@/utils/spring'
import { MessagePlugin } from 'tdesign-vue-next'
import { renderMarkdown, clearMarkdownCache } from '@/utils/markdown'
import { workflowApi } from '@/api/services'
import { useBotStore } from '@/stores/bot'

const props = defineProps({
  // 当前会话 id
  sessionId: { type: String, default: '' },
  // 该会话绑定的工作流 id（由 bot 在此 session 中通过 task 工具创建）
  workflowId: { type: String, default: '' }
})

const rootRef = ref(null)
const workflow = ref(null)
const rawNodes = ref([])
const expanded = ref(true)
const retrying = ref('')
let pollTimer = null

// ---- 节点详情（结果/错误）折叠与 markdown 渲染 ----
// 节点 result 往往是模型产出的长篇 markdown 审查报告（数千字），
// 直接内联成纯文本会把卡片撑爆且完全不可读。这里默认折叠成单行摘要，
// 展开后才按 markdown 渲染并限高滚动。
//
// 渲染与缓存统一走 utils/markdown（面板每 1.5s 轮询整体替换节点列表，
// 不缓存会对数千字的报告反复 parse + sanitize，展开态下明显卡顿）。

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

// 节点结果类别徽标：区分「跑完了」与「做成了」。
//
// 后端 NodeFlat 已带 outcome / outcomeReason / toolProfile（见 workflow README）。
// 不展示的话，一个 status=completed 但 outcome=missing_tool 的节点在用户看来
// 就是普通的 ✓——而它实际上什么都没做成。这正是后端引入 Outcome 要解决的问题。
//
// 只在非 ok 时返回徽标：正常节点不加视觉噪音。
function outcomeBadge(n) {
  const reason = n.outcomeReason || ''
  // 档位信息对「缺少工具」尤其有用：hover 即可看出被限制在哪个档位
  const profileHint = n.toolProfile ? `（当前档位：${n.toolProfile}）` : ''
  switch (n.outcome) {
    case 'noop':
      return {
        kind: 'degraded', text: '无变更', theme: 'default',
        title: reason || '范围内没有需要处理的变更'
      }
    case 'partial':
      return {
        kind: 'degraded', text: '部分完成', theme: 'warning',
        title: reason || '只完成了任务的一部分，产物可能不完整'
      }
    case 'missing_tool':
      return {
        kind: 'blocked', text: '缺少工具', theme: 'danger',
        title: `${reason || '完成任务所需的工具不可用'}${profileHint}`
      }
    case 'missing_data':
      return {
        kind: 'blocked', text: '上游数据缺失', theme: 'danger',
        title: reason || '缺少必需的输入数据，问题通常在上游节点'
      }
    default:
      return null // ok / 空值：不展示
  }
}

// 给每个节点附加预计算的 detail 与结果徽标，避免模板里多次调用导致重复计算
const nodes = computed(() => rawNodes.value.map(n => ({
  ...n,
  detail: buildDetail(n),
  badge: outcomeBadge(n)
})))

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

// 降级（partial / noop）与受阻（missing_tool / missing_data）的节点数。
// 这两类节点在 status 上仍是 completed / failed，计数不加进来用户就看不出质量差异。
const degradedCount = computed(() => nodes.value.filter(n => n.badge?.kind === 'degraded').length)
const blockedCount = computed(() => nodes.value.filter(n => n.badge?.kind === 'blocked').length)

// 进度百分比（供顶部细进度条使用）
const progressPercent = computed(() => {
  if (!nodes.value.length) return 0
  return Math.round((doneCount.value / nodes.value.length) * 100)
})

// 进度计数文案：分析阶段尚未生成子任务时显示分析进度（或"分析中…"）而非"0/0"，避免误判卡死
// 注意：只有 status 真的是 analyzing 才显示分析文案。曾经用 isLive（含 running）判断，
// 导致 running 且节点已拆出但列表尚未拉到时，仍渲染残留的 analyzeMessage → 假「分析中」。
const progressLabel = computed(() => {
  if (nodes.value.length) {
    const base = `${doneCount.value}/${nodes.value.length}`
    // 有降级/受阻时一并标出，避免「3/5」这种汇总掩盖质量差异
    const extra = []
    if (degradedCount.value) extra.push(`${degradedCount.value} 降级`)
    if (blockedCount.value) extra.push(`${blockedCount.value} 受阻`)
    return extra.length ? `${base}（${extra.join('、')}）` : base
  }
  if (workflow.value?.status === 'analyzing') return workflow.value?.analyzeMessage || '分析中…'
  return '0/0'
})

// 进行中（需轮询刷新）
const isLive = computed(() => {
  const s = workflow.value?.status
  return s === 'running' || s === 'analyzing' || s === 'interrupted'
})

function toggle() {
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
  window.addEventListener('pointerup', onGlobalPointerUp, { passive: true })
  window.addEventListener('pointercancel', onGlobalPointerUp, { passive: true })
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
  clearMarkdownCache()
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

// ---- 轮询节流与退避 ----
// 原实现固定 1.5s 一跳、每跳两个请求（status + nodes），错误全静默，
// 页面切到后台也照打。这里做三件事：
//   1. 文档不可见时暂停（visibilitychange 恢复时立刻补一跳）；
//   2. 连续失败指数退避（1.5s → 3s → 6s …，上限 30s），成功即复位；
//   3. 失败到一定次数后给一次可见提示，不再永远静默。
const POLL_BASE_MS = 1500
const POLL_MAX_MS = 30_000
const FAIL_NOTIFY_AT = 3
let pollFails = 0
let failNotified = false

function nextDelay() {
  if (!pollFails) return POLL_BASE_MS
  return Math.min(POLL_BASE_MS * 2 ** pollFails, POLL_MAX_MS)
}

async function tick() {
  if (!props.workflowId) return
  // 后台标签页不轮询：工作流状态在切回来时补拉即可
  if (typeof document !== 'undefined' && document.hidden) return
  try {
    const res = await fetchState()
    pollFails = 0
    failNotified = false
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
        // sessionId 为空说明调用方没把会话 ID 传进来（历史上误传过 botId），
        // 此时续跑请求必然打不中，宁可留一条告警也不要静默失败。
        if (props.sessionId) botStore.resumeContinuation(props.sessionId)
        else console.warn('workflow needsContinuation 但 sessionId 为空，已跳过续跑')
      }
    }
  } catch (e) {
    pollFails++
    // 一直失败下去要让用户知道（例如工作流已被清理、或登录态失效）
    if (pollFails >= FAIL_NOTIFY_AT && !failNotified) {
      failNotified = true
      MessagePlugin.warning('工作流状态刷新失败，正在降低频率重试')
    }
  }
}

let pollStopped = true

// 自调度轮询：用 setTimeout 串联而非 setInterval，才能在失败时拉长间隔，
// 也避免上一跳未返回就重复发起（两个请求叠加会放大后端压力）。
function scheduleNext() {
  if (pollStopped) return
  pollTimer = setTimeout(async () => {
    pollTimer = null
    if (pollStopped) return
    await tick()
    if (pollStopped || isTerminal.value) return
    scheduleNext()
  }, nextDelay())
}

function startLive() {
  stopLive()
  pollStopped = false
  pollFails = 0
  failNotified = false
  scheduleNext()
}

function stopLive() {
  pollStopped = true
  if (pollTimer) { clearTimeout(pollTimer); pollTimer = null }
}

// 切回前台立刻补一跳，把后台期间落下的状态追上
function onVisibilityChange() {
  if (document.hidden || pollStopped) return
  pollFails = 0
  tick().then(() => {
    if (!pollStopped && !isTerminal.value && !pollTimer) scheduleNext()
  })
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

document.addEventListener('visibilitychange', onVisibilityChange)

onBeforeUnmount(() => {
  stopLive()
  if (stopSpring) stopSpring()
  document.removeEventListener('visibilitychange', onVisibilityChange)
  window.removeEventListener('pointerup', onGlobalPointerUp)
  window.removeEventListener('pointercancel', onGlobalPointerUp)
})
</script>

<style scoped>
/* 面板作为内嵌卡片：宽度由外层容器（ChatWindow 的 .wf-inline）约束，
   自身不再设 max-width/auto margin，否则与外层重复约束。 */
.wf-panel {
  position: relative;
  overflow: hidden;
  border: none;
  border-radius: 12px;
  will-change: transform;
  background: var(--bp-surface);
  box-shadow: var(--bp-shadow-sm);
  transition: box-shadow var(--bp-duration) var(--bp-ease-out), transform var(--bp-duration) var(--bp-ease-out);
}
.wf-panel:hover { box-shadow: var(--bp-shadow-md); }

/* 顶部状态色条替代原来的左侧粗边框：更轻、且不挤压内容 */
.wf-panel::before {
  content: '';
  position: absolute;
  inset: 0 0 auto;
  height: 2px;
  background: var(--bp-separator-opaque);
}
.wf-panel.wf-running::before,
.wf-panel.wf-analyzing::before {
  background: linear-gradient(90deg, var(--bp-accent), var(--bp-accent-hover), var(--bp-accent));
  background-size: 200% 100%;
  animation: wf-flow 2.2s linear infinite;
}
.wf-panel.wf-completed::before { background: var(--bp-success); }
.wf-panel.wf-failed::before { background: var(--bp-danger); }
.wf-panel.wf-terminated::before { background: var(--bp-label-quaternary); }
@keyframes wf-flow {
  0% { background-position: 100% 0; }
  100% { background-position: -100% 0; }
}

.wf-head {
  display: flex;
  align-items: center;
  gap: 8px;
  cursor: pointer;
  user-select: none;
  padding: 10px 14px;
  border-radius: 12px 12px 0 0;
  transition: background var(--bp-duration) var(--bp-ease-out);
}
.wf-head:hover { background: var(--bp-bg-subtle); }
.wf-status {
  flex-shrink: 0;
  font-size: 12px;
  font-weight: 500;
  letter-spacing: var(--bp-tracking-caption);
  color: var(--bp-label-secondary);
}
.wf-st-running, .wf-st-analyzing { color: var(--bp-accent); }
.wf-st-completed { color: var(--bp-success); }
.wf-st-failed { color: var(--bp-danger); }
.wf-st-interrupted { color: var(--bp-warning); }
.wf-st-terminated { color: var(--bp-label-tertiary); }
.wf-goal {
  flex-shrink: 0;
  font-size: 12px;
  color: var(--bp-warning);
  font-variant-numeric: tabular-nums;
}
.wf-chevron {
  color: var(--bp-label-tertiary);
  flex-shrink: 0;
  pointer-events: none;
}
.wf-title {
  font-weight: 600;
  font-size: 13.5px;
  color: var(--bp-label);
  line-height: 1.5;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  flex: 1;
  min-width: 0;
}
.wf-progress-text {
  font-size: 12px;
  color: var(--bp-label-tertiary);
  font-variant-numeric: tabular-nums;
  flex-shrink: 0;
}
.wf-body-wrap {
  display: grid;
  grid-template-rows: 0fr;
  overflow: hidden;
  transition: grid-template-rows var(--bp-duration) var(--bp-ease-out);
}
.wf-body-wrap.is-open { grid-template-rows: 1fr; }
.wf-body-inner { min-height: 0; overflow: hidden; }

/* 进度条：节点已拆出时展示完成比例，长任务里比"3/10" 更直观 */
.wf-progress-bar {
  height: 2px;
  background: var(--bp-surface-fill);
  overflow: hidden;
}
.wf-progress-fill {
  height: 100%;
  border-radius: 0 2px 2px 0;
  background: var(--bp-accent);
  transition: width var(--bp-duration) var(--bp-ease-out);
}
.wf-panel.wf-completed .wf-progress-fill { background: var(--bp-success); }
.wf-panel.wf-failed .wf-progress-fill { background: var(--bp-danger); }

/* 一维 TODO 清单 */
.wf-todo {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 10px 14px 12px;
  background: var(--bp-bg-subtle);
  border-top: var(--bp-hairline);
}
.todo-item {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 8px 10px;
  border-radius: 8px;
  background: var(--bp-surface);
  border: var(--bp-hairline);
  transition: border-color var(--bp-duration) var(--bp-ease-out), background var(--bp-duration) var(--bp-ease-out);
}
.todo-item.todo-failed { border-color: var(--bp-danger); background: var(--bp-danger-soft); }
.todo-item.todo-running { border-color: var(--bp-accent); background: var(--bp-accent-soft); }
.todo-item.todo-reviewing { border-color: var(--bp-warning); background: var(--bp-warning-soft); }
.todo-item.todo-completed { border-color: var(--bp-separator); background: var(--bp-surface); }
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
  color: var(--bp-label-on-accent);
  background: var(--bp-label-quaternary);
  /* todo-item 已改为顶部对齐（详情展开后可能很高），此处微调与首行文字视觉对齐 */
  margin-top: 1px;
}
.check-completed { background: var(--bp-success); }
.check-failed { background: var(--bp-danger); }
.check-running {
  background: transparent;
  color: var(--bp-accent);
  border: 2px solid var(--bp-accent);
  box-shadow: 0 0 0 3px var(--bp-accent-soft);
}
.check-reviewing { background: var(--bp-warning); }
.check-terminated,
.check-skipped { background: var(--bp-label-quaternary); }

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
  color: var(--bp-label);
  line-height: 1.5;
}
.todo-name.done { color: var(--bp-label-secondary); text-decoration: line-through; opacity: 0.8; }
.todo-status {
  font-size: 10.5px;
  font-weight: 500;
  line-height: 17px;
  padding: 0 7px;
  border-radius: 9px;
  background: var(--bp-surface-fill);
  color: var(--bp-label-secondary);
  flex-shrink: 0;
}
.st-running { background: var(--bp-accent-soft); color: var(--bp-accent); }
.st-completed { background: var(--bp-success-soft); color: var(--bp-success); }
.st-failed { background: var(--bp-danger-soft); color: var(--bp-danger); }
.st-reviewing { background: var(--bp-warning-soft); color: var(--bp-warning); }
.st-terminated,
.st-skipped { background: var(--bp-surface-fill); color: var(--bp-label-tertiary); }

/* 结果类别徽标：与 todo-status 同高，避免行高抖动 */
.todo-outcome {
  flex-shrink: 0;
  height: 17px;
  line-height: 17px;
  padding: 0 6px;
  font-size: 10.5px;
  border-radius: 9px;
  cursor: help;
  background: var(--bp-surface-fill);
  color: var(--bp-label-secondary);
}
.todo-outcome.outcome-warning { background: var(--bp-warning-soft); color: var(--bp-warning); }
.todo-outcome.outcome-danger { background: var(--bp-danger-soft); color: var(--bp-danger); }
.todo-outcome.outcome-default { background: var(--bp-surface-fill); color: var(--bp-label-secondary); }

.todo-retry-count {
  font-size: 10.5px;
  color: var(--bp-warning);
  background: var(--bp-warning-soft);
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
.detail-head.foldable:hover { background: var(--bp-surface-fill); }
.detail-head.foldable:focus-visible {
  outline: 2px solid var(--bp-accent);
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
  color: var(--bp-label-tertiary);
  font-variant-numeric: tabular-nums;
}
.detail-caret { flex-shrink: 0; color: var(--bp-label-tertiary); font-size: 14px; }

.detail-error { color: var(--bp-danger); }
.detail-ok { color: var(--bp-success); }

.detail-wrap {
  margin-top: 6px;
  border: var(--bp-hairline);
  border-radius: 8px;
  background: var(--bp-surface);
  overflow: hidden;
}
.detail-body {
  max-height: 340px;
  overflow-y: auto;
  padding: 10px 12px;
  font-size: 12.5px;
  line-height: 1.75;
  color: var(--bp-label);
  overflow-wrap: break-word;
}
.detail-actions {
  display: flex;
  justify-content: flex-end;
  gap: 2px;
  padding: 2px 6px;
  border-top: var(--bp-hairline);
  background: var(--bp-bg-subtle);
}

/* markdown 渲染内容样式（v-html 需 :deep 穿透 scoped） */
.markdown-body :deep(h1),
.markdown-body :deep(h2),
.markdown-body :deep(h3),
.markdown-body :deep(h4) {
  margin: 12px 0 6px;
  font-weight: 600;
  line-height: 1.4;
  color: var(--bp-label);
}
.markdown-body :deep(h1) { font-size: 15px; }
.markdown-body :deep(h2) {
  font-size: 14px;
  padding-bottom: 4px;
  border-bottom: var(--bp-hairline);
}
.markdown-body :deep(h3) { font-size: 13px; }
.markdown-body :deep(h4) { font-size: 12.5px; }
.markdown-body :deep(> :first-child) { margin-top: 0; }
.markdown-body :deep(p) { margin: 6px 0; }
.markdown-body :deep(ul),
.markdown-body :deep(ol) { margin: 6px 0; padding-left: 20px; }
.markdown-body :deep(li) { margin: 2px 0; }
.markdown-body :deep(li > p) { margin: 0; }
.markdown-body :deep(strong) { font-weight: 600; color: var(--bp-label); }
.markdown-body :deep(code) {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 11.5px;
  padding: 1px 5px;
  border-radius: 4px;
  background: var(--bp-surface-fill);
  color: var(--bp-danger);
  overflow-wrap: break-word;
}
.markdown-body :deep(pre) {
  margin: 8px 0;
  padding: 10px 12px;
  border-radius: 6px;
  background: var(--bp-bg-subtle);
  border: var(--bp-hairline);
  overflow-x: auto;
}
.markdown-body :deep(pre code) {
  padding: 0;
  background: none;
  color: var(--bp-label);
  font-size: 11.5px;
  line-height: 1.65;
  white-space: pre;
}
.markdown-body :deep(blockquote) {
  margin: 8px 0;
  padding: 2px 0 2px 10px;
  border-left: 3px solid var(--bp-separator);
  color: var(--bp-label-secondary);
}
.markdown-body :deep(table) {
  width: 100%;
  margin: 8px 0;
  border-collapse: collapse;
  font-size: 11.5px;
}
.markdown-body :deep(th),
.markdown-body :deep(td) {
  border: var(--bp-hairline);
  padding: 4px 8px;
  text-align: left;
}
.markdown-body :deep(th) { background: var(--bp-bg-subtle); font-weight: 600; }
.markdown-body :deep(hr) {
  margin: 12px 0;
  border: none;
  border-top: var(--bp-hairline);
}
.markdown-body :deep(a) { color: var(--bp-accent); }

.todo-retry-btn { flex-shrink: 0; margin-top: -2px; }
.todo-empty {
  font-size: 12px;
  color: var(--bp-label-tertiary);
  padding: 10px;
  text-align: center;
  border-radius: 8px;
  background: var(--bp-surface);
  border: 1px dashed var(--bp-separator);
}
.todo-fail-title { color: var(--bp-danger); font-weight: 500; }
.todo-fail-reason {
  margin-top: 4px;
  color: var(--bp-warning);
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
@media (prefers-reduced-motion: reduce) {
  .wf-body-wrap { transition: none; }
  .wf-head { transition: none; }
  .wf-panel.wf-running::before,
  .wf-panel.wf-analyzing::before { animation: none; }
  .live-dot { animation: none; }
}
</style>
