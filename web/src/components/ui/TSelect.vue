<script setup lang="ts">
import { useVModel } from '@vueuse/core'
import { ChevronDown } from 'lucide-vue-next'

const props = defineProps<{
  modelValue?: string
  placeholder?: string
  disabled?: boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', v: string): void
}>()

const model = useVModel(props, 'modelValue', emit, { passive: true })
</script>

<template>
  <div class="tselect-wrap">
    <select v-model="model" :disabled="disabled" class="tselect">
      <option v-if="placeholder" value="" disabled>{{ placeholder }}</option>
      <slot />
    </select>
    <ChevronDown class="tselect-icon" :size="14" />
  </div>
</template>

<style scoped>
.tselect-wrap {
  position: relative;
  width: 100%;
}

.tselect {
  width: 100%;
  height: 2.25rem;
  padding: 0 2rem 0 0.75rem;
  font-size: 0.8125rem;
  font-family: inherit;
  background: var(--background);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  color: var(--foreground);
  outline: none;
  appearance: none;
  cursor: pointer;
  transition: border-color 0.15s ease, box-shadow 0.15s ease;
}
.tselect:focus {
  border-color: var(--ring);
  box-shadow: 0 0 0 2px oklch(0.575 0 0 / 0.15);
}
.tselect:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.tselect-icon {
  position: absolute;
  right: 0.625rem;
  top: 50%;
  transform: translateY(-50%);
  color: var(--muted-foreground);
  pointer-events: none;
}
</style>
