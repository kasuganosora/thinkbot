<script setup lang="ts">
/**
 * TDialog — Modal dialog with overlay, escape-to-close, click-outside.
 * Uses shadow-2xl for floating (bimodal elevation principle).
 */
import { onMounted, onUnmounted, watch } from 'vue'

const props = withDefaults(defineProps<{
  modelValue: boolean
  title?: string
  width?: string
  closeOnOverlay?: boolean
}>(), {
  title: '',
  width: '480px',
  closeOnOverlay: true,
})

const emit = defineEmits<{
  (e: 'update:modelValue', v: boolean): void
}>()

function close() {
  emit('update:modelValue', false)
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape' && props.modelValue) close()
}

onMounted(() => document.addEventListener('keydown', onKey))
onUnmounted(() => document.removeEventListener('keydown', onKey))

watch(() => props.modelValue, (v) => {
  document.body.style.overflow = v ? 'hidden' : ''
})
</script>

<template>
  <Teleport to="body">
    <Transition name="dialog">
      <div v-if="modelValue" class="dialog-overlay" @click.self="closeOnOverlay && close()">
        <div class="dialog-content" :style="{ maxWidth: width }">
          <div v-if="title || $slots.header" class="dialog-header">
            <slot name="header">
              <h2 class="dialog-title">{{ title }}</h2>
            </slot>
          </div>
          <button class="dialog-close" @click="close" aria-label="关闭">
            <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
              <path d="M18 6 6 18M6 6l12 12" />
            </svg>
          </button>
          <div class="dialog-body">
            <slot />
          </div>
          <div v-if="$slots.footer" class="dialog-footer">
            <slot name="footer" />
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.dialog-overlay {
  position: fixed;
  inset: 0;
  background: oklch(0 0 0 / 0.4);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
  padding: 1rem;
}

.dialog-content {
  position: relative;
  width: 100%;
  max-height: calc(100vh - 2rem);
  overflow-y: auto;
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: var(--radius-xl);
  box-shadow: var(--shadow-2xl);
  display: flex;
  flex-direction: column;
}

.dialog-header {
  padding: 1.25rem 1.5rem 0;
}

.dialog-title {
  font-size: 1rem;
  font-weight: 600;
  letter-spacing: -0.01em;
}

.dialog-close {
  position: absolute;
  top: 1rem;
  right: 1rem;
  width: 28px;
  height: 28px;
  border-radius: var(--radius-sm);
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--muted-foreground);
  transition: all 0.15s ease;
}
.dialog-close:hover {
  background: var(--accent);
  color: var(--foreground);
}

.dialog-body {
  padding: 1.25rem 1.5rem;
  flex: 1;
}

.dialog-footer {
  padding: 0 1.5rem 1.25rem;
  display: flex;
  justify-content: flex-end;
  gap: 0.5rem;
}

/* Transition */
.dialog-enter-active .dialog-content,
.dialog-leave-active .dialog-content {
  transition: all 0.2s ease;
}
.dialog-enter-active,
.dialog-leave-active {
  transition: opacity 0.2s ease;
}
.dialog-enter-from,
.dialog-leave-to {
  opacity: 0;
}
.dialog-enter-from .dialog-content,
.dialog-leave-to .dialog-content {
  transform: scale(0.96) translateY(8px);
}
</style>
