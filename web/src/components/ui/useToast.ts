import { ref, readonly } from 'vue'

export type ToastType = 'success' | 'error' | 'info' | 'warning'

export interface Toast {
  id: number
  type: ToastType
  message: string
}

const toasts = ref<Toast[]>([])
let nextId = 0

function show(type: ToastType, message: string, duration = 3000) {
  const id = ++nextId
  toasts.value.push({ id, type, message })
  setTimeout(() => {
    remove(id)
  }, duration)
}

function remove(id: number) {
  const idx = toasts.value.findIndex((t) => t.id === id)
  if (idx !== -1) toasts.value.splice(idx, 1)
}

export function useToast() {
  return {
    toasts: readonly(toasts),
    remove,
    success: (msg: string) => show('success', msg),
    error: (msg: string) => show('error', msg, 5000),
    info: (msg: string) => show('info', msg),
    warning: (msg: string) => show('warning', msg, 4000),
  }
}
