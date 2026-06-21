<script setup lang="ts">
/**
 * TInput — Flat input, 1px border, no shadow.
 * Focus uses dark ring (not brand color).
 */
import { useVModel } from '@vueuse/core'

const props = defineProps<{
  modelValue?: string | number
  defaultValue?: string | number
  type?: string
  placeholder?: string
  disabled?: boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', v: string | number): void
}>()

const model = useVModel(props, 'modelValue', emit, {
  passive: true,
  defaultValue: props.defaultValue as string | number,
})
</script>

<template>
  <input
    v-model="model"
    :type="type || 'text'"
    :placeholder="placeholder"
    :disabled="disabled"
    class="tinput"
  >
</template>

<style scoped>
.tinput {
  width: 100%;
  height: 2.25rem;
  padding: 0 0.75rem;
  font-size: 0.8125rem;
  font-family: inherit;
  background: var(--background);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  color: var(--foreground);
  outline: none;
  transition: border-color 0.15s ease, box-shadow 0.15s ease;
}
.tinput::placeholder {
  color: var(--muted-foreground);
}
.tinput:focus {
  border-color: var(--ring);
  box-shadow: 0 0 0 2px oklch(0.575 0 0 / 0.15);
}
.tinput:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
</style>
