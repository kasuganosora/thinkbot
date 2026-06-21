<script setup lang="ts">
/**
 * TButton — Flat button with variants.
 * Design rules (Memoh):
 *   - default = black bg (foreground), white text. Zero shadow.
 *   - outline/secondary = accent bg + 1px border.
 *   - ghost = transparent, hover accent.
 *   - destructive = red bg.
 *   - No purple on generic buttons.
 */
import { computed } from 'vue'

type Variant = 'default' | 'outline' | 'ghost' | 'destructive' | 'secondary'
type Size = 'default' | 'sm' | 'lg' | 'icon' | 'icon-sm'

const props = withDefaults(defineProps<{
  variant?: Variant
  size?: Size
  disabled?: boolean
  loading?: boolean
  type?: 'button' | 'submit' | 'reset'
}>(), {
  variant: 'default',
  size: 'default',
  type: 'button',
})

const variantClass = computed(() => `btn-${props.variant}`)
const sizeClass = computed(() => `btn-size-${props.size}`)
</script>

<template>
  <button
    :type="type"
    :disabled="disabled || loading"
    :class="['tbtn', variantClass, sizeClass]"
  >
    <svg v-if="loading" class="btn-spinner" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="3">
      <path d="M21 12a9 9 0 1 1-6.219-8.56" stroke-linecap="round" />
    </svg>
    <slot />
  </button>
</template>

<style scoped>
.tbtn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 0.5rem;
  white-space: nowrap;
  font-size: 0.8125rem;
  font-weight: 500;
  border-radius: var(--radius-md);
  transition: all 0.15s ease;
  outline: none;
  flex-shrink: 0;
  border: 1px solid transparent;
}
.tbtn:focus-visible {
  box-shadow: 0 0 0 2px oklch(0.575 0 0 / 0.3);
}
.tbtn:disabled {
  pointer-events: none;
  opacity: 0.5;
}

/* Variants */
.btn-default {
  background: var(--foreground);
  color: var(--background);
}
.btn-default:hover:not(:disabled) {
  opacity: 0.9;
}

.btn-outline {
  background: var(--background);
  border-color: var(--border);
  color: var(--foreground);
}
.btn-outline:hover:not(:disabled) {
  background: var(--accent);
}

.btn-secondary {
  background: var(--accent);
  border-color: var(--border);
  color: var(--foreground);
}
.btn-secondary:hover:not(:disabled) {
  opacity: 0.8;
}

.btn-ghost {
  background: transparent;
  color: var(--foreground);
}
.btn-ghost:hover:not(:disabled) {
  background: var(--accent);
}

.btn-destructive {
  background: var(--destructive);
  color: var(--destructive-foreground);
}
.btn-destructive:hover:not(:disabled) {
  opacity: 0.9;
}

/* Sizes */
.btn-size-default { height: 2.25rem; padding: 0 1rem; }
.btn-size-sm { height: 2rem; padding: 0 0.75rem; font-size: 0.75rem; gap: 0.375rem; }
.btn-size-lg { height: 2.5rem; padding: 0 1.5rem; }
.btn-size-icon { width: 2.25rem; height: 2.25rem; padding: 0; }
.btn-size-icon-sm { width: 2rem; height: 2rem; padding: 0; }

.btn-spinner {
  animation: spin 0.6s linear infinite;
}

@keyframes spin { to { transform: rotate(360deg); } }
</style>
