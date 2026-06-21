<script setup lang="ts">
import { nextTick, onMounted, ref, watch } from 'vue'
import { useChat } from '@/composables/useChat'
import MessageItem from '@/components/chat/MessageItem.vue'
import ChatInput from '@/components/chat/ChatInput.vue'
import TypingIndicator from '@/components/chat/TypingIndicator.vue'

const {
  bots, activeBotId, messages, sending, streamingText, status, toolCalls,
  loadingHistory, nextCursor, selectBot, loadHistory, sendMessage, stopStreaming,
} = useChat()

const scrollContainer = ref<HTMLElement | null>(null)
const activeBot = () => bots.value.find((b) => b.id === activeBotId.value)

function scrollToBottom() {
  if (scrollContainer.value) scrollContainer.value.scrollTop = scrollContainer.value.scrollHeight
}

function handleScroll() {
  const el = scrollContainer.value
  if (!el || !nextCursor.value || loadingHistory.value) return
  if (el.scrollTop < 60) {
    const prevHeight = el.scrollHeight
    loadHistory().then(() =>
      nextTick(() => {
        if (scrollContainer.value)
          scrollContainer.value.scrollTop = scrollContainer.value.scrollHeight - prevHeight
      }),
    )
  }
}

watch(() => messages.value.length + streamingText.value, () => nextTick(scrollToBottom))
onMounted(scrollToBottom)
</script>

<template>
  <div class="chat-view">
    <div class="chat-layout">
      <!-- Session 侧边栏：选择 Bot -->
      <div class="session-sidebar">
        <p class="sidebar-label">会话</p>
        <button
          v-for="bot in bots"
          :key="bot.id"
          class="session-item"
          :class="{ active: bot.id === activeBotId }"
          @click="selectBot(bot.id)"
        >
          <div class="session-avatar">{{ bot.name.charAt(0).toUpperCase() }}</div>
          <div class="session-text">
            <p class="session-name">{{ bot.name }}</p>
            <p class="session-sub">{{ bot.running ? '在线' : '离线' }}</p>
          </div>
        </button>
        <p v-if="bots.length === 0" class="empty-hint">暂无可用机器人</p>
      </div>

      <!-- 聊天主区域 -->
      <div class="chat-main">
        <header class="chat-header">
          <h2>{{ activeBot()?.name ?? 'ThinkBot' }}</h2>
          <span v-if="activeBot()?.running" class="status-badge">在线</span>
        </header>

        <div ref="scrollContainer" class="messages" @scroll="handleScroll">
          <div v-if="loadingHistory" class="loading-more"><div class="mini-spinner" /> 加载历史…</div>

          <div v-if="messages.length === 0 && !loadingHistory" class="empty-state">
            <svg viewBox="0 0 24 24" width="48" height="48" fill="none" stroke="currentColor" stroke-width="1.5">
              <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
            </svg>
            <p>开始与 {{ activeBot()?.name ?? '机器人' }} 对话</p>
          </div>

          <MessageItem v-for="msg in messages" :key="msg.id" :message="msg" />

          <!-- 工具调用指示器 -->
          <div v-if="toolCalls.length" class="tool-calls">
            <div v-for="(tc, i) in toolCalls" :key="i" class="tool-call-item">
              <span class="tool-icon">
                <svg v-if="tc.status === 'calling'" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" class="spin"><path d="M21 12a9 9 0 1 1-6.219-8.56" /></svg>
                <svg v-else-if="tc.status === 'done'" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="20 6 9 17 4 12" /></svg>
                <svg v-else viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2.5"><line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" /></svg>
              </span>
              <span class="tool-label">{{ tc.tool }}</span>
              <span class="tool-status" :class="tc.status">
                {{ tc.status === 'calling' ? '执行中…' : tc.status === 'done' ? '完成' : '失败' }}
              </span>
            </div>
          </div>

          <MessageItem
            v-if="sending && streamingText"
            :message="{ id: -999, role: 'assistant', content: streamingText, botId: '', userId: '', traceId: '', createdAt: '' }"
            :streaming="true"
          />
          <TypingIndicator v-if="sending && !streamingText" />
          <div v-if="status === 'error'" class="error-banner">消息发送失败，请重试</div>
        </div>

        <ChatInput :disabled="!activeBotId" :sending="sending" @send="sendMessage" @stop="stopStreaming" />
      </div>
    </div>
  </div>
</template>

<style scoped>
.chat-view { flex: 1; display: flex; min-height: 0; }
.chat-layout { flex: 1; display: flex; min-height: 0; }

.session-sidebar {
  width: 220px; flex-shrink: 0;
  border-right: 1px solid var(--border);
  background: var(--card);
  overflow-y: auto;
  padding: 0.625rem;
}

.sidebar-label {
  padding: 0.375rem 0.625rem 0.5rem;
  font-size: 0.6875rem; font-weight: 600; text-transform: uppercase;
  letter-spacing: 0.05em; color: var(--muted-foreground);
}

.session-item {
  width: 100%; display: flex; align-items: center; gap: 0.625rem;
  padding: 0.5rem 0.625rem; border-radius: var(--radius-sm);
  transition: background 0.15s ease; text-align: left; cursor: pointer;
}
.session-item:hover { background: var(--accent); }
.session-item.active { background: var(--accent); }

.session-avatar {
  width: 32px; height: 32px; flex-shrink: 0; border-radius: var(--radius-sm);
  background: var(--card); border: 1px solid var(--border);
  display: flex; align-items: center; justify-content: center;
  font-size: 0.8125rem; font-weight: 600; color: var(--muted-foreground);
}
.session-item.active .session-avatar {
  background: var(--foreground); color: var(--background); border-color: transparent;
}

.session-text { min-width: 0; }
.session-name { font-size: 0.8125rem; font-weight: 500; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.session-sub { font-size: 0.6875rem; color: var(--muted-foreground); }

.empty-hint { padding: 1rem; font-size: 0.8125rem; color: var(--muted-foreground); text-align: center; }

.chat-main { flex: 1; display: flex; flex-direction: column; min-width: 0; }

.chat-header {
  display: flex; align-items: center; gap: 0.625rem;
  padding: 0.875rem 1.5rem; border-bottom: 1px solid var(--border);
  background: var(--card);
}
.chat-header h2 { font-size: 1rem; font-weight: 600; }

.status-badge {
  font-size: 0.6875rem; font-weight: 600; padding: 0.125rem 0.5rem;
  border-radius: 100px; background: var(--success); color: white;
}

.messages { flex: 1; overflow-y: auto; padding: 1.5rem; display: flex; flex-direction: column; gap: 0.5rem; }

.loading-more {
  display: flex; align-items: center; justify-content: center; gap: 0.5rem;
  padding: 0.75rem; font-size: 0.8125rem; color: var(--muted-foreground);
}
.mini-spinner {
  width: 14px; height: 14px; border: 2px solid var(--border);
  border-top-color: var(--muted-foreground); border-radius: 50%; animation: spin 0.6s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }

.empty-state {
  flex: 1; display: flex; flex-direction: column; align-items: center; justify-content: center;
  gap: 1rem; color: var(--muted-foreground); opacity: 0.4;
}
.empty-state p { font-size: 0.9375rem; }

.error-banner {
  align-self: center; padding: 0.5rem 1rem;
  background: var(--destructive); color: var(--destructive-foreground);
  border-radius: var(--radius-md); font-size: 0.8125rem;
}

.tool-calls { display: flex; flex-direction: column; gap: 0.25rem; max-width: 780px; }
.tool-call-item {
  display: flex; align-items: center; gap: 0.375rem;
  padding: 0.375rem 0.625rem; border-radius: var(--radius-sm);
  background: var(--accent); font-size: 0.75rem; color: var(--muted-foreground);
}
.tool-icon { display: flex; align-items: center; justify-content: center; }
.tool-label { font-family: var(--font-mono, monospace); font-weight: 600; color: var(--foreground); }
.tool-status.calling { color: var(--muted-foreground); }
.tool-status.done { color: var(--success, #22c55e); }
.tool-status.error { color: var(--destructive, #ef4444); }
.spin { animation: spin 0.8s linear infinite; }
</style>
