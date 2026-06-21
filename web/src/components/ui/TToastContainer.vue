<script setup lang="ts">
import { useToast } from './useToast'
import { CheckCircle2, XCircle, Info, AlertTriangle, X } from 'lucide-vue-next'

const { toasts, remove } = useToast()

const iconMap = {
  success: CheckCircle2,
  error: XCircle,
  info: Info,
  warning: AlertTriangle,
}

const colorMap = {
  success: 'var(--success)',
  error: 'var(--destructive)',
  info: 'var(--muted-foreground)',
  warning: 'var(--warning)',
}
</script>

<template>
  <Teleport to="body">
    <div class="toast-container">
      <Transition-group name="toast" tag="div">
        <div
          v-for="t in toasts"
          :key="t.id"
          class="toast-item"
          @click="remove(t.id)"
        >
          <component
            :is="iconMap[t.type]"
            :size="16"
            :style="{ color: colorMap[t.type] }"
          />
          <span class="toast-msg">{{ t.message }}</span>
        </div>
      </Transition-group>
    </div>
  </Teleport>
</template>

<style scoped>
.toast-container {
  position: fixed;
  bottom: 1.5rem;
  right: 1.5rem;
  z-index: 200;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  pointer-events: none;
}

.toast-item {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  padding: 0.625rem 0.875rem;
  box-shadow: var(--shadow-md);
  cursor: pointer;
  pointer-events: auto;
  min-width: 240px;
  max-width: 400px;
}

.toast-msg {
  font-size: 0.8125rem;
  color: var(--foreground);
}

.toast-enter-active,
.toast-leave-active {
  transition: all 0.3s ease;
}
.toast-enter-from {
  opacity: 0;
  transform: translateX(100%);
}
.toast-leave-to {
  opacity: 0;
  transform: translateX(100%);
}
</style>
