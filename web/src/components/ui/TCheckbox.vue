<script setup lang="ts">
import { useVModel } from '@vueuse/core'
import { Check } from 'lucide-vue-next'

const props = defineProps<{
  modelValue?: boolean
  disabled?: boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', v: boolean): void
}>()

const model = useVModel(props, 'modelValue', emit, { passive: true, defaultValue: false })
</script>

<template>
  <label class="tcheckbox" :class="{ disabled }">
    <input type="checkbox" v-model="model" :disabled="disabled" class="tcheckbox-input" />
    <span class="tcheckbox-box">
      <Check v-if="model" :size="12" stroke-width="3" />
    </span>
    <span v-if="$slots.default" class="tcheckbox-label">
      <slot />
    </span>
  </label>
</template>

<style scoped>
.tcheckbox {
  display: inline-flex;
  align-items: center;
  gap: 0.5rem;
  cursor: pointer;
  font-size: 0.8125rem;
  user-select: none;
}
.tcheckbox.disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.tcheckbox-input {
  position: absolute;
  opacity: 0;
  pointer-events: none;
}
.tcheckbox-box {
  width: 16px;
  height: 16px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--background);
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--background);
  transition: all 0.15s ease;
  flex-shrink: 0;
}
.tcheckbox-input:checked + .tcheckbox-box {
  background: var(--foreground);
  border-color: var(--foreground);
}
.tcheckbox-input:focus-visible + .tcheckbox-box {
  box-shadow: 0 0 0 2px oklch(0.575 0 0 / 0.2);
}
.tcheckbox-label {
  color: var(--foreground);
}
</style>
