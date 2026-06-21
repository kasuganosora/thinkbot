<script setup lang="ts">
import { ref, nextTick } from 'vue'

const props = defineProps<{ disabled?: boolean; sending?: boolean }>()
const emit = defineEmits<{ send: [text: string]; stop: [] }>()

const text = ref('')
const textareaRef = ref<HTMLTextAreaElement | null>(null)

function autoResize() {
  const el = textareaRef.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = Math.min(el.scrollHeight, 200) + 'px'
}

function handleKeydown(e: KeyboardEvent) {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit() }
}

function submit() {
  const trimmed = text.value.trim()
  if (!trimmed || props.disabled || props.sending) return
  emit('send', trimmed)
  text.value = ''
  nextTick(autoResize)
}
</script>

<template>
  <div class="input-bar">
    <div class="input-wrap">
      <textarea
        ref="textareaRef" v-model="text" :disabled="disabled" rows="1"
        :placeholder="disabled ? '请先选择一个机器人…' : '输入消息，Enter 发送，Shift+Enter 换行'"
        @keydown="handleKeydown" @input="autoResize"
      />
      <button v-if="sending" class="send-btn stop" title="停止生成" @click="emit('stop')">
        <svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor"><rect x="6" y="6" width="12" height="12" rx="2" /></svg>
      </button>
      <button v-else class="send-btn" :disabled="!text.trim() || disabled" title="发送" @click="submit">
        <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2">
          <line x1="22" y1="2" x2="11" y2="13" /><polygon points="22 2 15 22 11 13 2 9 22 2" fill="currentColor" stroke="none" />
        </svg>
      </button>
    </div>
  </div>
</template>

<style scoped>
.input-bar { padding: 0.75rem 1.5rem 1.25rem; border-top: 1px solid var(--border); background: var(--card); }
.input-wrap { display: flex; align-items: flex-end; gap: 0.5rem; max-width: 780px; margin: 0 auto; background: var(--background); border: 1px solid var(--border); border-radius: var(--radius-lg); padding: 0.5rem 0.5rem 0.5rem 0.9375rem; transition: border-color 0.15s ease, box-shadow 0.15s ease; }
.input-wrap:focus-within { border-color: var(--ring); box-shadow: 0 0 0 2px oklch(0.575 0 0 / 0.15); }
textarea { flex: 1; resize: none; border: none; background: none; color: var(--foreground); font-size: 0.9375rem; line-height: 1.5; max-height: 200px; padding: 0.25rem 0; font-family: inherit; }
textarea:focus { outline: none; }
textarea::placeholder { color: var(--muted-foreground); }
textarea:disabled { cursor: not-allowed; opacity: 0.5; }
.send-btn { flex-shrink: 0; width: 36px; height: 36px; border-radius: var(--radius-md); background: var(--foreground); color: var(--background); display: flex; align-items: center; justify-content: center; transition: opacity 0.15s ease; }
.send-btn:hover:not(:disabled) { opacity: 0.85; }
.send-btn:disabled { opacity: 0.4; cursor: not-allowed; }
.send-btn.stop { background: var(--destructive); color: var(--destructive-foreground); }
.send-btn.stop:hover { opacity: 0.85; }
</style>
