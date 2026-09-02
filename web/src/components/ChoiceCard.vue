<template>
  <!--
    ChoiceCard — user_choice 交互的内联选择卡片。

    数据契约对齐 n7 后端（internal/userchoice，Web channel 适配层）：
      payload = { questionId, question, mode:'single'|'multiple',
                  options:[{id,label,description?}], inputHint?, timeoutAt? }
      提交     = POST /api/user-choice/{questionId}/answer
                 body: { selectedIds:[...], freeText:'' }
      终态     = 工具 tool_result 的 status（timeout/cancelled/resolved...）经由
                 status prop 传入；刷新后由 timeoutAt 估算兜底。

    动效契约（skills/apple-design）：
      - 按压反馈在 pointerdown 触发（§1 Response），不等待 click；
      - 选中指示器用弹簧生长 spring(damping 1.0, response 0.4)（§4，无过冲）；
      - 所有弹簧从"当前呈现值+当前速度"出发，可中断可反转（§3）；
      - reduced-motion 时 JS 短路弹簧 + CSS 关动画（§14）；
      - 卡片本体纯白不透明，不与消息流叠加半透明材质（§12）。
  -->
  <div
    v-if="question"
    ref="rootRef"
    class="cc-card"
    :class="['cc-' + phase, 'cc-' + mode]"
    :data-testid="'choice-card-' + questionId"
    :data-phase="phase"
    :data-mode="mode"
    role="group"
    :aria-label="ariaLabel"
  >
    <!-- 顶部状态色条：与 SessionWorkflowPanel 同一视觉语言 -->
    <div class="cc-rail" aria-hidden="true" />

    <!-- 问题区 -->
    <div class="cc-head">
      <span class="cc-icon" aria-hidden="true">🧭</span>
      <span class="cc-q" data-testid="choice-question">{{ question }}</span>
      <span v-if="multi" class="cc-multi-badge" data-testid="choice-multi-badge">可多选</span>
    </div>

    <!-- 选项区：单选 = radio 语义（点击即提交）；多选 = checkbox 语义 + 确认按钮 -->
    <div class="cc-options" :role="multi ? 'group' : 'radiogroup'" :aria-label="question">
      <button
        v-for="opt in options"
        :key="opt.id"
        :ref="(el) => setOptionRef(opt.id, el)"
        type="button"
        class="cc-opt"
        :class="{ selected: selectedIds.includes(opt.id), locked: isLocked }"
        :role="multi ? 'checkbox' : 'radio'"
        :aria-checked="selectedIds.includes(opt.id) ? 'true' : 'false'"
        :aria-disabled="isLocked ? 'true' : 'false'"
        :tabindex="isLocked ? -1 : 0"
        :data-testid="'choice-option-' + opt.id"
        :data-selected="selectedIds.includes(opt.id) ? 'true' : 'false'"
        @pointerdown="onOptionPointerDown($event, opt)"
        @click="onOptionActivate(opt)"
        @keydown="onOptionKeydown($event, opt)"
      >
        <span class="cc-glyph" aria-hidden="true">
          <!-- 指示器：box（外框）+ mark（勾/点）。mark 的显隐由弹簧驱动
               stroke-dashoffset，从 0 生长到全长——"选中痕迹长出来"而非跳变 -->
          <svg v-if="multi" viewBox="0 0 20 20" class="cc-glyph-svg">
            <path class="cc-box" d="M4.5 3.5h11a1 1 0 0 1 1 1v11a1 1 0 0 1-1 1h-11a1 1 0 0 1-1-1v-11a1 1 0 0 1 1-1z" />
            <path class="cc-mark" d="M6.4 10.3l2.5 2.5 4.7-5.4" />
          </svg>
          <svg v-else viewBox="0 0 20 20" class="cc-glyph-svg">
            <circle class="cc-box" cx="10" cy="10" r="6.6" />
            <circle class="cc-mark" cx="10" cy="10" r="2.8" />
          </svg>
        </span>
        <span class="cc-opt-body">
          <span class="cc-opt-label">{{ opt.label }}</span>
          <span v-if="opt.description" class="cc-opt-desc">{{ opt.description }}</span>
        </span>
      </button>
    </div>

    <!-- 多选确认条：紧贴其影响的选项区（分组与映射），选了至少一项才可用 -->
    <div v-if="multi && (phase === 'active' || phase === 'submitting')" class="cc-confirm-bar">
      <span class="cc-count" data-testid="choice-count">
        {{ selectedIds.length ? '已选 ' + selectedIds.length + ' 项' : '可勾选多项后确认' }}
      </span>
      <button
        ref="confirmBtnRef"
        type="button"
        class="cc-confirm"
        :disabled="!selectedIds.length || submitting"
        :data-testid="'choice-confirm-' + questionId"
        @pointerdown="onBarPointerDown(confirmBtnRef)"
        @click="onConfirm"
      >
        <t-icon v-if="submitting" name="loading" class="cc-spin" />
        <template v-else>确认选择</template>
      </button>
    </div>

    <!-- 底部常驻自由输入 + 提交按钮（placeholder 用 payload.inputHint） -->
    <div v-if="phase === 'active' || phase === 'submitting'" class="cc-input-row">
      <input
        ref="inputRef"
        v-model="freeText"
        type="text"
        class="cc-input"
        :placeholder="inputPlaceholder"
        data-testid="choice-input"
        :aria-label="freeAriaLabel"
        :disabled="submitting"
        @keydown.enter.prevent="onFreeSubmit"
      />
      <button
        ref="sendBtnRef"
        type="button"
        class="cc-send"
        :class="{ silent: !canFreeSubmit }"
        :disabled="submitting"
        data-testid="choice-send"
        aria-label="提交选择"
        @pointerdown="onBarPointerDown(sendBtnRef)"
        @click="onFreeSubmit"
      >
        <t-icon v-if="submitting" name="loading" class="cc-spin" />
        <t-icon v-else name="arrow-up" />
      </button>
    </div>

    <!-- 终态脚注：显示所选内容 / 超时说明（锁定态唯一的信息出口） -->
    <div v-if="isLocked" class="cc-foot" data-testid="choice-foot">
      <template v-if="phase === 'answered'">
        <span class="cc-foot-mark ok" aria-hidden="true">✓</span>
        <span class="cc-foot-text" data-testid="choice-answered-summary">已选择：{{ answeredSummary }}</span>
      </template>
      <template v-else-if="phase === 'timeout'">
        <span class="cc-foot-mark warn" aria-hidden="true">⏱</span>
        <span class="cc-foot-text">已超时，本次选择已失效</span>
      </template>
      <template v-else-if="phase === 'cancelled'">
        <span class="cc-foot-mark warn" aria-hidden="true">–</span>
        <span class="cc-foot-text">已取消，本次选择未生效</span>
      </template>
      <template v-else-if="phase === 'failed'">
        <span class="cc-foot-mark warn" aria-hidden="true">!</span>
        <span class="cc-foot-text">选择提交失败，本题已失效</span>
      </template>
    </div>
    <!-- 激活态：超时倒计时（有截止时间才显示，避免无期限问题徒增压力） -->
    <div v-else-if="hasDeadline" class="cc-timer" data-testid="choice-timer">
      <span class="cc-timer-num">{{ timerText }}</span>
      <span class="cc-timer-lbl">后超时</span>
    </div>
    <div v-else class="cc-foot-spacer" aria-hidden="true" />
  </div>
</template>

<script setup>
import { ref, computed, watch, onMounted, onBeforeUnmount, nextTick } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { userChoiceApi } from '@/api/services'
import { useBotStore } from '@/stores/bot'
import { animateSpring, prefersReducedMotion } from '@/utils/spring'

/**
 * props：
 *  - payload    n7 定义的卡片负载（questionId/question/mode/options/inputHint/timeoutAt）
 *  - status     工具执行状态：''（未知）| 'running' | 'success' | 'error' | 'timeout'
 *               | 'cancelled' | 'killed' | 'answered' | 'resolved'
 *  - submitted  父级（store）确认"这一题已成功提交"的独立信号；与 status 互补，
 *               刷新后 toolCalls 里的终态 status 仍可恢复锁定态
 */
const props = defineProps({
  payload: { type: Object, required: true },
  status: { type: String, default: '' },
  submitted: { type: Boolean, default: false },
})

// store：终态回显（answer）与提交标记的持久来源（刷新后仍可恢复锁定态与所选内容）
const store = useBotStore()

// ── payload 解包（防御式：任何字段缺失都降级为安全默认值，渲染永不崩） ──
const questionId = computed(() => String(props.payload?.questionId || ''))
const question = computed(() => String(props.payload?.question || '请选择'))
const mode = computed(() => (props.payload?.mode === 'multiple' ? 'multiple' : 'single'))
const multi = computed(() => mode.value === 'multiple')
const options = computed(() => {
  const list = Array.isArray(props.payload?.options) ? props.payload.options : []
  return list
    .filter((o) => o && o.id != null)
    .map((o) => ({
      id: String(o.id),
      label: String(o.label ?? o.id),
      description: o.description ? String(o.description) : '',
    }))
})
const inputHint = computed(() => String(props.payload?.inputHint || ''))
const timeoutAt = computed(() => {
  const v = props.payload?.timeoutAt
  if (!v) return 0
  const t = typeof v === 'number' ? v : Date.parse(v)
  return Number.isFinite(t) && t > 0 ? t : 0
})
const inputPlaceholder = computed(() => inputHint.value || '补充说明（可选）')
const ariaLabel = computed(() => question.value + '（' + (multi.value ? '多选' : '单选') + '）')
const freeAriaLabel = computed(() => '补充输入：' + inputPlaceholder.value)

// ── 状态机：active → submitting → answered | timeout | cancelled | failed ──
// localPhase 由本组件自持（提交成败的即时反馈），props.status 提供外部终态；
// 二者取"先到者"，避免父级状态抖动把已锁定的 UI 回滚。
const localPhase = ref('')
const submitting = ref(false)
const phase = computed(() => {
  if (localPhase.value) return localPhase.value
  if (props.submitted) return 'answered'
  const s = props.status
  if (s === 'timeout') return 'timeout'
  if (s === 'cancelled' || s === 'killed') return 'cancelled'
  if (s === 'success' || s === 'answered' || s === 'resolved') return 'answered'
  if (s === 'error') return 'failed'
  return 'active'
})
const isLocked = computed(() => phase.value !== 'active')

// ── 选中集合与自由文本 ──
// 初始值优先取 store 里持久化的 answer（刷新后恢复用户已选内容），
// 其次才是本地空态——保证"提交后显示所选内容"跨刷新仍然成立。
const storeState = computed(() => store.choiceState(questionId.value))
const restoredAnswer = computed(() => storeState.value?.answer || null)
const selectedIds = ref(restoredAnswer.value ? [...restoredAnswer.value.selectedIds] : [])
const freeText = ref(restoredAnswer.value ? restoredAnswer.value.freeText : '')

// 终态回显摘要：所选 label 串联 + 补充文本（锁定态"显示所选内容"）
const answeredSummary = computed(() => {
  const labels = options.value
    .filter((o) => selectedIds.value.includes(o.id))
    .map((o) => o.label)
  const extra = freeText.value.trim()
  const parts = []
  if (labels.length) parts.push(labels.join('、'))
  if (extra) parts.push('补充：' + extra)
  return parts.length ? parts.join('；') : '（无）'
})

// ── 超时倒计时（仅展示；真正的超时判定以后端事件为准，前端不武断锁死） ──
const nowTs = ref(Date.now())
let timerRaf = 0
const hasDeadline = computed(() => timeoutAt.value > 0 && phase.value === 'active')
const remainMs = computed(() => Math.max(0, timeoutAt.value - nowTs.value))
const timerText = computed(() => {
  const s = Math.ceil(remainMs.value / 1000)
  const m = Math.floor(s / 60)
  return m > 0 ? m + ':' + String(s % 60).padStart(2, '0') : s + 's'
})
function tickTimer() {
  nowTs.value = Date.now()
  timerRaf = requestAnimationFrame(tickTimer)
}

// ── DOM 引用表：选项按钮按 id 注册，供弹簧与键盘巡游定位 ──
const optRefs = new Map()
function setOptionRef(id, el) {
  if (el) optRefs.set(id, el)
  else optRefs.delete(id)
}
const rootRef = ref()
const inputRef = ref()
const sendBtnRef = ref()
const confirmBtnRef = ref()

// ── 选中指示器弹簧：mark 从 0 生长到 1（damping 1.0 / response 0.4，无过冲） ──
// 用 stroke-dashoffset 表达"长出来"的过程；取消选中时反向收回。
// 关键：动画上下文以 **mark 元素自身** 为键，与按钮的按压弹簧（scale 域，
// 以按钮元素为键）彻底隔离——否则 click（紧随 pointerup 触发）启动的选中
// 弹簧会覆盖仍在回弹中的按压弹簧上下文，把 transform 冻结在中间值，
// 且 mark 会因共享 pos 而不从 0 开始生长（两域数值语义不同，绝不能共域）。
const MARK_LEN = { check: 13, dot: 18 } // 勾/圆点的路径近似长度
function setMark(mark, kind, v) {
  const total = MARK_LEN[kind] || 14
  // 预留 2px 余量，避免 100% 时端点露出微小缝隙
  mark.style.strokeDasharray = String(total + 2)
  mark.style.strokeDashoffset = String((total + 2) * (1 - v))
  mark.style.opacity = v <= 0.02 ? '0' : '1'
}
function springSelection(el, on) {
  if (!el) return
  const mark = el.querySelector ? el.querySelector('.cc-mark') : null
  if (!mark) return
  const kind = multi.value ? 'check' : 'dot'
  const reduce = prefersReducedMotion()
  animateSpring({
    el: mark, // 以 mark 元素为键（与按压弹簧的按钮键隔离，见上）
    from: on ? 0 : 1, // 首次调用无上下文，必须显式给起点，否则 pos=to 直接跳变
    to: on ? 1 : 0,
    damping: 1.0,
    response: 0.4,
    disabled: reduce, // reduced-motion：直接落终值（静态呈现，保留颜色语义）
    onUpdate: (v) => setMark(mark, kind, v),
  })
}

// ── 按压即时反馈（§1：pointerdown 即时，不等 click） ──
// 按下 → 快弹簧缩到 0.97（response 0.16，手还按着，必须跟手）；
// 抬起 → 标准弹簧回 1.0（damping 1.0 / response 0.4，Apple 默认 UI 档）。
// 同一元素连续按压复用同一动画上下文：从当前呈现值与速度续跑，可被打断反转。
const pressingId = ref('')
function scaleTo(el, to, response, then) {
  // 首次按压时该元素尚无动画上下文，animateSpring 的 pos 会直接等于 to（跳变）。
  // 因此首次必须显式给起点：按物理语义，静止按钮的呈现值就是 1。
  animateSpring({
    el,
    from: 1,
    to,
    damping: 1.0,
    response,
    onUpdate: (v) => { el.style.transform = 'scale(' + v + ')' },
    onComplete: then,
  })
}
function onOptionPointerDown(e, opt) {
  if (isLocked.value || submitting.value) return
  if (e.button != null && e.button !== 0) return // 只响应主键
  pressingId.value = opt.id
  const el = optRefs.get(opt.id)
  if (el) scaleTo(el, 0.97, 0.16)
}
// window 级监听：指针拖出元素再抬起也能正确复位（cancel-by-dragging-away）
function onGlobalPointerUp() {
  const id = pressingId.value
  if (!id) return
  pressingId.value = ''
  const el = optRefs.get(id)
  if (el) scaleTo(el, 1, 0.4)
}
// 确认/发送按钮的按压：与选项同一套物理语言
function onBarPointerDown(el) {
  if (!el || isLocked.value || submitting.value) return
  scaleTo(el, 0.94, 0.16, () => scaleTo(el, 1, 0.4))
}

// ── 交互语义 ──
function onOptionActivate(opt) {
  if (isLocked.value || submitting.value) return
  const el = optRefs.get(opt.id)
  if (multi.value) {
    // 多选：切换勾选，等"确认选择"统一提交
    const i = selectedIds.value.indexOf(opt.id)
    if (i >= 0) {
      selectedIds.value = selectedIds.value.filter((x) => x !== opt.id)
      springSelection(el, false)
    } else {
      selectedIds.value = [...selectedIds.value, opt.id]
      springSelection(el, true)
    }
  } else {
    // 单选：radio 语义——点击即选即提交
    selectedIds.value = [opt.id]
    springSelection(el, true)
    submit()
  }
}
function onOptionKeydown(e, opt) {
  if (isLocked.value) return
  if (e.key === ' ' && multi.value) {
    // 原生 button 已处理 Enter；Space 默认触发 click，此处不拦截
    return
  }
  if (!multi.value && (e.key === 'ArrowLeft' || e.key === 'ArrowRight' || e.key === 'ArrowUp' || e.key === 'ArrowDown')) {
    // radio 组方向键巡游（WAI-ARIA radiogroup 惯例）
    e.preventDefault()
    const ids = options.value.map((o) => o.id)
    const step = (e.key === 'ArrowRight' || e.key === 'ArrowDown') ? 1 : -1
    const idx = ids.indexOf(opt.id)
    const nextEl = optRefs.get(ids[(idx + step + ids.length) % ids.length])
    if (nextEl) nextEl.focus()
  }
}

// ── 提交（n7 端点：POST /api/user-choice/{questionId}/answer） ──
async function submit() {
  if (submitting.value || isLocked.value) return
  if (!selectedIds.value.length && !freeText.value.trim()) return
  submitting.value = true
  localPhase.value = 'submitting'
  const chosen = [...selectedIds.value]
  const extra = freeText.value.trim()
  try {
    await userChoiceApi.answer(questionId.value, { selectedIds: chosen, freeText: extra })
    localPhase.value = 'answered' // 锁定为「已选择」态（终态入场弹簧见 watch）
    // 同步写回 store：answer + submitted 持久化，刷新页面后终态卡片仍能回显所选内容
    store.markChoiceSubmitted(questionId.value, { selectedIds: chosen, freeText: extra })
    MessagePlugin.success({ message: '选择已提交', placement: 'top', duration: 1600 })
  } catch (e) {
    // 提交失败必须回到可交互态：绝不能把用户锁死在"提交中"
    console.warn('[ChoiceCard] 提交失败', e)
    localPhase.value = ''
    submitting.value = false
    MessagePlugin.error({ message: (e && e.message) || '提交失败，请重试', placement: 'top' })
  }
  submitting.value = false
}
function onConfirm() {
  if (!submitting.value && selectedIds.value.length) submit()
}

// 自由输入：可与选项并存提交；纯文本（未选任何项）也可直接提交
const canFreeSubmit = computed(
  () => phase.value === 'active' && !submitting.value && (freeText.value.trim().length > 0 || selectedIds.value.length > 0)
)
function onFreeSubmit() {
  if (canFreeSubmit.value) submit()
}

// ── 终态入场：卡片轻缩 + 淡入一次（materialize，而非生硬切换） ──
watch(phase, async (p) => {
  if (p === 'active') return
  await nextTick()
  const el = rootRef.value
  if (!el) return
  // 从呈现值出发（0.98 起点），damping 1.0 / response 0.4 无过冲；
  // reduced-motion 时 disabled → 直接落 1（无位移，仅状态色/文案变化）
  animateSpring({
    el,
    from: 0.985,
    to: 1,
    damping: 1.0,
    response: 0.4,
    disabled: prefersReducedMotion(),
    onUpdate: (v) => {
      el.style.transform = 'scale(' + v + ')'
      el.style.opacity = String(0.72 + 0.28 * v)
    },
  })
  // 终态落定后清掉选项上残留的按压 transform，防止锁定态留有 0.97 缩放
  for (const [, oel] of optRefs) oel.style.transform = ''
})

// ── 生命周期：挂全局指针监听 / 清理全部 rAF 与监听（防泄漏与幽灵帧） ──
onMounted(() => {
  if (timeoutAt.value) tickTimer()
  window.addEventListener('pointerup', onGlobalPointerUp, { passive: true })
  window.addEventListener('pointercancel', onGlobalPointerUp, { passive: true })
})
onBeforeUnmount(() => {
  if (timerRaf) cancelAnimationFrame(timerRaf)
  timerRaf = 0
  window.removeEventListener('pointerup', onGlobalPointerUp)
  window.removeEventListener('pointercancel', onGlobalPointerUp)
})
</script>

<style scoped>
/* 卡片本体：与 SessionWorkflowPanel 同语言——纯白面卡片 + 顶部状态色条。
   刻意不用 backdrop-filter：消息流是滚动内容层，卡片属于内容而非悬浮 chrome，
   叠半透明材质会破坏可读性（skill §12：never stack translucent surfaces） */
.cc-card {
  position: relative;
  overflow: hidden;
  border-radius: 12px;
  background: var(--bp-surface);
  box-shadow: var(--bp-shadow-sm);
  padding-bottom: 12px;
  transition: box-shadow var(--bp-duration) var(--bp-ease-out);
  will-change: transform;
}
.cc-card:hover { box-shadow: var(--bp-shadow-md); }

/* 状态色条：active=accent、answered=success、timeout/cancelled/failed=warning */
.cc-rail { position: absolute; inset: 0 0 auto; height: 2px; background: var(--bp-accent); }
.cc-answered .cc-rail { background: var(--bp-success); }
.cc-timeout .cc-rail,
.cc-cancelled .cc-rail,
.cc-failed .cc-rail { background: var(--bp-warning); }

.cc-head { display: flex; align-items: center; gap: 10px; padding: 12px 14px 6px; }
.cc-icon {
  width: 26px; height: 26px; flex-shrink: 0;
  display: flex; align-items: center; justify-content: center;
  font-size: 14px; border-radius: 7px;
  background: var(--bp-accent-soft);
}
.cc-q {
  flex: 1; min-width: 0;
  font-weight: 600; font-size: 13.5px; line-height: 1.5;
  letter-spacing: var(--bp-tracking-body);
  color: var(--bp-label);
}
.cc-multi-badge {
  flex-shrink: 0;
  font-size: 11px; padding: 2px 8px;
  border-radius: var(--bp-radius-pill);
  background: var(--bp-surface-fill);
  color: var(--bp-label-secondary);
  letter-spacing: var(--bp-tracking-caption);
}

/* 选项列表 */
.cc-options { display: flex; flex-direction: column; gap: 6px; padding: 6px 14px 4px; }
.cc-opt {
  display: flex; align-items: center; gap: 10px;
  padding: 9px 12px;
  border-radius: 10px;
  border: var(--bp-hairline);
  background: var(--bp-surface);
  text-align: left;
  cursor: pointer;
  font-family: inherit;
  color: var(--bp-label);
  will-change: transform;
}
.cc-opt:hover:not(.locked) { background: var(--bp-bg-subtle); }
.cc-opt:focus-visible { outline: 2px solid var(--bp-accent); outline-offset: 2px; }
.cc-opt.selected { border-color: var(--bp-accent); background: var(--bp-accent-soft); }
/* 锁定态：禁用一切交互（pointer-events 一并关掉，点击/悬停都不再有反应） */
.cc-opt.locked { cursor: default; pointer-events: none; }
.cc-opt.locked .cc-opt-label { color: var(--bp-label-secondary); }

/* 选中指示器 */
.cc-glyph { width: 20px; height: 20px; flex-shrink: 0; display: block; }
.cc-glyph-svg { width: 100%; height: 100%; display: block; overflow: visible; }
.cc-glyph-svg .cc-box { fill: none; stroke: var(--bp-label-quaternary); stroke-width: 1.4; }
.cc-opt.selected .cc-glyph-svg .cc-box { stroke: var(--bp-accent); }
.cc-glyph-svg .cc-mark {
  fill: none;
  stroke: var(--bp-accent);
  stroke-width: 2.4;
  stroke-linecap: round;
  stroke-linejoin: round;
  /* 初始隐藏态：dashoffset = dasharray 全长（mark 完全收起）。
     取 20：需同时覆盖勾路径（≈10.7）与 radio 圆点周长（2π·2.8≈17.6），
     否则静态兜底态圆点会缺一小段弧。springSelection 的弹簧逐帧写内联
     style 覆盖这里；不选中时保持收起，防止"未选中却显示对勾" */
  stroke-dasharray: 20;
  stroke-dashoffset: 20;
  opacity: 0;
}
/* 选中态（含刷新后恢复的 answered 选项）：mark 全量显示。
   内联弹簧值优先级更高，动画期间以弹簧为准；静止态由本规则兜底 */
.cc-opt.selected .cc-glyph-svg .cc-mark {
  stroke-dashoffset: 0;
  opacity: 1;
}
.cc-opt-body { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.cc-opt-label { font-size: 13.5px; line-height: 1.45; letter-spacing: var(--bp-tracking-body); }
.cc-opt-desc { font-size: 12px; line-height: 1.4; color: var(--bp-label-tertiary); }

/* 多选确认条 */
.cc-confirm-bar { display: flex; align-items: center; justify-content: space-between; padding: 8px 14px 2px; }
.cc-count { font-size: 12px; color: var(--bp-label-tertiary); letter-spacing: var(--bp-tracking-caption); }
.cc-confirm {
  border: none; cursor: pointer;
  padding: 6px 16px; border-radius: var(--bp-radius-pill);
  background: var(--bp-accent); color: var(--bp-label-on-accent);
  font-size: 12.5px; font-weight: 600; font-family: inherit;
  letter-spacing: var(--bp-tracking-caption);
  will-change: transform;
}
.cc-confirm:disabled { opacity: 0.4; cursor: default; }

/* 自由输入行 */
.cc-input-row { display: flex; align-items: center; gap: 8px; padding: 10px 14px 0; }
.cc-input {
  flex: 1; min-width: 0;
  height: 34px; padding: 0 12px;
  border: none; border-radius: 9px;
  background: var(--bp-surface-fill);
  font-size: 13px; font-family: inherit; color: var(--bp-label);
}
.cc-input::placeholder { color: var(--bp-label-tertiary); }
.cc-input:focus { outline: 2px solid var(--bp-accent); outline-offset: 1px; }
.cc-send {
  width: 34px; height: 34px; flex-shrink: 0;
  border: none; border-radius: 50%;
  display: flex; align-items: center; justify-content: center;
  background: var(--bp-accent); color: var(--bp-label-on-accent);
  cursor: pointer; font-size: 15px;
  will-change: transform;
}
.cc-send.silent { background: var(--bp-surface-fill-active); color: var(--bp-label-tertiary); }
.cc-spin { animation: cc-rotate 0.8s linear infinite; }
@keyframes cc-rotate { to { transform: rotate(1turn); } }

/* 终态脚注 */
.cc-foot {
  display: flex; align-items: flex-start; gap: 8px;
  padding: 10px 14px 0;
  font-size: 12.5px; color: var(--bp-label-secondary);
}
.cc-foot-mark { width: 16px; text-align: center; flex-shrink: 0; }
.cc-foot-mark.ok { color: var(--bp-success); }
.cc-foot-mark.warn { color: var(--bp-warning); }
.cc-foot-text { letter-spacing: var(--bp-tracking-caption); word-break: break-word; }

/* 超时倒计时 */
.cc-timer {
  display: flex; align-items: baseline; gap: 4px;
  padding: 10px 14px 0;
  font-size: 12px;
}
.cc-timer-num { font-variant-numeric: tabular-nums; color: var(--bp-warning); font-weight: 600; }
.cc-timer-lbl { color: var(--bp-label-quaternary); letter-spacing: var(--bp-tracking-caption); }
.cc-foot-spacer { height: 2px; }

/* ── 可访问性：reduced motion（skill §14）──
   不是"零反馈"而是温和替代：停掉旋转/位移动画，保留颜色与透明度语义。
   JS 侧 prefersReducedMotion() 已把弹簧短路为直接落值，这里关掉残余 CSS 动画。 */
@media (prefers-reduced-motion: reduce) {
  .cc-card,
  .cc-opt,
  .cc-confirm,
  .cc-send { transition: none; }
  .cc-spin { animation: none; }
}

/* ── 可访问性：更高对比（skill §14 prefers-contrast）── */
@media (prefers-contrast: more) {
  .cc-opt { border: 1px solid var(--bp-separator-opaque); }
  .cc-opt.selected { border-width: 2px; }
  .cc-input { border: 1px solid var(--bp-separator-opaque); }
}
</style>
