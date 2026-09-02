<template>
  <div
    ref="rootRef"
    id="chat-command-palette"
    class="cmd-palette"
    data-testid="chat-command-palette"
    role="listbox"
    aria-label="斜杠命令"
    :aria-activedescendant="activeOptionId"
    @mousedown.prevent
  >
    <template v-if="flatItems.length">
      <template v-for="section in sections" :key="section.category">
        <div class="cmd-heading" aria-hidden="true">{{ section.category }}</div>
        <div
          v-for="row in section.rows"
          :key="row.item.id"
          :id="'cmd-opt-' + row.item.id"
          class="cmd-row"
          :class="{ active: row.flatIndex === activeIndex }"
          :data-testid="'chat-command-option-' + row.item.name"
          :data-index="row.flatIndex"
          role="option"
          :aria-selected="row.flatIndex === activeIndex"
          @mousemove="onHover(row.flatIndex)"
          @click="$emit('select', row.item)"
        >
          <span class="cmd-icon" aria-hidden="true">
            <t-icon :name="row.item.icon" />
          </span>
          <span class="cmd-text">
            <span class="cmd-name">/{{ row.item.name }}</span>
            <span v-if="row.item.description" class="cmd-desc">{{ row.item.description }}</span>
          </span>
          <span class="cmd-cat">{{ row.item.category }}</span>
        </div>
      </template>
    </template>
    <div v-else class="cmd-empty" data-testid="chat-command-empty">无匹配命令</div>
  </div>
</template>

<script setup>
import { computed, nextTick, ref, watch } from 'vue'

const props = defineProps({
  query: { type: String, default: '' },
  items: { type: Array, default: () => [] },
  activeIndex: { type: Number, default: 0 },
})

const emit = defineEmits(['select', 'close', 'highlight'])

const rootRef = ref(null)

const CATEGORY_ORDER = ['命令', '技能']

const flatItems = computed(() => props.items || [])

const sections = computed(() => {
  const groups = new Map()
  flatItems.value.forEach((item, flatIndex) => {
    const cat = item.category || '命令'
    if (!groups.has(cat)) groups.set(cat, [])
    groups.get(cat).push({ item, flatIndex })
  })
  const out = []
  for (const cat of CATEGORY_ORDER) {
    const rows = groups.get(cat)
    if (rows && rows.length) out.push({ category: cat, rows })
  }
  for (const [cat, rows] of groups) {
    if (!CATEGORY_ORDER.includes(cat) && rows.length) out.push({ category: cat, rows })
  }
  return out
})

const activeOptionId = computed(() => {
  const item = flatItems.value[props.activeIndex]
  return item ? 'cmd-opt-' + item.id : undefined
})

function onHover(index) {
  if (index !== props.activeIndex) emit('highlight', index)
}

watch(
  () => props.activeIndex,
  (i) => {
    nextTick(() => {
      const el = rootRef.value?.querySelector(`[data-index="${i}"]`)
      if (el && typeof el.scrollIntoView === 'function') {
        el.scrollIntoView({ block: 'nearest' })
      }
    })
  }
)
</script>

<style scoped>
.cmd-palette {
  position: absolute;
  left: 0;
  right: 0;
  bottom: calc(100% + 8px);
  z-index: 20;
  max-height: 320px;
  overflow-y: auto;
  padding: 6px;
  background: var(--bp-surface);
  border-radius: var(--bp-radius-lg);
  box-shadow: var(--bp-shadow-md);
  border: var(--bp-hairline);
  transform-origin: 50% 100%;
  opacity: 1;
  transform: translateY(0);
  transition: opacity 140ms var(--bp-ease-out),
              transform 140ms var(--bp-ease-out);
  scrollbar-width: thin;
}
@starting-style {
  .cmd-palette {
    opacity: 0;
    transform: translateY(6px) scale(0.98);
  }
}
.cmd-heading {
  padding: 8px 10px 4px;
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.04em;
  color: var(--bp-label-tertiary);
}
.cmd-row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 10px;
  border-radius: var(--bp-radius-md);
  cursor: pointer;
  min-height: 44px;
}
.cmd-row.active {
  background: var(--bp-accent-soft);
}
.cmd-icon {
  flex-shrink: 0;
  width: 28px;
  height: 28px;
  border-radius: var(--bp-radius-sm);
  background: var(--bp-surface-fill);
  color: var(--bp-label-secondary);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-size: 16px;
}
.cmd-row.active .cmd-icon {
  background: var(--bp-accent-soft-strong);
  color: var(--bp-accent);
}
.cmd-text {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 1px;
}
.cmd-name {
  font-size: 13px;
  font-weight: 600;
  letter-spacing: var(--bp-tracking-body);
  color: var(--bp-label);
  line-height: 1.3;
}
.cmd-desc {
  font-size: 12px;
  color: var(--bp-label-secondary);
  line-height: 1.3;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.cmd-cat {
  flex-shrink: 0;
  font-size: 11px;
  letter-spacing: var(--bp-tracking-caption);
  color: var(--bp-label-tertiary);
  padding-left: 8px;
}
.cmd-empty {
  padding: 18px 12px;
  text-align: center;
  font-size: 13px;
  color: var(--bp-label-tertiary);
}
@media (hover: hover) and (pointer: fine) {
  .cmd-row:hover:not(.active) {
    background: var(--bp-surface-fill-hover);
  }
}
@media (prefers-reduced-motion: reduce) {
  .cmd-palette {
    transition: opacity 120ms ease;
    transform: none;
  }
  @starting-style {
    .cmd-palette {
      opacity: 0;
      transform: none;
    }
  }
}
</style>
