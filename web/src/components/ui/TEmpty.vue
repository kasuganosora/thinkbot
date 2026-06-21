<script setup lang="ts">
withDefaults(defineProps<{
  text?: string
}>(), {
  text: '暂无数据',
})
</script>

<template>
  <div class="tempty">
    <svg viewBox="0 0 24 24" width="32" height="32" fill="none" stroke="currentColor" stroke-width="1.5" class="tempty-icon">
      <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" stroke-linecap="round" stroke-linejoin="round" />
    </svg>
    <p class="tempty-text">{{ text }}</p>
    <slot />
  </div>
</template>

<style scoped>
.tempty {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 4rem 1.5rem;
  gap: 0.75rem;
}
.tempty-icon {
  color: var(--border);
}
.tempty-text {
  font-size: 0.875rem;
  color: var(--muted-foreground);
}
</style>
