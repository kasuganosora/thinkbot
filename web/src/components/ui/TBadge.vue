<script setup lang="ts">
/**
 * TBadge — Flat badge, 4px radius.
 * default = black bg (foreground). secondary = accent. destructive = red.
 */
import { computed } from 'vue'

type Variant = 'default' | 'secondary' | 'destructive' | 'outline' | 'warning' | 'success'

const props = withDefaults(defineProps<{
  variant?: Variant
}>(), {
  variant: 'secondary',
})

const cls = computed(() => `badge-${props.variant}`)
</script>

<template>
  <span class="tbadge" :class="cls">
    <slot />
  </span>
</template>

<style scoped>
.tbadge {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
  font-size: 0.6875rem;
  font-weight: 500;
  padding: 0.0625rem 0.5rem;
  border-radius: var(--radius-sm);
  white-space: nowrap;
  line-height: 1.5;
  border: 1px solid transparent;
}

.badge-default {
  background: var(--foreground);
  color: var(--background);
}
.badge-secondary {
  background: var(--accent);
  color: var(--foreground);
}
.badge-outline {
  background: var(--background);
  border-color: var(--border);
  color: var(--foreground);
}
.badge-destructive {
  background: var(--destructive);
  color: var(--destructive-foreground);
}
.badge-warning {
  background: var(--warning);
  color: white;
}
.badge-success {
  background: var(--success);
  color: white;
}
</style>
