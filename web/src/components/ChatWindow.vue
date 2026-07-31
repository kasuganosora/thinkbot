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
      @scroll="onScroll"
    >
      <div v-if="sessionWorkflowId" class="wf-sticky">
        <SessionWorkflowPanel
          :session-id="store.activeBotId"
          :workflow-id="sessionWorkflowId"
        />
      </div>

      <!-- 顶部分页哨兵：上翻到顶自动加载更早消息，不满一屏时可手动点击兜底 -->
      <div v-if="store.hasMore" class="load-more-bar" data-testid="chat-load-more">
        <span v-if="store.loadingMore" class="load-more-loading">
          <t-loading size="small" />
          <span>加载更早的消息…</span>
        </span>
        <button v-else type="button" class="load-more-btn" @click="triggerLoadMore">
          加载更早的消息
        </button>
      </div>
      <div
        v-else-if="messages.length >= 20"
        class="load-more-bar load-more-end"
        data-testid="chat-history-start"
      >
        已经到最开始了
      </div>

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
            <!-- 按 LLM 调用顺序交错渲染文本和工具卡片 -->
            <template v-if="msg.role === 'assistant' && hasOrderedParts(msg)">
              <div class="msg-bubble" data-testid="chat-message-content">
                <template v-if="store.replying && !hasContentText(msg)">
                  <span class="typing-indicator" data-testid="chat-typing"><i></i><i></i><i></i></span>
                  <span class="typing-text">思考中…</span>
                </template>
                <template v-for="(part, pi) in renderParts(msg)" :key="pi">
                  <!-- 文本 part → markdown -->
                  <div
                    v-if="part.type === 'text' && part.content"
                    class="markdown-body"
                    v-html="renderMarkdown(part.content)"
                  ></div>
                  <span
                    v-if="part.type === 'text' && store.replying && isLastTextPart(msg, pi)"
                    class="stream-caret"
                    data-testid="chat-stream-caret"
                  ></span>
                  <!-- 工具 part → 卡片/归并组 -->
                  <div
                    v-if="part.type === 'tool'"
                    class="msg-toolcall-item"
                    data-testid="chat-message-toolcall"
                  >
                    <ToolCallGroup
                      v-if="part._group"
                      :name="part.name"
                      :calls="part._group"
                    />
                    <ToolCallCard
                      v-else
                      :key="part.id"
                      :call="part"
                    />
                  </div>
                </template>
              </div>
            </template>
            <!-- 降级：无 parts 的旧消息（含 user 消息）仍走原逻辑 -->
            <template v-else>
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
              <template v-for="(g, gi) in groupToolCalls(msg.toolCalls)" :key="gi">
                <ToolCallGroup
                  v-if="g.type === 'group'"
                  :name="g.name"
                  :calls="g.calls"
                />
                <ToolCallCard
                  v-else
                  :key="g.call.id"
                  :call="g.call"
                />
              </template>
            </div>
            </template>
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

      <!-- 回到底部按钮：流式期间用户上翻时显示 -->
      <Transition name="scroll-bottom-fade">
        <div
          v-if="store.replying && !isAtBottom"
          class="scroll-to-bottom-btn"
          data-testid="chat-scroll-bottom"
          role="button"
          aria-label="回到底部"
          tabindex="0"
          @click="scrollToBottomManual"
          @keydown.enter="scrollToBottomManual"
        >
          <t-icon name="chevron-down" />
        </div>
      </Transition>
    </div>

    <div class="chat-input-area" data-testid="chat-input-area">
      <div class="input-box">
        <t-textarea
          v-model="draft"
          :autosize="{ minRows: 1, maxRows: 6 }"
          :placeholder="inputPlaceholder"
          :bordered="false"
          data-testid="chat-input-textarea"
          :aria-label="inputAriaLabel"
          @keydown="onKeydown"
        />
        <div class="input-toolbar">
          <div class="tool-left">
            <label class="attach-btn" data-testid="chat-btn-attach" title="上传文件（图片、音频、视频）">
              <t-icon name="attach" />
              <input
                ref="fileInputRef"
                type="file"
                multiple
                accept="image/*,audio/*,video/*,.pdf,.txt,.md,.json,.csv,.xml,.html,.css,.js,.ts,.py,.go,.java,.c,.cpp,.h,.sh,.yaml,.yml,.toml,.env,.log,.doc,.docx,.xls,.xlsx,.ppt,.pptx"
                class="attach-input"
                @change="onFileSelect"
              />
            </label>
            <!-- 附件预览条（有附件时显示） -->
            <div v-if="attachments.length" class="attach-strip">
              <span
                v-for="(f, i) in attachments"
                :key="i"
                class="attach-chip"
                :data-testid="`chat-attach-${i}`"
              >
                <t-icon :name="fileIcon(f.type)" />
                {{ f.name }}
                <t-icon name="close" class="attach-remove" @click.stop="removeAttach(i)" />
              </span>
            </div>
          </div>
          <!-- 发送 / 中断 按钮 -->
          <t-button
            v-if="!store.replying"
            theme="primary"
            shape="circle"
            :disabled="!draft.trim() && !attachments.length"
            data-testid="chat-btn-send"
            aria-label="发送消息"
            @click="send"
          >
            <template #icon><t-icon name="send" /></template>
          </t-button>
          <template v-else>
            <!-- 生成中：补充内容注入同一轮（Claude-CLI 风格） -->
            <t-button
              theme="primary"
              shape="circle"
              :disabled="!draft.trim() || attachments.length > 0"
              data-testid="chat-btn-append"
              aria-label="补充内容（同一轮）"
              title="把这段内容补充进当前回复，无需停止"
              @click="send"
            >
              <template #icon><t-icon name="arrow-up" /></template>
            </t-button>
            <t-button
              theme="danger"
              shape="circle"
              variant="outline"
              data-testid="chat-btn-stop"
              aria-label="停止生成"
              @click="store.stopReply()"
            >
              <template #icon><t-icon name="stop" /></template>
            </t-button>
          </template>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, nextTick, watch, onUnmounted } from 'vue'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { useBotStore } from '@/stores/bot'
import { useUserStore } from '@/stores/user'
import { loadUserPreferences } from '@/utils/userPreferences'
import SessionWorkflowPanel from '@/components/SessionWorkflowPanel.vue'
import ToolCallCard from '@/components/ToolCallCard.vue'
import ToolCallGroup from '@/components/ToolCallGroup.vue'

marked.setOptions({ breaks: true, gfm: true })
function renderMarkdown(text) {
  if (!text) return ''
  return DOMPurify.sanitize(marked.parse(text))
}

const store = useBotStore()
const userStore = useUserStore()
const userPreferences = computed(() => loadUserPreferences(userStore.user?.id))
const inputPlaceholder = computed(() => userPreferences.value.sendKey === 'cmd-enter'
  ? '有问题，尽管问，⌘ / Ctrl + Enter 发送'
  : '有问题，尽管问，Shift + Enter 换行')
const inputAriaLabel = computed(() => userPreferences.value.sendKey === 'cmd-enter'
  ? '消息输入框，Command 或 Control 加 Enter 发送，Enter 换行'
  : '消息输入框，Enter 发送，Shift 加 Enter 换行')
const draft = ref('')
const scrollRef = ref()
const fileInputRef = ref()
// ── 智能滚动：用户在底部才自动滚，上翻时不干扰 ──
const isAtBottom = ref(true)
const SCROLL_THRESHOLD = 120 // 距底部多少 px 内视为"在底部"

/** 判断当前滚动位置是否接近底部 */
function checkAtBottom() {
  const el = scrollRef.value
  if (!el) return
  isAtBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight < SCROLL_THRESHOLD
}

/** 滚动到底部（供外部调用） */
function scrollToBottom() {
  nextTick(() => {
    if (scrollRef.value) scrollRef.value.scrollTop = scrollRef.value.scrollHeight
  })
}

// ── 上翻分页：滚到顶部附近自动加载更早的一页（每页 20 条）──
const LOAD_MORE_THRESHOLD = 80 // 距顶部多少 px 内触发加载

/**
 * 加载更早一页消息，并保持视觉滚动位置不跳动。
 * 关键：prepend 会撑高内容，需按「新旧 scrollHeight 差值」补偿 scrollTop，
 * 否则用户视野会被瞬间推走。
 */
async function triggerLoadMore() {
  const el = scrollRef.value
  if (!el || !store.hasMore || store.loadingMore) return
  const prevHeight = el.scrollHeight
  const prevTop = el.scrollTop
  const added = await store.loadMoreMessages()
  if (!added) return
  await nextTick()
  const cur = scrollRef.value
  if (!cur) return
  // 补偿新增内容的高度，保持原来那条消息仍停在同一视觉位置
  cur.scrollTop = cur.scrollHeight - prevHeight + prevTop
}

/** 用户手动滚动时更新 atBottom 状态，并在接近顶部时触发上翻分页 */
function onScroll() {
  checkAtBottom()
  const el = scrollRef.value
  if (!el) return
  if (el.scrollTop < LOAD_MORE_THRESHOLD && store.hasMore && !store.loadingMore) {
    triggerLoadMore()
  }
}

/** 点击"回到底部"按钮 */
function scrollToBottomManual() {
  scrollToBottom()
  isAtBottom.value = true
}
const attachments = ref([])  // { name, type, size, dataUrl (base64) }[]

/** 根据 MIME type 返回图标名 */
function fileIcon(mimeType) {
  if (mimeType?.startsWith('image')) return 'image'
  if (mimeType?.startsWith('audio')) return 'voice'
  if (mimeType?.startsWith('video')) return 'video-play'
  if (mimeType === 'application/pdf') return 'file-pdf'
  return 'file-unknown'
}

/** 文件选择 → 读为 base64 data URL */
async function onFileSelect(e) {
  const files = Array.from(e.target.files || [])
  for (const f of files) {
    // 单文件上限 20MB
    if (f.size > 20 * 1024 * 1024) {
      alert(`文件 "${f.name}" 超过 20MB 上限，已跳过`)
      continue
    }
    const dataUrl = await readFileAsDataURL(f)
    attachments.value.push({
      name: f.name,
      type: f.type || guessType(f.name),
      size: f.size,
      dataUrl,
    })
  }
  // 重置 input 以允许重复选择同一文件
  e.target.value = ''
}

function removeAttach(i) { attachments.value.splice(i, 1) }

/** 将 File 读为 base64 Data URL */
function readFileAsDataURL(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(reader.result)
    reader.onerror = () => reject(reader.error)
    reader.readAsDataURL(file)
  })
}

/** 从扩展名猜测 MIME type */
function guessType(name) {
  const ext = name.split('.').pop()?.toLowerCase() || ''
  const map = {
    pdf: 'application/pdf', txt: 'text/plain', md: 'text/markdown',
    json: 'application/json', csv: 'text/csv', xml: 'application/xml',
    html: 'text/html', css: 'text/css',
    js: 'text/javascript', ts: 'text/typescript', py: 'text/x-python',
    go: 'text/x-go', java: 'text/x-java', c: 'text/x-c',
    cpp: 'text/x-c++', h: 'text/x-c-header', sh: 'text/x-shellscript',
    yaml: 'text/yaml', yml: 'text/yaml', toml: 'text/toml', env: 'text/plain',
    log: 'text/plain',
    doc: 'application/msword', docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    xls: 'application/vnd.ms-excel', xlsx: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
    ppt: 'application/vnd.ms-powerpoint', pptx: 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
  }
  return map[ext] || 'application/octet-stream'
}

// 直接使用 store 的 messages（reactive ref，SSE 更新时自动触发重渲染）
const messages = computed(() => store.messages)

// ── 有序 Parts 渲染（LLM 文本与工具按调用顺序交错展示）──

/** 消息是否包含有序 parts 数组 */
function hasOrderedParts(msg) {
  return Array.isArray(msg.parts) && msg.parts.length > 0
}

/** parts 中是否有文本内容（用于判断"思考中"占位） */
function hasContentText(msg) {
  if (!Array.isArray(msg.parts)) return !!msg.content
  return msg.parts.some(p => p.type === 'text' && p.content)
}

/** 当前 text part 是否是最后一个 part（用于流式光标位置） */
function isLastTextPart(msg, pi) {
  if (!Array.isArray(msg.parts)) return true
  // 检查之后是否还有非空 part
  for (let i = pi + 1; i < msg.parts.length; i++) {
    if (msg.parts[i].type === 'text' && msg.parts[i].content) return false
    if (msg.parts[i].type === 'tool') return false
  }
  return true
}

/**
 * 将有序 parts 展平为渲染列表。
 * 规则：
 * - text part → {type:'text', content:'...'}
 * - tool part → {type:'tool', ...call} （单独卡片）
 * - 连续同名 tool part → 第一个升为 {type:'tool', _group:[...]}
 *
 * 这样 LLM 的 "说话→调用工具→说话→调用工具" 自然呈现为交错顺序。
 */
function renderParts(msg) {
  if (!Array.isArray(msg.parts)) return []
  const out = []
  let groupAcc = null   // 正在收集的连续同名工具组

  for (const p of msg.parts) {
    if (p.type === 'text') {
      // flush group
      if (groupAcc) { flushGroup(out, groupAcc); groupAcc = null }
      out.push(p)
      continue
    }
    if (p.type === 'tool') {
      if (groupAcc && groupAcc.name === p.name) {
        // 同名连续 → 加入当前组
        groupAcc.calls.push(p)
      } else {
        // 不同名 → flush 旧组，开新组或单卡
        if (groupAcc) { flushGroup(out, groupAcc); groupAcc = null }
        groupAcc = { name: p.name, calls: [p] }
      }
    }
  }
  if (groupAcc) flushGroup(out, groupAcc)
  return out
}

/** 将归并组写入渲染列表 */
function flushGroup(out, g) {
  if (g.calls.length === 1) {
    // 单个工具不套组壳，直接渲染 ToolCallCard
    out.push(g.calls[0])
  } else {
    // 多个同名工具 → 带组的 tool part
    out.push({ type: 'tool', name: g.name, _group: g.calls })
  }
}

// ── 降级：旧消息无 parts 时仍用此函数对 toolCalls 归并分组 ──
function groupToolCalls(calls) {
  const list = Array.isArray(calls) ? calls : []
  const groups = []
  for (const c of list) {
    const last = groups[groups.length - 1]
    if (last && last.type === 'group' && last.name === c.name) {
      last.calls.push(c)
    } else if (last && last.type === 'single' && last.call.name === c.name) {
      // 把落单的上一个同 name 调用升级成组
      groups[groups.length - 1] = { type: 'group', name: c.name, calls: [last.call, c] }
    } else {
      groups.push({ type: 'single', call: c })
    }
  }
  return groups
}

// 工作流：由 bot 在对话中通过 task 工具创建后，从 store 拿到 workflowId 驱动面板展示
const sessionWorkflowId = computed(() => store.activeWorkflowId)

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

// ── 智能自动滚动 ──

/** 仅在用户位于底部时才执行滚底（不干扰手动上翻） */
function scrollToBottomIfAtBottom() {
  if (isAtBottom.value) scrollToBottom()
}

// 消息列表长度变化时（新消息到达）智能滚动
watch(() => messages.value.length, scrollToBottomIfAtBottom)
// bot 切换时强制滚到底部
watch(() => store.activeBotId, () => {
  isAtBottom.value = true
  scrollToBottom()
})

// 流式期间：仅在用户在底部时才持续跟滚；用户上翻即暂停
watch(() => store.replying, (val) => {
  if (val) {
    // 流式开始：启动智能定时滚动
    _scrollTimer = setInterval(scrollToBottomIfAtBottom, 200)
  } else {
    // 流式结束：停止定时器，做一次最终滚底
    clearInterval(_scrollTimer)
    _scrollTimer = null
    isAtBottom.value = true
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
  if (!text && !attachments.value.length) return
  // 提取附件信息（dataUrl 含 base64 编码的文件数据）
  const attachPayload = attachments.value.map(a => ({
    name: a.name,
    type: a.type,
    size: a.size,
    dataUrl: a.dataUrl,   // "data:<mime>;base64,<data>"
  }))
  // 生成中：把纯文本补充注入同一轮（不开启新一轮）；带附件时回退普通发送。
  if (store.replying && !attachments.value.length) {
    store.appendToCurrentReply(text || '[附件]')
  } else {
    store.sendMessage(text || '[附件]', attachPayload)
  }
  draft.value = ''
  attachments.value = []
}

function quickSend(text) {
  store.sendMessage(text)
}

function onKeydown(value, { e }) {
  if (e.key !== 'Enter' || e.isComposing) return

  const useCommandEnter = userPreferences.value.sendKey === 'cmd-enter'
  const shouldSend = useCommandEnter
    ? (e.metaKey || e.ctrlKey)
    : !e.shiftKey && !e.metaKey && !e.ctrlKey

  if (shouldSend) {
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
  position: relative; /* 为悬浮按钮提供定位上下文 */
}
/* 回到底部悬浮按钮 */
.scroll-to-bottom-btn {
  position: absolute;
  bottom: 16px;
  right: 24px;
  width: 36px;
  height: 36px;
  border-radius: 50%;
  background: #fff;
  border: 1px solid #e0e0e0;
  box-shadow: 0 2px 12px rgba(0, 0, 0, 0.1);
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  color: #666;
  font-size: 18px;
  transition: all 0.2s ease;
  z-index: 10;
}
.scroll-to-bottom-btn:hover {
  background: #0052d9;
  border-color: #0052d9;
  color: #fff;
  box-shadow: 0 4px 16px rgba(0, 82, 217, 0.3);
}
/* 按钮淡入淡出 */
.scroll-bottom-fade-enter-active,
.scroll-bottom-fade-leave-active {
  transition: opacity 0.25s ease, transform 0.25s ease;
}
.scroll-bottom-fade-enter-from,
.scroll-bottom-fade-leave-to {
  opacity: 0;
  transform: translateY(8px) scale(0.9);
}
/* workflow 面板：固定在聊天区顶部，任务进行中始终可见（避免被长对话滚出视口） */
.wf-sticky {
  position: sticky;
  top: 0;
  z-index: 6;
  background: #fff;
  padding: 10px 32px 8px;
  border-bottom: 1px solid #f0f0f0;
}
/* 顶部分页条：加载更早消息 / 到达历史起点 */
.load-more-bar {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  padding: 10px 32px 14px;
  font-size: 13px;
  color: #999;
}
.load-more-loading {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}
.load-more-btn {
  background: transparent;
  border: 1px solid #e0e0e0;
  border-radius: 14px;
  padding: 4px 14px;
  font-size: 13px;
  color: #666;
  cursor: pointer;
  transition: all 0.2s ease;
}
.load-more-btn:hover {
  border-color: #0052d9;
  color: #0052d9;
}
.load-more-end {
  color: #bbb;
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
.msg-toolcall-item {
  width: 100%;
  margin: 2px 0;
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
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.attach-btn {
  position: relative;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 32px; height: 32px;
  border-radius: 8px;
  cursor: pointer;
  color: #666;
  font-size: 18px;
  transition: background 0.15s, color 0.15s;
}
.attach-btn:hover { background: #f2f3f5; color: #333; }
.attach-input {
  position: absolute; inset: 0; opacity: 0; cursor: pointer;
  font-size: 0;
}
.attach-strip {
  display: flex;
  gap: 6px;
  overflow-x: auto;
  padding: 2px 0;
  max-width: 400px;
  scrollbar-width: none;
}
.attach-strip::-webkit-scrollbar { display: none; }
.attach-chip {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 3px 8px 3px 6px;
  border-radius: 6px;
  background: #eef1f5;
  font-size: 12px;
  color: #444;
  white-space: nowrap;
  max-width: 160px;
}
.attach-chip .t-icon:first-child { color: #4b8bf5; font-size: 14px; }
.attach-chip .attach-remove {
  font-size: 12px; color: #aaa; cursor: pointer; opacity: 0;
  transition: opacity 0.12s;
}
.attach-chip:hover .attach-remove { opacity: 1; }
.attach-chip:hover .attach-remove:hover { color: #e06a6a; }
</style>