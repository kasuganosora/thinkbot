<template>
  <div
    class="tool-call-group"
    :class="`tcg-${aggState}`"
    :data-testid="`chat-toolgroup-${name}`"
    :data-group-name="name"
    :data-group-count="calls.length"
  >
    <div class="tcg-head" :data-testid="`chat-toolgroup-head-${name}`" @click="toggle">
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
        @click.stop="toggle"
      />
    </div>

    <div v-show="expanded" class="tcg-body">
      <div v-for="c in calls" :key="c.id" class="tc-group-item">
        <ToolCallCard :call="c" />
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

const displayName = computed(() => toolLabels[props.name] || props.name)

const aggState = computed(() => {
  const calls = props.calls || []
  if (calls.some(c => c.status === 'running')) return 'running'
  if (calls.some(c => c.status === 'killed')) return 'killed'
  if (calls.some(c => c.status === 'error')) return 'error'
  // superseded 是被后续同名调用取代的 phantom call，视为已结束
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

// 运行中时默认展开，全部结束后自动收起；用户手动展开后尊重其选择。
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
</script>

<style scoped>
.tool-call-group {
  border: none;
  border-radius: 8px;
  margin: 4px 0;
  overflow: hidden;
  background: var(--bp-surface);
  box-shadow: var(--bp-shadow-sm);
}
.tcg-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  cursor: pointer;
  user-select: none;
  background: var(--bp-surface-fill-hover);
  transition: background var(--bp-duration) var(--bp-ease-out), transform var(--bp-duration) var(--bp-ease-out);
}
.tcg-head:active { transform: scale(var(--bp-press-scale)); }
.tcg-icon { display: inline-flex; }
.tcg-title { font-weight: 600; }
.tcg-count {
  color: var(--bp-label-tertiary);
  font-weight: 400;
  margin-left: 2px;
}
.tcg-meta {
  flex: 1;
  text-align: right;
  color: var(--bp-label-tertiary);
  font-size: 12px;
}
.tcg-chevron { color: var(--bp-label-tertiary); }

.tcg-running .tcg-icon { color: var(--bp-accent); }
.tcg-success .tcg-icon { color: var(--bp-success); }
.tcg-error .tcg-icon { color: var(--bp-danger); }
.tcg-killed .tcg-icon { color: var(--bp-warning); }

.tcg-body { padding: 4px 8px; }
.tcg-body .tc-group-item { margin: 4px 0 4px 6px; }
.tcg-body .tc-group-item :deep(.tool-call) {
  border-left: 2px solid var(--bp-separator);
  border-radius: 0 6px 6px 0;
}
.tcg-spinner {
  width: 14px;
  height: 14px;
  border: 2px solid var(--bp-accent);
  border-top-color: transparent;
  border-radius: 50%;
  display: inline-block;
  animation: tcg-spin 0.8s linear infinite;
}
@keyframes tcg-spin { to { transform: rotate(360deg); } }
</style>
