<script setup lang="ts">
defineProps<{
  tabs: { id: string; label: string }[]
  modelValue: string
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', v: string): void
}>()
</script>

<template>
  <div class="ttab-bar">
    <button
      v-for="tab in tabs"
      :key="tab.id"
      class="ttab-btn"
      :class="{ active: modelValue === tab.id }"
      @click="emit('update:modelValue', tab.id)"
    >
      {{ tab.label }}
    </button>
  </div>
</template>

<style scoped>
.ttab-bar {
  display: flex;
  gap: 0.125rem;
  padding: 0 1.5rem;
  border-bottom: 1px solid var(--border);
  overflow-x: auto;
  scrollbar-width: none;
}
.ttab-bar::-webkit-scrollbar { display: none; }

.ttab-btn {
  padding: 0.625rem 0.875rem;
  font-size: 0.8125rem;
  font-weight: 500;
  color: var(--muted-foreground);
  border-bottom: 2px solid transparent;
  transition: all 0.15s ease;
  white-space: nowrap;
  margin-bottom: -1px;
}
.ttab-btn:hover {
  color: var(--foreground);
}
.ttab-btn.active {
  color: var(--foreground);
  border-bottom-color: var(--foreground);
}
</style>
