<template>
  <div
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

    <div class="tcg-body-wrap" :class="{ 'is-open': expanded }" :inert="!expanded" :aria-hidden="!expanded">
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
import { ref, computed, watch } from 'vue'
import ToolCallCard from '@/components/ToolCallCard.vue'
import toolLabels from '@/i18n/toolLabels'

const props = defineProps({
  name: { type: String, required: true },
  calls: { type: Array, required: true }
})

const displayName = computed(() => {
  const name = props.name
  const key = String(name || "").replace(/^sandbox_/, "")
  return toolLabels[name] || (key !== name ? toolLabels[key] : null) || name
})

const statusCounts = computed(() => {
  const counts = { running: 0, success: 0, error: 0, timeout: 0, cancelled: 0, superseded: 0, unknown: 0 }
  for (const call of props.calls || []) {
    let status = call.status || 'success'
    if (status === 'killed') status = 'cancelled'
    if (status === 'answered' || status === 'resolved') status = 'success'
    counts[Object.hasOwn(counts, status) ? status : 'unknown'] += 1
  }
  return counts
})
const aggState = computed(() => {
  const counts = statusCounts.value
  return ['running', 'error', 'timeout', 'cancelled', 'unknown', 'superseded'].find(status => counts[status]) || 'success'
})

const aggIcon = computed(() => {
  switch (aggState.value) {
    case 'error': return 'close-circle'
    case 'timeout': return 'time'
    case 'cancelled':
    case 'superseded': return 'minus-circle'
    case 'unknown': return 'help-circle'
    case 'success': return 'check-circle'
    default: return 'check-circle'
  }
})

const aggMeta = computed(() => {
  const counts = statusCounts.value
  const labels = { running: '执行中', success: '完成', error: '失败', timeout: '超时', cancelled: '已停止', superseded: '已取代', unknown: '状态待确认' }
  return Object.entries(labels).filter(([status]) => counts[status]).map(([status, label]) => `${counts[status]} ${label}`).join(' · ')
})

const expanded = ref(true)
const userToggled = ref(false)
watch(
  () => aggState.value,
  (state) => {
    // 失败和超时留下详情，成功记录自动收起；用户手动选择始终优先。
    if (!userToggled.value) expanded.value = ['running', 'error', 'timeout', 'unknown'].includes(state)
  },
  { immediate: true }
)

function toggle() {
  userToggled.value = true
  expanded.value = !expanded.value
}

</script>

<style scoped>
.tool-call-group {
  position: relative;
  border: none;
  border-radius: 0;
  margin: 0;
  overflow: visible;
  background: transparent;
  box-shadow: none;
  padding: 1px 0 1px 26px;
}
.tool-call-group::before {
  content: "";
  position: absolute;
  left: 8px;
  top: 0;
  bottom: 0;
  width: 1px;
  background: rgba(60, 60, 67, 0.16);
}
.tcg-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  min-height: 32px;
  padding: 3px 2px;
  cursor: pointer;
  user-select: none;
  background: transparent;
  transition: color var(--bp-duration) var(--bp-ease-out);
}
.tcg-head:focus-visible {
  outline: 2px solid var(--bp-accent);
  outline-offset: 2px;
  border-radius: 6px;
}
@media (hover: hover) and (pointer: fine) {
  .tcg-head:hover { background: transparent; }
  .tcg-head:hover .tcg-title { color: var(--bp-label); }
}
.tcg-icon {
  position: absolute;
  left: 0;
  top: 5px;
  width: 17px;
  height: 17px;
  border-radius: 50%;
  background: var(--bp-chat-bg);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-size: 13px;
  z-index: 1;
  color: var(--bp-label-tertiary);
}
.tcg-title {
  font-weight: 500;
  color: var(--bp-label-secondary);
  letter-spacing: 0;
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
  line-height: 1.5;
}
.tcg-chevron {
  color: var(--bp-label-tertiary);
  pointer-events: none;
}

.tcg-running .tcg-icon { color: var(--bp-accent); }
.tcg-success .tcg-icon { color: var(--bp-label-tertiary); }
.tcg-error .tcg-icon { color: var(--bp-danger); }
.tcg-timeout .tcg-icon,
.tcg-cancelled .tcg-icon,
.tcg-unknown .tcg-icon { color: var(--bp-warning); }
.tcg-superseded .tcg-icon { color: var(--bp-label-tertiary); }
.tcg-error .tcg-meta { color: var(--bp-danger); }
.tcg-timeout .tcg-meta { color: var(--bp-warning); }

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
  padding: 0 0 4px 4px;
  border-top: none;
}
.tcg-body .tc-group-item { margin: 0; }
.tcg-body .tc-group-item :deep(.tool-call) {
  margin: 0;
  padding-left: 0;
  border-left: none;
  border-radius: 0;
}
.tcg-body .tc-group-item :deep(.tool-call)::before {
  display: none;
}
.tcg-body .tc-group-item :deep(.tc-icon) {
  position: static;
  width: auto;
  height: auto;
  background: transparent;
  border-radius: 0;
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
  .tc-spinner { animation: none !important; opacity: 0.7; }
}
@media (prefers-contrast: more) {
  .tool-call-group::before { background: var(--bp-label-secondary); }
}
</style>
