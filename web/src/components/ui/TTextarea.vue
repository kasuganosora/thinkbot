<script setup lang="ts">
import { useVModel } from '@vueuse/core'

const props = defineProps<{
  modelValue?: string
  placeholder?: string
  rows?: number
  disabled?: boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', v: string): void
}>()

const model = useVModel(props, 'modelValue', emit, { passive: true })
</script>

<template>
  <textarea
    v-model="model"
    :placeholder="placeholder"
    :rows="rows || 4"
    :disabled="disabled"
    class="ttextarea"
  />
</template>

<style scoped>
.ttextarea {
  width: 100%;
  padding: 0.5rem 0.75rem;
  font-size: 0.8125rem;
  font-family: inherit;
  background: var(--background);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  color: var(--foreground);
  outline: none;
  resize: vertical;
  min-height: 80px;
  line-height: 1.5;
  transition: border-color 0.15s ease, box-shadow 0.15s ease;
}
.ttextarea::placeholder {
  color: var(--muted-foreground);
}
.ttextarea:focus {
  border-color: var(--ring);
  box-shadow: 0 0 0 2px oklch(0.575 0 0 / 0.15);
}
.ttextarea:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
</style>
