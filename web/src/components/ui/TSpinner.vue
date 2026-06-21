<script setup lang="ts">
withDefaults(defineProps<{
  size?: 'sm' | 'default' | 'lg'
}>(), {
  size: 'default',
})
</script>

<template>
  <div class="tspinner" :class="`spinner-${size}`" />
</template>

<style scoped>
.tspinner {
  border-radius: 50%;
  border: 2px solid var(--border);
  border-top-color: var(--muted-foreground);
  animation: spin 0.6s linear infinite;
}
.spinner-sm { width: 16px; height: 16px; border-width: 2px; }
.spinner-default { width: 24px; height: 24px; border-width: 2.5px; }
.spinner-lg { width: 36px; height: 36px; border-width: 3px; }

@keyframes spin { to { transform: rotate(360deg); } }
</style>
