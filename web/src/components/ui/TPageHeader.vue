<script setup lang="ts">
defineProps<{
  title: string
  subtitle?: string
}>()
</script>

<template>
  <div class="tpage-header">
    <div class="tpage-header-left">
      <slot name="back" />
      <div>
        <h1 class="tpage-title">{{ title }}</h1>
        <p v-if="subtitle" class="tpage-subtitle">{{ subtitle }}</p>
      </div>
    </div>
    <div class="tpage-header-actions">
      <slot name="actions" />
    </div>
  </div>
</template>

<style scoped>
.tpage-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 1rem;
  padding: 1rem 1.5rem;
  border-bottom: 1px solid var(--border);
  background: var(--background);
  position: sticky;
  top: 0;
  z-index: 10;
}
.tpage-header-left {
  display: flex;
  align-items: center;
  gap: 0.75rem;
  min-width: 0;
}
.tpage-title {
  font-size: 1.0625rem;
  font-weight: 600;
  letter-spacing: -0.02em;
}
.tpage-subtitle {
  font-size: 0.75rem;
  color: var(--muted-foreground);
  margin-top: 0.125rem;
}
.tpage-header-actions {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  flex-shrink: 0;
}
</style>
