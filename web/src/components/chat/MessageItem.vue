<script setup lang="ts">
import { computed } from 'vue'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import type { ChatMessage } from '@/types/api'

const props = defineProps<{ message: ChatMessage; streaming?: boolean }>()

marked.setOptions({ breaks: true, gfm: true })

const isUser = computed(() => props.message.role === 'user')
const htmlContent = computed(() => {
  const raw = marked.parse(props.message.content, { async: false }) as string
  return DOMPurify.sanitize(raw)
})
</script>

<template>
  <div class="message" :class="{ user: isUser }">
    <div class="avatar" v-if="!isUser">
      <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2">
        <rect x="3" y="11" width="18" height="10" rx="2" /><circle cx="12" cy="5" r="2" /><path d="M12 7v4" />
      </svg>
    </div>
    <div class="bubble" :class="{ user: isUser }">
      <div v-if="isUser" class="text">{{ message.content }}</div>
      <div v-else class="markdown" v-html="htmlContent"></div>
      <span v-if="streaming" class="cursor" />
    </div>
  </div>
</template>

<style scoped>
.message { display: flex; gap: 0.75rem; max-width: 780px; width: 100%; animation: fade-in 0.2s ease-out; }
.message.user { flex-direction: row-reverse; }
.avatar { flex-shrink: 0; width: 32px; height: 32px; border-radius: var(--radius-sm); background: var(--accent); color: var(--muted-foreground); display: flex; align-items: center; justify-content: center; margin-top: 2px; }
.bubble { padding: 0.625rem 0.9375rem; border-radius: var(--radius-md); background: var(--card); border: 1px solid var(--border); font-size: 0.9375rem; line-height: 1.6; word-break: break-word; }
.bubble.user { background: var(--foreground); color: var(--background); border-color: transparent; border-bottom-right-radius: var(--radius-sm); }
.bubble:not(.user) { border-bottom-left-radius: var(--radius-sm); }
.text { white-space: pre-wrap; }
.cursor { display: inline-block; width: 2px; height: 1em; background: currentColor; margin-left: 2px; vertical-align: text-bottom; animation: pulse-cursor 0.8s ease-in-out infinite; }
.markdown :deep(p) { margin-bottom: 0.5rem; }
.markdown :deep(p:last-child) { margin-bottom: 0; }
.markdown :deep(pre) { background: var(--background); border: 1px solid var(--border); border-radius: var(--radius-sm); padding: 0.75rem; overflow-x: auto; margin: 0.5rem 0; }
.markdown :deep(code) { font-family: var(--font-mono); font-size: 0.8125rem; }
.markdown :deep(pre code) { background: none; padding: 0; }
.markdown :deep(:not(pre) > code) { background: var(--accent); padding: 0.125rem 0.375rem; border-radius: 4px; }
.markdown :deep(ul), .markdown :deep(ol) { padding-left: 1.25rem; margin-bottom: 0.5rem; }
.markdown :deep(a) { color: var(--foreground); text-decoration: underline; }
.markdown :deep(blockquote) { border-left: 3px solid var(--border); padding-left: 0.75rem; margin: 0.5rem 0; color: var(--muted-foreground); }
.markdown :deep(table) { border-collapse: collapse; width: 100%; margin: 0.5rem 0; }
.markdown :deep(th), .markdown :deep(td) { border: 1px solid var(--border); padding: 0.375rem 0.625rem; text-align: left; font-size: 0.875rem; }
</style>
