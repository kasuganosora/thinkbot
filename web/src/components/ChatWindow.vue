<template>
  <div
    class="chat-window"
    data-testid="chat-window"
    role="region"
    aria-label="会话主区域：展示当前会话消息并发送新消息"
  >
    <div class="chat-topbar" data-testid="chat-topbar">
      <div class="topbar-title" data-testid="chat-session-title">
        {{ store.activeSession?.title || store.activeBot?.name || '对话' }}
      </div>
      <t-tag v-if="store.activeBot" theme="success" variant="light" data-testid="chat-bot-model">
        {{ store.activeBot.model }}
      </t-tag>
    </div>

    <div
      ref="scrollRef"
      class="chat-body"
      data-testid="chat-message-list"
      role="log"
      aria-label="消息列表"
      aria-live="polite"
    >
      <SessionWorkflowPanel
        v-if="sessionWorkflowId"
        :session-id="store.activeSessionId"
        :workflow-id="sessionWorkflowId"
      />

      <template v-if="messages.length">
        <div
          v-for="msg in messages"
          :key="msg.id"
          class="msg-row"
          :class="msg.role"
          :data-testid="`chat-message-${msg.role}`"
          :data-role="msg.role"
        >
          <div class="msg-avatar">
            <div v-if="msg.role === 'user'" class="user-bubble-avatar" data-testid="chat-user-avatar">{{ userInitial }}</div>
            <div v-else class="bot-bubble-avatar">{{ store.activeBot?.avatar }}</div>
          </div>
          <div class="msg-content-wrap">
            <div class="msg-bubble" data-testid="chat-message-content">{{ msg.content }}</div>
            <div
              v-if="msg.role === 'assistant' && msg.toolCalls && msg.toolCalls.length"
              class="msg-toolcalls"
              data-testid="chat-message-toolcalls"
            >
              <ToolCallCard
                v-for="tc in msg.toolCalls"
                :key="tc.id"
                :call="tc"
              />
            </div>
          </div>
        </div>
      </template>

      <div v-else class="empty-greeting" data-testid="chat-empty-state">
        <div class="greet-avatar">{{ store.activeBot?.avatar || '🤖' }}</div>
        <h2 class="greet-title">Hi，今天想聊点什么？</h2>
        <p class="greet-sub">当前 Bot：{{ store.activeBot?.name || '未选择' }}</p>
        <div class="greet-chips" data-testid="chat-quick-chips">
          <div
            v-for="(chip, i) in chips"
            :key="i"
            class="greet-chip"
            :data-testid="`chat-quick-chip-${i}`"
            role="button"
            :aria-label="`快捷提问：${chip}`"
            @click="quickSend(chip)"
          >
            {{ chip }}
          </div>
        </div>
      </div>
    </div>

    <div class="chat-input-area" data-testid="chat-input-area">
      <div class="input-box">
        <t-textarea
          v-model="draft"
          :autosize="{ minRows: 1, maxRows: 6 }"
          placeholder="有问题，尽管问，Shift + Enter 换行"
          :bordered="false"
          data-testid="chat-input-textarea"
          aria-label="消息输入框，Enter 发送，Shift+Enter 换行"
          @keydown="onKeydown"
        />
        <div class="input-toolbar">
          <div class="tool-left">
            <t-button variant="text" size="small" shape="round" data-testid="chat-btn-deepthink">
              <template #icon><t-icon name="lightbulb" /></template>
              深度思考
            </t-button>
            <t-button variant="text" size="small" shape="round" data-testid="chat-btn-tools">
              <template #icon><t-icon name="tools" /></template>
              工具
            </t-button>
          </div>
          <t-button
            theme="primary"
            shape="circle"
            :disabled="!draft.trim()"
            data-testid="chat-btn-send"
            aria-label="发送消息"
            @click="send"
          >
            <template #icon><t-icon name="send" /></template>
          </t-button>
        </div>
      </div>
      <div class="input-foot">内容由 AI 生成，仅供参考</div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, nextTick, watch } from 'vue'
import { useBotStore } from '@/stores/bot'
import { useUserStore } from '@/stores/user'
import SessionWorkflowPanel from '@/components/SessionWorkflowPanel.vue'
import ToolCallCard from '@/components/ToolCallCard.vue'

const store = useBotStore()
const userStore = useUserStore()
const draft = ref('')
const scrollRef = ref()

const messages = computed(() => store.activeSession?.messages || [])

// 当前会话关联的工作流 id：
// 真实接入后应由后端返回（session 创建工作流时下发），这里 mock 演示：
// 优先取 session.workflowId，否则对带有「报告/调研/重构」等任务型标题的会话挂演示工作流 wf-1。
const sessionWorkflowId = computed(() => {
  const sess = store.activeSession
  if (!sess) return ''
  if (sess.workflowId) return sess.workflowId
  const title = sess.title || ''
  if (/报告|调研|重构|文案|整理|优化/.test(title)) return 'wf-1'
  return ''
})

const userInitial = computed(() => {
  const name = userStore.user?.nickname || userStore.user?.username || 'U'
  return String(name).trim().charAt(0).toUpperCase() || 'U'
})

const chips = [
  '帮我写一份周报',
  '解释一下什么是 RAG',
  '推荐三个高效学习方法',
  '帮我润色一段商务邮件'
]

function scrollToBottom() {
  nextTick(() => {
    if (scrollRef.value) scrollRef.value.scrollTop = scrollRef.value.scrollHeight
  })
}

watch(() => messages.value.length, scrollToBottom)
watch(() => store.activeSessionId, scrollToBottom)

function send() {
  const text = draft.value.trim()
  if (!text) return
  store.sendMessage(text)
  draft.value = ''
}

function quickSend(text) {
  store.sendMessage(text)
}

function onKeydown(value, { e }) {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    send()
  }
}
</script>

<style scoped>
.chat-window {
  flex: 1;
  height: 100%;
  display: flex;
  flex-direction: column;
  background: #fff;
  min-width: 0;
}
.chat-topbar {
  height: 56px;
  flex-shrink: 0;
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 0 24px;
  border-bottom: 1px solid #f0f0f0;
}
.topbar-title {
  font-size: 16px;
  font-weight: 600;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.chat-body {
  flex: 1;
  overflow-y: auto;
  padding: 20px 0;
}
.msg-row {
  display: flex;
  gap: 10px;
  padding: 8px 32px;
  max-width: 820px;
  margin: 0 auto;
  align-items: flex-start;
}
.msg-row.user {
  flex-direction: row-reverse;
}
.msg-avatar {
  flex-shrink: 0;
}
.bot-bubble-avatar {
  width: 36px;
  height: 36px;
  border-radius: 50%;
  background: #e6f4ef;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 20px;
}
.user-bubble-avatar {
  width: 36px;
  height: 36px;
  border-radius: 50%;
  background: #0052d9;
  color: #fff;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 15px;
  font-weight: 600;
}
.msg-bubble {
  padding: 10px 14px;
  border-radius: 12px;
  font-size: 14px;
  line-height: 1.7;
  white-space: pre-wrap;
  word-break: break-word;
  max-width: 72%;
}
.msg-content-wrap {
  min-width: 0;
  max-width: 72%;
  display: flex;
  flex-direction: column;
}
.msg-content-wrap .msg-bubble {
  max-width: 100%;
  align-self: flex-start;
}
.msg-row.user .msg-content-wrap {
  align-items: flex-end;
}
.msg-toolcalls {
  width: 100%;
}
.msg-row.user .msg-bubble {
  background: #0052d9;
  color: #fff;
  border-top-right-radius: 4px;
}
.msg-row.assistant .msg-bubble {
  background: #f5f5f5;
  color: #1d1d1f;
  border-top-left-radius: 4px;
}
.empty-greeting {
  height: 100%;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  text-align: center;
}
.greet-avatar {
  font-size: 56px;
}
.greet-title {
  margin-top: 16px;
  font-size: 26px;
  font-weight: 600;
}
.greet-sub {
  margin-top: 8px;
  color: #999;
  font-size: 13px;
}
.greet-chips {
  margin-top: 28px;
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  justify-content: center;
  max-width: 560px;
}
.greet-chip {
  padding: 10px 18px;
  background: #f5f5f5;
  border-radius: 20px;
  font-size: 13px;
  cursor: pointer;
  transition: all 0.15s;
}
.greet-chip:hover {
  background: #e6f4ef;
  color: #00a870;
}
.chat-input-area {
  flex-shrink: 0;
  padding: 0 24px 16px;
}
.input-box {
  max-width: 820px;
  margin: 0 auto;
  border: 1px solid #e7e7e7;
  border-radius: 16px;
  padding: 8px 12px;
  background: #fff;
  box-shadow: 0 4px 16px rgba(0, 0, 0, 0.04);
}
.input-box :deep(.t-textarea__inner) {
  font-size: 14px;
  padding: 6px 4px;
  resize: none;
}
.input-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 4px;
}
.tool-left {
  display: flex;
  gap: 6px;
}
.input-foot {
  text-align: center;
  font-size: 11px;
  color: #bbb;
  margin-top: 8px;
}
</style>
