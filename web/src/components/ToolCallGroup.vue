<template>
  <div
    ref="rootRef"
    class="tool-call-group"
    :class="`tcg-${aggState}`"
    :data-testid="`chat-toolgroup-${name}`"
    :data-group-name="name"
    :data-group-count="calls.length"
  >
    <div
      class="tcg-head"
      :data-testid="`chat-toolgroup-head-${name}`"
      role="button"
      tabindex="0"
      :aria-expanded="expanded"
      @click="toggle"
      @pointerdown="onHeadPointerDown"
      @keydown.enter.prevent="toggle"
      @keydown.space.prevent="toggle"
    >
      <span class="tcg-icon" :class="`icon-${aggState}`">
        <span v-if="aggState === 'running'" class="tc-spinner" />
        <t-icon v-else :name="aggIcon" />
      </span>
      <span class="tcg-title">
        {{ displayName }}
        <span class="tcg-count">×{{ calls.length }}</span>
      </span>
      <span class="tcg-meta">{{ aggMeta }}</span>
      <t-icon
        :name="expanded ? 'chevron-up' : 'chevron-down'"
        class="tcg-chevron"
        :data-testid="`chat-toolgroup-toggle-${name}`"
        aria-hidden="true"
      />
    </div>

    <div class="tcg-body-wrap" :class="{ 'is-open': expanded }">
      <div class="tcg-body-inner">
        <div class="tcg-body">
          <div v-for="c in calls" :key="c.id" class="tc-group-item">
            <ToolCallCard :call="c" />
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import ToolCallCard from '@/components/ToolCallCard.vue'
import toolLabels from '@/i18n/toolLabels'
import { animateSpring, prefersReducedMotion } from '@/utils/spring'

const props = defineProps({
  name: { type: String, required: true },
  calls: { type: Array, required: true }
})

const rootRef = ref(null)
const displayName = computed(() => toolLabels[props.name] || props.name)

const aggState = computed(() => {
  const calls = props.calls || []
  if (calls.some(c => c.status === 'running')) return 'running'
  if (calls.some(c => c.status === 'killed')) return 'killed'
  if (calls.some(c => c.status === 'error')) return 'error'
  return 'success'
})

const aggIcon = computed(() => {
  switch (aggState.value) {
    case 'error': return 'close-circle'
    case 'killed': return 'minus-circle'
    case 'success': return 'check-circle'
    default: return 'check-circle'
  }
})

const aggMeta = computed(() => {
  const calls = props.calls || []
  const n = calls.length
  if (aggState.value === 'running') return '执行中'
  const errs = calls.filter(c => c.status === 'error').length
  const superseded = calls.filter(c => c.status === 'superseded').length
  if (errs > 0) return `${n} 次 · ${errs} 失败`
  if (superseded > 0) return `已完成 ${n - superseded} 次 · ${superseded} 已取代`
  return `已完成 ${n} 次`
})

const expanded = ref(true)
const userToggled = ref(false)
watch(
  () => (props.calls || []).some(c => c.status === 'running'),
  (running) => {
    if (!running && !userToggled.value) expanded.value = false
    if (running && !userToggled.value) expanded.value = true
  },
  { immediate: true }
)

function toggle() {
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
onUnmounted(() => {
  if (stopSpring) stopSpring()
  window.removeEventListener('pointerup', onGlobalPointerUp)
  window.removeEventListener('pointercancel', onGlobalPointerUp)
})
</script>

<style scoped>
.tool-call-group {
  border: none;
  border-radius: var(--bp-radius-md);
  margin: 4px 0;
  overflow: hidden;
  background: var(--bp-surface);
  box-shadow: var(--bp-shadow-sm);
  will-change: transform;
}
.tcg-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  cursor: pointer;
  user-select: none;
  background: var(--bp-surface-fill-hover);
  transition: background var(--bp-duration) var(--bp-ease-out);
}
.tcg-head:hover { background: var(--bp-surface-fill-active); }
.tcg-icon { display: inline-flex; }
.tcg-title {
  font-weight: 600;
  color: var(--bp-label);
  letter-spacing: var(--bp-tracking-body);
}
.tcg-count {
  color: var(--bp-label-tertiary);
  font-weight: 400;
  margin-left: 2px;
}
.tcg-meta {
  flex: 1;
  text-align: right;
  color: var(--bp-label-secondary);
  font-size: 12px;
}
.tcg-chevron {
  color: var(--bp-label-tertiary);
  pointer-events: none;
}

.tcg-running .tcg-icon { color: var(--bp-accent); }
.tcg-success .tcg-icon { color: var(--bp-success); }
.tcg-error .tcg-icon { color: var(--bp-danger); }
.tcg-killed .tcg-icon { color: var(--bp-warning); }

.tcg-body-wrap {
  display: grid;
  grid-template-rows: 0fr;
  overflow: hidden;
  transition: grid-template-rows var(--bp-duration) var(--bp-ease-out);
}
.tcg-body-wrap.is-open { grid-template-rows: 1fr; }
.tcg-body-inner {
  min-height: 0;
  overflow: hidden;
}
.tcg-body {
  padding: 4px 8px 6px;
  border-top: var(--bp-hairline);
}
.tcg-body .tc-group-item { margin: 2px 0 2px 6px; }
.tcg-body .tc-group-item :deep(.tool-call) {
  margin-top: 0;
  border-left: 2px solid var(--bp-separator);
  border-radius: 0 var(--bp-radius-sm) var(--bp-radius-sm) 0;
}

.tc-spinner {
  width: 13px;
  height: 13px;
  border: 2px solid var(--bp-accent-soft-strong);
  border-top-color: var(--bp-accent);
  border-radius: 50%;
  display: inline-block;
  animation: tcg-spin 0.7s linear infinite;
}
@keyframes tcg-spin { to { transform: rotate(360deg); } }

@media (prefers-reduced-motion: reduce) {
  .tcg-body-wrap { transition: none; }
  .tcg-head { transition: none; }
  .tc-spinner { animation: none; opacity: 0.7; }
}
</style>
