<script setup lang="ts">
/**
 * TCard — Container with 1px border, zero shadow (flat).
 * The exception to the flat rule: focal cards can use shadow-lg via `elevated` prop.
 */
withDefaults(defineProps<{
  elevated?: boolean
  padding?: 'none' | 'sm' | 'default' | 'lg'
}>(), {
  elevated: false,
  padding: 'default',
})
</script>

<template>
  <div
    class="tcard"
    :class="[
      elevated ? 'tcard-elevated' : '',
      `tcard-pad-${padding}`,
    ]"
  >
    <slot />
  </div>
</template>

<style scoped>
.tcard {
  background: var(--card);
  color: var(--card-foreground);
  border: 1px solid var(--border);
  border-radius: var(--radius-lg);
}
.tcard-elevated {
  box-shadow: var(--shadow-lg);
}
.tcard-pad-none { padding: 0; }
.tcard-pad-sm { padding: 0.875rem; }
.tcard-pad-default { padding: 1.25rem; }
.tcard-pad-lg { padding: 1.75rem; }
</style>
