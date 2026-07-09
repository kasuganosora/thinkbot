<template>
  <div
    class="chat-window"
    data-testid="chat-window"
    role="region"
    aria-label="会话主区域：展示当前会话消息并发送新消息"
  >
    <div class="chat-topbar" data-testid="chat-topbar">
      <div class="topbar-title" data-testid="chat-session-title">
        {{ store.activeBot?.name || '对话' }}
      </div>
      <t-tag v-if="store.activeBot && store.activeBot.model" theme="success" variant="light" data-testid="chat-bot-model">
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
        :session-id="store.activeBotId"
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
          <div v-if="msg.role === 'assistant'" class="msg-header" data-testid="chat-bot-header">
            <div class="msg-header-avatar">{{ store.activeBot?.avatar || '🤖' }}</div>
            <span class="msg-header-name">{{ store.activeBot?.name || 'Bot' }}</span>
          </div>
          <div class="msg-content-wrap">
            <div class="msg-bubble" data-testid="chat-message-content">
              <template v-if="msg.role === 'assistant' && store.replying && !msg.content">
                <span class="typing-indicator" data-testid="chat-typing">
                  <i></i><i></i><i></i>
                </span>
                <span class="typing-text">思考中…</span>
              </template>
              <template v-else-if="msg.role === 'assistant'"><div
                class="markdown-body"
                v-html="renderMarkdown(msg.content)"
              ></div><span
                v-if="store.replying"
                class="stream-caret"
                data-testid="chat-stream-caret"
              ></span></template>
              <template v-else>{{ msg.content }}</template>
            </div>
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
            :disabled="!draft.trim() || store.replying"
            :loading="store.replying"
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
import { ref, computed, nextTick, watch, onUnmounted } from 'vue'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { useBotStore } from '@/stores/bot'
import { useUserStore } from '@/stores/user'
import SessionWorkflowPanel from '@/components/SessionWorkflowPanel.vue'
import ToolCallCard from '@/components/ToolCallCard.vue'

marked.setOptions({ breaks: true, gfm: true })
function renderMarkdown(text) {
  if (!text) return ''
  return DOMPurify.sanitize(marked.parse(text))
}

const store = useBotStore()
const userStore = useUserStore()
const draft = ref('')
const scrollRef = ref()

// 直接使用 store 的 messages（reactive ref，SSE 更新时自动触发重渲染）
const messages = computed(() => store.messages)

// 工作流（预留）
const sessionWorkflowId = computed(() => '')

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

// 消息列表长度变化时滚动到底部
watch(() => messages.value.length, scrollToBottom)
// bot 切换时滚动
watch(() => store.activeBotId, scrollToBottom)
// 流式期间持续滚动（通过 replying 状态变化 + messages 引用变化）
watch(() => store.replying, (val) => {
  if (val) {
    // 流式开始：启动定时滚动
    _scrollTimer = setInterval(scrollToBottom, 200)
  } else {
    // 流式结束：停止定时滚动，做一次最终滚动
    clearInterval(_scrollTimer)
    _scrollTimer = null
    scrollToBottom()
  }
})

let _scrollTimer = null

// 组件卸载时清理定时器，防止内存泄漏
onUnmounted(() => {
  if (_scrollTimer) {
    clearInterval(_scrollTimer)
    _scrollTimer = null
  }
})

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
  padding: 4px 32px;
  max-width: 820px;
  margin: 0 auto;
}
.msg-row.assistant {
  flex-direction: column;
  align-items: stretch;
  margin-top: 18px;
}
.msg-header {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 10px;
}
.msg-header-avatar {
  width: 30px;
  height: 30px;
  border-radius: 50%;
  background: #e6f4ef;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 17px;
  flex-shrink: 0;
}
.msg-header-name {
  font-size: 15px;
  font-weight: 600;
  color: #1d1d1f;
}
.msg-row.user {
  justify-content: flex-end;
}
.msg-bubble {
  font-size: 15px;
  line-height: 1.75;
  white-space: pre-wrap;
  word-break: break-word;
}
.msg-content-wrap {
  min-width: 0;
  display: flex;
  flex-direction: column;
}
.msg-row.assistant .msg-content-wrap {
  width: 100%;
  max-width: 100%;
}
.msg-row.user .msg-content-wrap {
  max-width: 75%;
  align-items: flex-end;
}
.msg-toolcalls {
  width: 100%;
}
.msg-row.user .msg-bubble {
  background: #f0f1f3;
  color: #1d1d1f;
  border-radius: 14px;
  padding: 10px 16px;
}
.msg-row.assistant .msg-bubble {
  background: transparent;
  color: #1d1d1f;
  padding: 4px 0;
  width: 100%;
}
/* markdown 渲染内容样式（v-html，需用 deep 穿透 scoped） */
.markdown-body {
  white-space: normal;
  font-size: 15px;
  line-height: 1.75;
  word-break: break-word;
}
.markdown-body :deep(h1),
.markdown-body :deep(h2),
.markdown-body :deep(h3),
.markdown-body :deep(h4) {
  font-weight: 600;
  line-height: 1.4;
  margin: 16px 0 8px;
}
.markdown-body :deep(h1) { font-size: 20px; }
.markdown-body :deep(h2) { font-size: 18px; }
.markdown-body :deep(h3) { font-size: 16px; }
.markdown-body :deep(h4) { font-size: 15px; }
.markdown-body :deep(p) { margin: 8px 0; }
.markdown-body :deep(ul),
.markdown-body :deep(ol) { margin: 8px 0; padding-left: 22px; }
.markdown-body :deep(li) { margin: 3px 0; }
.markdown-body :deep(li > p) { margin: 0; }
.markdown-body :deep(strong) { font-weight: 600; }
.markdown-body :deep(hr) {
  border: none;
  border-top: 1px solid #e7e7e7;
  margin: 16px 0;
}
.markdown-body :deep(blockquote) {
  margin: 8px 0;
  padding: 2px 12px;
  border-left: 3px solid #d0d0d0;
  color: #666;
}
.markdown-body :deep(code) {
  background: #f2f3f5;
  padding: 2px 5px;
  border-radius: 4px;
  font-size: 13px;
  font-family: "SFMono-Regular", Consolas, Menlo, monospace;
}
.markdown-body :deep(pre) {
  background: #f6f7f9;
  padding: 12px 14px;
  border-radius: 8px;
  overflow-x: auto;
  margin: 10px 0;
}
.markdown-body :deep(pre code) {
  background: transparent;
  padding: 0;
  font-size: 13px;
}
.markdown-body :deep(a) { color: #0052d9; text-decoration: none; }
.markdown-body :deep(a:hover) { text-decoration: underline; }
.markdown-body :deep(table) {
  border-collapse: collapse;
  margin: 10px 0;
  width: 100%;
}
.markdown-body :deep(th),
.markdown-body :deep(td) {
  border: 1px solid #e0e0e0;
  padding: 6px 10px;
  text-align: left;
}
.markdown-body :deep(> *:first-child) { margin-top: 0; }
.markdown-body :deep(> *:last-child) { margin-bottom: 0; }
/* bot 持续输出时的 loading 指示 */
.typing-indicator {
  display: inline-flex;
  gap: 4px;
  margin-right: 8px;
  vertical-align: middle;
}
.typing-indicator i {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: #00a870;
  animation: chat-blink 1.2s infinite ease-in-out;
}
.typing-indicator i:nth-child(2) { animation-delay: 0.2s; }
.typing-indicator i:nth-child(3) { animation-delay: 0.4s; }
.typing-text {
  color: #8a8a8a;
  font-size: 13px;
}
.stream-caret {
  display: inline-block;
  width: 2px;
  height: 14px;
  margin-left: 2px;
  background: #00a870;
  vertical-align: text-bottom;
  animation: chat-caret 1s steps(1) infinite;
}
@keyframes chat-blink {
  0%, 80%, 100% { opacity: 0.25; transform: translateY(0); }
  40% { opacity: 1; transform: translateY(-2px); }
}
@keyframes chat-caret {
  0%, 50% { opacity: 1; }
  50.01%, 100% { opacity: 0; }
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
