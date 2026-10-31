<template>
  <div
    class="chat-window"
    :class="{ 'is-dragging': draggingFiles }"
    @dragenter="onDragEnter"
    @dragover="onDragOver"
    @dragleave="onDragLeave"
    @drop="onFileDrop"
    data-testid="chat-window"
    role="region"
    aria-label="会话主区域：展示当前会话消息并发送新消息"
  >
    <div v-if="draggingFiles" class="attachment-drop-overlay" aria-hidden="true"><t-icon name="file-add" /><strong>松开以添加附件</strong><span>单个文件不超过 20 MB，最多 10 个</span></div>
    <div class="chat-topbar" data-testid="chat-topbar">
      <button type="button" class="topbar-icon" :title="layout.isNarrow ? '打开导航' : (layout.botCollapsed ? '展开 Bot 导航' : '收起 Bot 导航')" aria-label="切换 Bot 导航" @click="layout.toggleBots()"><t-icon name="view-list" /></button>
      <button v-if="!layout.isNarrow" type="button" class="topbar-icon" :title="layout.sessionsVisible ? '收起会话列表' : '展开会话列表'" aria-label="切换会话列表" :aria-expanded="layout.sessionsVisible" @click="layout.toggleSessions()"><t-icon name="chat" /></button>
      <div class="topbar-heading">
        <div class="topbar-title" data-testid="chat-session-title" :title="sessionTitle">{{ sessionTitle }}</div>
        <div class="topbar-subtitle"><span>{{ store.activeBot?.name || '选择一个 Bot 开始' }}</span><span v-if="store.activeBot?.model" class="topbar-model" data-testid="chat-bot-model">{{ store.activeBot.model }}</span></div>
      </div>
      <span v-if="store.replying" class="reply-status"><span></span> 正在执行</span>
      <button v-if="!layout.toolsOpen" type="button" class="topbar-workspace" title="打开工作区" aria-label="打开工作区" @click="layout.openTools()"><t-icon name="terminal" /><span>工作区</span></button>
    </div>

    <div v-if="store.streamError" class="stream-error-banner" role="status" data-testid="chat-stream-error"><t-icon name="error-circle" /><span>{{ store.streamError }}</span><button type="button" :disabled="store.reconnecting" @click="store.reconnectStream()">{{ store.reconnecting ? '连接中…' : '重新连接' }}</button></div>
    <button v-if="waitingChoices.length" type="button" class="choice-reminder" data-testid="chat-choice-reminder" @click="goToChoice(waitingChoices[0].id)"><t-icon name="help-circle" /><span>有 {{ waitingChoices.length }} 个问题等待你的选择</span><strong>前往处理</strong><t-icon name="chevron-right" /></button>

    <div
      ref="scrollRef"
      class="chat-body"
      data-testid="chat-message-list"
      role="log"
      aria-label="消息列表"
      aria-live="polite"
      @scroll="onScroll"
      @wheel.passive="cancelScrollRestore"
      @touchstart.passive="cancelScrollRestore"
      @keydown="onReadingKeydown"
      @click="onMessageBodyClick"
    >
      <div v-if="store.messagesError" class="history-notice" role="alert" data-testid="chat-history-error"><t-icon name="error-circle" /><span>消息加载失败{{ messages.length ? '，已保留当前内容' : '' }}。</span><button type="button" :disabled="store.messagesLoading" @click="store.loadMessages()">重试</button></div>
      <!-- 顶部分页哨兵：上翻到顶自动加载更早消息，不满一屏时可手动点击兜底 -->
      <div v-if="store.hasMore" class="load-more-bar" data-testid="chat-load-more">
        <span v-if="store.loadingMore" class="load-more-loading">
          <t-loading size="small" />
          <span>加载更早的消息…</span>
        </span>
        <button v-else type="button" class="load-more-btn" @click="triggerLoadMore">
          {{ store.loadMoreError || '加载更早的消息' }}
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
          :data-message-id="String(msg.id)"
        >
          <div v-if="msg.role === 'assistant'" class="msg-header" data-testid="chat-bot-header">
            <div class="msg-header-avatar">{{ store.activeBot?.avatar || '🤖' }}</div>
            <span class="msg-header-name">{{ store.activeBot?.name || 'Bot' }}</span>
          </div>
          <div class="msg-content-wrap">
            <!-- 按 LLM 调用顺序交错渲染文本和工具卡片 -->
            <template v-if="msg.role === 'assistant' && hasOrderedParts(msg)">
              <div class="msg-bubble" data-testid="chat-message-content">
                <template v-if="isStreamingMessage(msg) && !hasContentText(msg)">
                  <span class="typing-indicator" data-testid="chat-typing"><i></i><i></i><i></i></span>
                  <span class="typing-text">思考中…</span>
                </template>
                <template v-for="(part, pi) in renderParts(msg)" :key="renderPartKey(part, pi)">
                  <!-- 文本 part → markdown -->
                  <div
                    v-if="part.type === 'text' && part.content"
                    class="markdown-body"
                    v-html="renderMarkdown(part.content)"
                  ></div>
                  <span
                    v-if="part.type === 'text' && isStreamingMessage(msg) && isLastTextPart(msg, pi)"
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
                    <div
                      v-for="qid in choiceIdsForToolPart(part)"
                      :key="'choice-inline-' + qid"
                      class="cc-inline"
                    >
                      <ChoiceCard
                        :data-choice-id="qid"
                        :payload="store.choiceState(qid)?.payload || { questionId: qid }"
                        :status="choiceCardStatus(qid, part)"
                        :submitted="store.choiceState(qid)?.submitted || false"
                      />
                    </div>
                  </div>
                </template>
              </div>
            </template>
            <!-- 降级：无 parts 的旧消息（含 user 消息）仍走原逻辑 -->
            <template v-else>
            <div class="msg-bubble" data-testid="chat-message-content">
              <template v-if="msg.role === 'assistant' && isStreamingMessage(msg) && !msg.content">
                <span class="typing-indicator" data-testid="chat-typing">
                  <i></i><i></i><i></i>
                </span>
                <span class="typing-text">思考中…</span>
              </template>
              <template v-else-if="msg.role === 'assistant'"><div
                class="markdown-body"
                v-html="renderMarkdown(msg.content)"
              ></div><span
                v-if="isStreamingMessage(msg)"
                class="stream-caret"
                data-testid="chat-stream-caret"
              ></span></template>
              <template v-else>{{ msg.content }}<span
                v-if="msg._appendUnconfirmed"
                class="append-unconfirmed"
                data-testid="chat-append-unconfirmed"
                title="这条补充的投递结果未确认（网络异常），如未被回复请手动重发"
              >投递未确认</span></template>
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
                <template v-else>
                  <ToolCallCard
                    :key="g.call.id"
                    :call="g.call"
                  />
                  <!-- user_choice：内联选择卡（按 LLM 发出顺序紧随工具卡片，
                       而非堆到消息末尾）-->
                  <div
                    v-if="store.choiceIdByToolCallId(g.call.id)"
                    :key="'choice-inline-' + g.call.id"
                    class="cc-inline"
                  >
                    <ChoiceCard
                      :data-choice-id="store.choiceIdByToolCallId(g.call.id)"
                      :payload="store.choiceState(store.choiceIdByToolCallId(g.call.id))?.payload || { questionId: store.choiceIdByToolCallId(g.call.id) }"
                      :status="choiceCardStatus(store.choiceIdByToolCallId(g.call.id), g.call)"
                      :submitted="store.choiceState(store.choiceIdByToolCallId(g.call.id))?.submitted || false"
                    />
                  </div>
                </template>
              </template>
            </div>
            </template>
            <SecretInputCard v-for="secretRequest in secretRequestsForMessage(msg)" :key="secretRequest.question_id"
              :request="secretRequest" :bot-id="store.activeBotId" :session-id="String(store.activeSessionId || '')" />
            <MessageArtifacts v-if="msg.role === 'assistant' && store.activeBotId" :message="msg" :bot-id="store.activeBotId" :session-id="store.activeSessionId" />
            <div v-if="messageRunNotice(msg)" class="msg-run-notice" :class="messageRunNotice(msg).tone" role="status" data-testid="chat-message-run-notice"><t-icon name="info-circle" /><span>{{ messageRunNotice(msg).text }}</span></div>
            <div v-if="msg.persistenceError" class="msg-run-notice warning" role="status" data-testid="chat-message-persistence-notice"><t-icon name="error-circle" /><span>回复已完成，但历史记录保存失败。请复制留存，不必重复发送。</span></div>
            <div class="msg-actions">
              <time v-if="messageTime(msg)" :datetime="msg.createdAt" :title="messageTime(msg, true)">{{ messageTime(msg) }}</time>
              <button v-if="copyableText(msg)" type="button" class="message-copy" :aria-label="copiedMessageId === msg.id ? '消息已复制' : '复制消息'" @click="copyMessage(msg)"><t-icon :name="copiedMessageId === msg.id ? 'check' : 'copy'" />{{ copiedMessageId === msg.id ? '已复制' : '复制' }}</button>
            </div>
          </div>
          <!-- 工作流卡片：内联锚定在创建它的那一轮助手消息之后，而非钉在对话底部。
               渲染条件不再强依赖 msg.workflowId（刷新/历史会话里该字段不持久化、且
                findAndTagWorkflowMessage 只在已加载页扫描，创建工作流的那条消息若在未加载的
                ／更早分页页就会锚定失败导致面板永远不显示）。改为：只要本会话 activeWorkflowId
                ／存在就渲染——优先锚到被标记 workflowId 的助手消息，否则兜底到最后一条助手消息，
                ／保证已完成/历史工作流在刷新后依然可见。 -->
          <div
            v-if="msg.role === 'assistant' && store.activeWorkflowId && msg.id === workflowAnchorMsgId"
            class="wf-inline"
          >
            <!-- session-id 必须是当前会话 ID：面板终态时会用它按会话 resume 续跑，
                 早期误传 activeBotId，导致续跑请求带着 botId 当 traceId 永远打不中 -->
            <SessionWorkflowPanel
              :session-id="String(store.activeSessionId || '')"
              :workflow-id="msg.workflowId || store.activeWorkflowId"
            />
          </div>

          <!-- 选择卡片（user_choice）遗留兜底：仅渲染无法关联到 toolCallId 的历史卡片。
               新卡片已内联到对应 ToolCallCard 后面（见上方 msg-toolcalls 循环）。 -->
          <div
            v-for="qid in orphanQuestionIds(msg) || []"
            :key="'choice-' + qid"
            class="wf-inline cc-inline"
          >
            <ChoiceCard
              :data-choice-id="qid"
              :payload="choiceState(qid)?.payload || { questionId: qid }"
              :status="choiceCardStatus(qid)"
              :submitted="choiceState(qid)?.submitted || false"
            />
          </div>
        </div>

      </template>

      <div v-else-if="store.messagesLoading || store.sessionsLoading" class="history-state" role="status" data-testid="chat-history-loading"><t-loading size="medium" /><h3>正在加载会话</h3><p>稍等，消息即将显示</p></div>
      <div v-else-if="store.messagesError || store.sessionsError" class="history-state" data-testid="chat-history-empty-error"><t-icon name="chat" /><h3>暂时无法显示会话</h3><p>请稍后重试，已有内容不会被清除。</p><button v-if="store.sessionsError" class="load-more-btn" type="button" @click="store.loadSessions()">重新加载会话</button></div>
      <div v-else class="empty-greeting" data-testid="chat-empty-state">
        <div class="greet-avatar">{{ store.activeBot?.avatar || '🤖' }}</div>
        <h2 class="greet-title">{{ store.activeBot ? '今天，一起完成些什么？' : '欢迎来到 Bot 平台' }}</h2>
        <p class="greet-sub">{{ store.activeBot ? `与 ${store.activeBot.name} 讨论想法、整理资料，或开始一项任务` : '从左侧选择或创建一个 Bot，开启第一段对话' }}</p>
        <div v-if="store.activeBot" class="greet-chips" data-testid="chat-quick-chips">
          <button
            v-for="(chip, i) in chips"
            :key="i"
            type="button"
            class="greet-chip"
            :data-testid="`chat-quick-chip-${i}`"
            :aria-label="`快捷提问：${chip}`"
            @click="quickSend(chip)"
          >
            {{ chip }}
          </button>
        </div>
      </div>

    </div>

      <!-- 始终保留回到最新入口，回复结束也不打断阅读。 -->
    <div class="scroll-bottom-dock">
      <Transition name="scroll-bottom-fade">
        <button
          v-if="!isAtBottom && messages.length"
          type="button"
          class="scroll-to-bottom-btn"
          data-testid="chat-scroll-bottom"
          :aria-label="hasUnreadContent ? '有新内容，回到最新' : '回到最新'"
          @click="scrollToBottomManual"
        >
          <t-icon name="chevron-down" /><span>{{ hasUnreadContent ? '有新内容 · 回到最新' : '回到最新' }}</span>
        </button>
      </Transition>
    </div>

    <div class="chat-input-area" data-testid="chat-input-area">
      <div ref="composerRef" class="composer" @paste="onPaste">
        <div v-if="store.composerDraft.notice || store.composerDraft.recovery" class="composer-notice" role="status"><t-icon name="info-circle" /><span>{{ store.composerDraft.notice || '上次发送的内容仍可恢复。' }}</span><button v-if="store.composerDraft.recovery" type="button" @click="store.restoreDraft()">恢复上次内容</button><button v-if="!store.composerDraft.recovery" type="button" class="notice-dismiss" aria-label="关闭提示" @click="store.composerDraft.notice = ''"><t-icon name="close" /></button></div>
        <div v-if="copyError" class="composer-notice" role="status">{{ copyError }}</div>
        <CommandPalette
          v-if="slashOpen"
          :query="slashQuery"
          :items="slashItems"
          :active-index="slashIndex"
          @select="onSlashSelect"
          @close="dismissSlash"
          @highlight="slashIndex = $event"
        />
        <div class="input-box">
          <div v-if="attachments.length" class="attach-strip" data-testid="chat-attach-strip">
            <span
              v-for="(f, i) in attachments"
              :key="i"
              class="attach-chip"
              :data-testid="`chat-attach-${i}`"
            >
              <img v-if="f.type?.startsWith('image/') && f.dataUrl" :src="f.dataUrl" class="attach-preview" alt="" />
              <t-loading v-else-if="f.status === 'reading'" size="small" />
              <t-icon v-else :name="fileIcon(f.type)" />
              <span class="attach-info" :title="f.name"><span>{{ f.name }}</span><small>{{ f.status === 'reading' ? '准备中…' : f.status === 'error' ? '读取失败，请重新添加' : formatFileSize(f.size) }}</small></span>
              <button type="button" class="attach-remove" :aria-label="`移除附件 ${f.name}`" @click.stop="removeAttach(i)">
                <t-icon name="close" />
              </button>
            </span>
          </div>
          <t-textarea
            v-model="draft"
            :autosize="{ minRows: 1, maxRows: 6 }"
            :placeholder="inputPlaceholder"
            :bordered="false"
            data-testid="chat-input-textarea"
            :aria-label="inputAriaLabel"
            :aria-expanded="slashOpen"
            aria-autocomplete="list"
            :aria-controls="slashOpen ? 'chat-command-palette' : undefined"
            :aria-activedescendant="slashActiveId"
            @keydown="onKeydown"
          />
          <div class="composer-bar">
            <label class="attach-btn" data-testid="chat-btn-attach" title="添加附件">
              <t-icon name="add" />
              <input
                ref="fileInputRef"
                type="file"
                aria-label="添加附件"
                multiple
                accept="image/*,audio/*,video/*,.pdf,.txt,.md,.json,.csv,.xml,.html,.css,.js,.ts,.py,.go,.java,.c,.cpp,.h,.sh,.yaml,.yml,.toml,.env,.log,.doc,.docx,.xls,.xlsx,.ppt,.pptx"
                class="attach-input"
                @change="onFileSelect"
              />
            </label>
            <span class="composer-hint">{{ composerHint }}</span>
            <div class="input-actions">
              <button
                v-if="!store.replying"
                type="button"
                class="composer-btn send"
                :disabled="!canSend"
                data-testid="chat-btn-send"
                aria-label="发送消息"
                @click="send"
              >
                <span>发送</span><t-icon name="arrow-up" />
              </button>
              <template v-else>
                <button
                  type="button"
                  class="composer-btn send"
                  :disabled="!canSend"
                  data-testid="chat-btn-append"
                  aria-label="补充内容（同一轮）"
                  title="把这段内容补充进当前回复，无需停止"
                  @click="send"
                >
                  <span>{{ store.composerDraft.pending ? '投递中' : '补充' }}</span><t-icon name="arrow-up" />
                </button>
                <button
                  type="button"
                  class="composer-btn stop"
                  :disabled="store.stopping"
                  data-testid="chat-btn-stop"
                  aria-label="停止生成"
                  @click="store.stopReply()"
                >
                  <t-icon name="stop" /><span>{{ store.stopping ? '停止中' : '停止' }}</span>
                </button>
              </template>
            </div>
          </div>
        </div>
      </div>
      <div class="composer-footer">{{ sendKeyHint }}<span>草稿仅保存在当前页面</span></div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed, nextTick, watch, onMounted, onUnmounted, onUpdated } from 'vue'
import { renderMarkdown } from '@/utils/markdown'
import { useBotStore } from '@/stores/bot'
import { useLayoutStore } from '@/stores/layout'
import { useUserStore } from '@/stores/user'
import { loadUserPreferences } from '@/utils/userPreferences'
import SessionWorkflowPanel from '@/components/SessionWorkflowPanel.vue'
import ToolCallCard from '@/components/ToolCallCard.vue'
import ToolCallGroup from '@/components/ToolCallGroup.vue'
import ChoiceCard from '@/components/ChoiceCard.vue'
import SecretInputCard from '@/components/SecretInputCard.vue'
import { secretRequestsForMessage } from '@/utils/secretInput'
import MessageArtifacts from '@/components/MessageArtifacts.vue'
import CommandPalette from '@/components/CommandPalette.vue'
import { botSkillApi } from '@/api/services'
import { MAX_CHAT_ATTACHMENT_BYTES, MAX_CHAT_ATTACHMENT_COUNT, MAX_CHAT_ATTACHMENT_TOTAL_BYTES } from '@/constants/uploadLimits'

// markdown 渲染统一走 utils/markdown：
// 内置渲染缓存（模板每帧都会重新调用，历史消息不必反复 parse + sanitize）
// 与收紧后的净化策略（禁 javascript: 等伪协议、外链强制 noopener）。
const store = useBotStore()
const layout = useLayoutStore()
const sessionTitle = computed(() => store.sessions.find(s => String(s.id) === String(store.activeSessionId))?.title || '新会话')
const userStore = useUserStore()
const userPreferences = computed(() => loadUserPreferences(userStore.user?.id))
const inputPlaceholder = computed(() => userPreferences.value.sendKey === 'cmd-enter'
  ? '有问题尽管问，输入 / 使用命令，⌘ / Ctrl + Enter 发送'
  : '有问题尽管问，输入 / 使用命令')
const inputAriaLabel = computed(() => userPreferences.value.sendKey === 'cmd-enter'
  ? '消息输入框，输入斜杠打开命令，Command 或 Control 加 Enter 发送，Enter 换行'
  : '消息输入框，输入斜杠打开命令，Enter 发送，Shift 加 Enter 换行')
const draft = computed({ get: () => store.composerDraft.text, set: value => { store.composerDraft.text = value } })
const sendKeyHint = computed(() => userPreferences.value.sendKey === 'cmd-enter' ? '⌘ / Ctrl + Enter 发送 · Enter 换行' : 'Enter 发送 · Shift + Enter 换行')
const composerHint = computed(() => store.replying && attachments.value.length ? '附件已保留，任务结束后可发送' : store.replying ? '可补充当前任务的要求' : '输入 / 使用命令')
const canSend = computed(() => Boolean(store.activeBot) && !store.messagesLoading && !store.sessionsLoading && !store.composerDraft.pending
  && !attachments.value.some(a => a.status === 'reading' || a.status === 'error')
  && attachments.value.length <= MAX_ATTACH_COUNT
  && (store.replying ? Boolean(draft.value.trim()) && !attachments.value.length : Boolean(draft.value.trim() || attachments.value.length)))
const copiedMessageId = ref(null)
const copyError = ref('')
let copyTimer = null
const codeCopyButtons = new WeakSet()
const codeCopyTimers = new Set()
function decorateCodeBlocks() {
  for (const pre of scrollRef.value?.querySelectorAll('.markdown-body pre') || []) {
    if (!pre.querySelector('code') || [...pre.children].some(child => codeCopyButtons.has(child))) continue
    const button = document.createElement('button')
    button.type = 'button'
    button.className = 'code-copy-button'
    button.dataset.testid = 'chat-code-copy'
    button.setAttribute('aria-label', '复制代码')
    button.textContent = '复制代码'
    codeCopyButtons.add(button)
    pre.appendChild(button)
  }
}
async function onMessageBodyClick(event) {
  const button = event.target.closest?.('button')
  if (!button || !codeCopyButtons.has(button)) return
  const code = button.closest('pre')?.querySelector('code')
  if (!code) return
  try {
    await navigator.clipboard.writeText(code.textContent || '')
    button.textContent = '已复制'
    button.setAttribute('aria-label', '代码已复制')
    copyError.value = ''
    const timer = setTimeout(() => {
      button.textContent = '复制代码'
      button.setAttribute('aria-label', '复制代码')
      codeCopyTimers.delete(timer)
    }, 2000)
    codeCopyTimers.add(timer)
  } catch { copyError.value = '代码复制失败，请选择代码后手动复制。' }
}
onUpdated(decorateCodeBlocks)
function messageRunNotice(msg) {
  if (msg.role !== 'assistant') return null
  const status = msg.streamStatus || msg.status
  if (msg.streamError) return { tone: ['error', 'failed'].includes(status) ? 'error' : 'warning', text: msg.streamError }
  if (status === 'error' || status === 'failed') return { tone: 'error', text: '本次任务执行失败，请检查已有结果。' }
  if (status === 'cancelled') return { tone: 'neutral', text: '任务已停止，已生成的内容保留在此。' }
  if (status === 'dropped') return { tone: 'neutral', text: '本轮未执行。' }
  return null
}
function copyableText(msg) {
  return msg.content || (msg.parts || []).filter(p => p.type === 'text').map(p => p.content || '').join('\n\n')
}
async function copyMessage(msg) {
  try {
    await navigator.clipboard.writeText(copyableText(msg))
    copyError.value = ''
    copiedMessageId.value = msg.id
    clearTimeout(copyTimer)
    copyTimer = setTimeout(() => { copiedMessageId.value = null }, 2000)
  } catch {
    copyError.value = '复制失败，请选择消息文本后手动复制。'
  }
}
function messageTime(msg, full = false) {
  if (!msg.createdAt) return ''
  const date = new Date(msg.createdAt)
  if (Number.isNaN(date.getTime())) return ''
  return full ? date.toLocaleString('zh-CN') : date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
}
function isStreamingMessage(msg) { return store.replying && msg.role === 'assistant' && (msg._temp || msg.streaming) }
const scrollRef = ref()
const fileInputRef = ref()
const composerRef = ref(null)

const BUILTIN_COMMANDS = [
  { id: 'cmd-help', name: 'help', description: '显示可用命令列表', icon: 'help-circle', category: '命令' },
  { id: 'cmd-clear', name: 'clear', description: '清空当前会话上下文', icon: 'delete', category: '命令' },
  { id: 'cmd-compact', name: 'compact', description: '压缩当前会话上下文（保留最近 N 条消息）', icon: 'folder-zip', category: '命令' },
  { id: 'cmd-status', name: 'status', description: '显示当前会话状态', icon: 'info-circle', category: '命令' },
]

function truncateDesc(text, max = 40) {
  const t = String(text || '').trim().replace(/\s+/g, ' ')
  if (!t) return ''
  if (t.length <= max) return t
  return t.slice(0, max - 1) + '…'
}

const skillItems = ref([])
const slashDismissed = ref(false)
const slashIndex = ref(0)

/** Slash palette opens only for a leading `/` with no newline and no args yet. */
const slashQuery = computed(() => {
  const text = draft.value
  if (!/^\/[^\n]*$/.test(text)) return null
  const m = text.match(/^\/(\S*)$/)
  return m ? m[1] : null
})
const slashOpen = computed(() => slashQuery.value !== null && !slashDismissed.value)

const slashItems = computed(() => {
  const q = (slashQuery.value || '').toLowerCase()
  const skills = skillItems.value.filter((s) => s && s.name)
  const all = [...BUILTIN_COMMANDS, ...skills]
  if (!q) return all
  return all.filter((item) => String(item.name).toLowerCase().startsWith(q))
})
const slashActiveId = computed(() => {
  if (!slashOpen.value) return undefined
  const item = slashItems.value[slashIndex.value]
  return item ? 'cmd-opt-' + item.id : undefined
})

function dismissSlash() {
  slashDismissed.value = true
}

async function loadBotSkills(botId) {
  skillItems.value = []
  if (!botId) return
  try {
    const res = await botSkillApi.list(botId)
    if (botId !== store.activeBotId) return
    const list = Array.isArray(res?.skills) ? res.skills : []
    skillItems.value = list
      .filter((s) => s && String(s.name || '').trim())
      .map((s) => ({
        id: 'skill-' + (s.id || s.name),
        name: String(s.name).trim(),
        description: truncateDesc(s.description || ''),
        icon: 'extension',
        category: '技能',
      }))
  } catch {
    if (botId === store.activeBotId) skillItems.value = []
  }
}

watch(slashQuery, (q, prev) => {
  if (q === null) slashDismissed.value = false
  if (q !== prev) slashIndex.value = 0
})
watch(slashItems, (items) => {
  if (slashIndex.value >= items.length) {
    slashIndex.value = Math.max(0, items.length - 1)
  }
})
watch(() => store.activeBotId, (id) => { loadBotSkills(id) }, { immediate: true })

function onSlashSelect(item) {
  if (!item || !item.name) return
  draft.value = '/' + item.name
  slashDismissed.value = true
  nextTick(() => send())
}

function completeSlash(item) {
  if (!item || !item.name) return
  draft.value = '/' + item.name
}

function onDocPointerDown(e) {
  if (!slashOpen.value) return
  const root = composerRef.value
  if (root && !root.contains(e.target)) dismissSlash()
}
onMounted(() => {
  document.addEventListener('pointerdown', onDocPointerDown, true)
  decorateCodeBlocks()
})

// ── 智能滚动：用户在底部才自动滚，上翻时不干扰 ──
const isAtBottom = ref(true)
const hasUnreadContent = ref(false)
const restoringScroll = ref(false)
let restoreGeneration = 0
function captureViewport(key = store.draftKey, switching = false) {
  const container = scrollRef.value
  if (!container || restoringScroll.value || (!switching && store.messagesLoading) || !key) return
  const top = container.getBoundingClientRect().top
  const rows = [...container.querySelectorAll('[data-message-id]')]
  const anchor = rows.find(row => row.getBoundingClientRect().bottom > top + 8)
  if (!anchor) return
  store.getDraft(key).viewport = { anchorId: anchor.dataset.messageId, offset: anchor.getBoundingClientRect().top - top, scrollTop: container.scrollTop, atBottom: isAtBottom.value }
}
function cancelScrollRestore() {
  restoreGeneration++
  restoringScroll.value = false
}
function onReadingKeydown(event) {
  if (['PageUp', 'PageDown', 'Home', 'End', 'ArrowUp', 'ArrowDown', ' '].includes(event.key)) cancelScrollRestore()
}
async function restoreViewport() {
  const scope = store.draftKey
  const saved = store.getDraft(scope).viewport
  const generation = ++restoreGeneration
  if (!saved || saved.atBottom) {
    restoringScroll.value = false
    isAtBottom.value = true
    scrollToBottom()
    return
  }
  restoringScroll.value = true
  isAtBottom.value = false
  await nextTick()
  const current = () => generation === restoreGeneration && scope === store.draftKey
  const findAnchor = () => [...(scrollRef.value?.querySelectorAll('[data-message-id]') || [])].find(row => row.dataset.messageId === saved.anchorId)
  let anchor = findAnchor()
  // A previously read page may lie before the freshly loaded latest page.
  // Restore its message anchor without holding an unbounded transcript cache.
  for (let page = 0; !anchor && store.hasMore && page < 12 && current(); page++) {
    const added = await store.loadMoreMessages()
    await nextTick()
    if (!added) break
    anchor = findAnchor()
  }
  if (!current()) return
  const container = scrollRef.value
  if (container) container.scrollTop = anchor
    ? container.scrollTop + anchor.getBoundingClientRect().top - container.getBoundingClientRect().top - saved.offset
    : saved.scrollTop
  restoringScroll.value = false
  checkAtBottom()
}

let scrollFrame = null
const SCROLL_THRESHOLD = 120 // 距底部多少 px 内视为"在底部"

/** 判断当前滚动位置是否接近底部 */
function checkAtBottom() {
  const el = scrollRef.value
  if (!el || restoringScroll.value) return
  isAtBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight < SCROLL_THRESHOLD
  if (isAtBottom.value) hasUnreadContent.value = false
}

/** 立即把容器贴到底部（单次尝试） */
function forceScrollBottom() {
  const el = scrollRef.value
  if (!el || !isAtBottom.value) return
  el.scrollTop = el.scrollHeight
}

/**
 * 滚动到底部（供外部调用）。
 * 单次 nextTick 不可靠：首屏消息含 markdown/代码块/图片等异步内容，
 * 渲染完成前 scrollHeight 偏小，设完 scrollTop 后内容继续撑高会把视图留在中上部。
 * 因此在 nextTick + requestAnimationFrame 重试，并始终复核用户是否仍在底部。
 */
function scrollToBottom() {
  nextTick(() => {
    if (!isAtBottom.value) return
    forceScrollBottom()
    cancelAnimationFrame(scrollFrame)
    scrollFrame = requestAnimationFrame(forceScrollBottom)
  })
}

// ── 上翻分页：滚到顶部附近自动加载更早的一页（每页 20 条）──
const loadingPreviousPage = ref(false)
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
  loadingPreviousPage.value = true
  const scope = store.draftKey
  const added = await store.loadMoreMessages()
  if (!added || scope !== store.draftKey) { loadingPreviousPage.value = false; return }
  await nextTick()
  const cur = scrollRef.value
  if (!cur) { loadingPreviousPage.value = false; return }
  // 补偿新增内容的高度，保持原来那条消息仍停在同一视觉位置
  cur.scrollTop = cur.scrollHeight - prevHeight + prevTop
  loadingPreviousPage.value = false
}

/** 用户手动滚动时更新 atBottom 状态，并在接近顶部时触发上翻分页 */
function onScroll() {
  if (restoringScroll.value) return
  checkAtBottom()
  captureViewport()
  const el = scrollRef.value
  if (!el) return
  if (el.scrollTop < LOAD_MORE_THRESHOLD && store.hasMore && !store.loadingMore && !store.messagesLoading && !store.loadMoreError) {
    triggerLoadMore()
  }
}

/** 点击"回到底部"按钮 */
function scrollToBottomManual() {
  cancelScrollRestore()
  isAtBottom.value = true
  hasUnreadContent.value = false
  scrollToBottom()
}
const attachments = computed({ get: () => store.composerDraft.attachments, set: value => { store.composerDraft.attachments = value } })  // { name, type, size, dataUrl (base64) }[]

// 附件上限与后端 validateChatAttachments 一致：单文件 20MB、单次总大小 60MB、最多 10 个。
// 这是 base64 JSON，不是工作区文件上传（后者见 uploadLimits.js 的 100MB）。
// 仅单文件限制时，用户批量塞入大文件会让 base64 全量驻留内存直至发送。
const MAX_ATTACH_FILE_BYTES = MAX_CHAT_ATTACHMENT_BYTES
const MAX_ATTACH_TOTAL_BYTES = MAX_CHAT_ATTACHMENT_TOTAL_BYTES
const MAX_ATTACH_COUNT = MAX_CHAT_ATTACHMENT_COUNT

/** 根据 MIME type 返回图标名 */
function fileIcon(mimeType) {
  if (mimeType?.startsWith('image')) return 'image'
  if (mimeType?.startsWith('audio')) return 'voice'
  if (mimeType?.startsWith('video')) return 'video-play'
  if (mimeType === 'application/pdf') return 'file-pdf'
  return 'file-unknown'
}

/** 文件选择 → 读为 base64 data URL */
const draggingFiles = ref(false)
let fileDragDepth = 0
const isFileDrag = event => Array.from(event.dataTransfer?.types || []).includes('Files')
function onDragEnter(event) {
  if (!isFileDrag(event)) return
  event.preventDefault()
  fileDragDepth++
  draggingFiles.value = true
}
function onDragOver(event) {
  if (!isFileDrag(event)) return
  event.preventDefault()
  event.dataTransfer.dropEffect = 'copy'
}
function onDragLeave(event) {
  if (!isFileDrag(event) && !draggingFiles.value) return
  fileDragDepth = Math.max(0, fileDragDepth - 1)
  if (!fileDragDepth) draggingFiles.value = false
}
function onFileDrop(event) {
  if (!isFileDrag(event)) return
  event.preventDefault()
  draggingFiles.value = false
  fileDragDepth = 0
  addAttachments(Array.from(event.dataTransfer?.files || []))
  composerRef.value?.querySelector('textarea')?.focus()
}
function onPaste(event) {
  const files = Array.from(event.clipboardData?.files || [])
  if (!files.length) return
  if (!event.clipboardData.getData('text/plain')) event.preventDefault()
  addAttachments(files)
}
async function onFileSelect(event) {
  await addAttachments(Array.from(event.target.files || []))
  event.target.value = ''
}
async function addAttachments(files) {
  const target = store.getDraft(store.draftKey)
  if (!store.activeBot) { target.notice = '请先选择一个 Bot，再添加附件'; return }
  const pending = []
  // Reserve every selected file synchronously so concurrent paste/drop actions
  // share the same size/count limits, even while FileReader is still running.
  for (const file of files) {
    if (target.attachments.length >= MAX_ATTACH_COUNT) { target.notice = `最多只能添加 ${MAX_ATTACH_COUNT} 个附件`; break }
    if (file.size > MAX_ATTACH_FILE_BYTES) { target.notice = `文件「${file.name}」超过 ${formatFileSize(MAX_ATTACH_FILE_BYTES)} 上限，已跳过`; continue }
    const total = target.attachments.reduce((sum, attachment) => sum + (attachment.size || 0), 0)
    if (total + file.size > MAX_ATTACH_TOTAL_BYTES) { target.notice = `附件总大小不能超过 ${formatFileSize(MAX_ATTACH_TOTAL_BYTES)}`; break }
    const attachment = { id: `${Date.now()}-${Math.random()}`, name: file.name, type: file.type || guessType(file.name), size: file.size, dataUrl: '', status: 'reading' }
    target.attachments.push(attachment)
    pending.push({ file, attachment })
  }
  for (const { file, attachment } of pending) {
    try {
      const dataUrl = await readFileAsDataURL(file)
      const saved = target.attachments.find(item => item.id === attachment.id)
      if (saved) { saved.dataUrl = dataUrl; saved.status = 'ready' }
    } catch {
      const saved = target.attachments.find(item => item.id === attachment.id)
      if (saved) saved.status = 'error'
      target.notice = `文件「${file.name}」读取失败，请移除后重新添加`
    }
  }
}

function formatFileSize(size) { return size < 1024 * 1024 ? `${Math.max(1, Math.round(size / 1024))} KB` : `${(size / 1024 / 1024).toFixed(1)} MB` }

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

/** 当前 text part 是否是渲染列表中最后一个可见文本（用于流式光标位置）。
 *  必须按 renderParts 下标判断：连续同名工具会归并，渲染列表短于 msg.parts，
 *  用 parts 下标会对错块或丢光标。
 */
function isLastTextPart(msg, pi) {
  const list = renderParts(msg)
  for (let i = pi + 1; i < list.length; i++) {
    if (list[i].type === 'text') return false
    if (list[i].type === 'tool') return false
  }
  return true
}

/** v-for 稳定 key：工具用 call id，文本用 part.id，归并用 group-name-firstId。 */
function renderPartKey(part, pi) {
  if (!part) return 'idx-' + pi
  if (part.type === 'text') return part.id || ('text-' + pi)
  if (part._group) {
    const firstId = part._group[0] && part._group[0].id
    return 'group-' + (part.name || 'tool') + '-' + (firstId || pi)
  }
  return part.id || ('tool-' + pi)
}

function isChoiceToolName(name) {
  return name === 'user_choice' || name === 'sandbox_user_choice'
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
      // user_choice 必须保持单卡，ChoiceCard 才能紧挨对应调用；归并会把选择题堆到组尾。
      if (isChoiceToolName(p.name)) {
        if (groupAcc) { flushGroup(out, groupAcc); groupAcc = null }
        out.push(p)
        continue
      }
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


/** 工具 part（单卡或归并组）上挂着的 user_choice questionId，供内联 ChoiceCard */
function choiceIdsForToolPart(part) {
  if (!part) return []
  const calls = Array.isArray(part._group) && part._group.length ? part._group : [part]
  const ids = []
  for (const c of calls) {
    const qid = store.choiceIdByToolCallId(c && c.id)
    if (qid) ids.push(qid)
  }
  return ids
}

// ── 降级：旧消息无 parts 时仍用此函数对 toolCalls 归并分组 ──
function groupToolCalls(calls) {
  const list = Array.isArray(calls) ? calls : []
  const groups = []
  for (const c of list) {
    if (isChoiceToolName(c && c.name)) {
      groups.push({ type: 'single', call: c })
      continue
    }
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

// 最后一条助手消息的 id：当 activeWorkflowId 存在但没有任何消息被锚定 workflowId 时
// （刷新后历史会话、创建工作流的那条消息在未加载的分页页里），工作流面板兜底渲染到此处，
// 保证已完成/历史工作流在刷新后仍然可见。
const lastAssistantMsgId = computed(() => {
  const list = messages.value
  for (let i = list.length - 1; i >= 0; i--) {
    if (list[i].role === 'assistant') return list[i].id
  }
  return null
})

const workflowAnchorMsgId = computed(() => {
  const wid = store.activeWorkflowId
  if (!wid) return null
  const list = messages.value
  for (let i = list.length - 1; i >= 0; i--) {
    if (list[i].role === 'assistant' && list[i].workflowId === wid) return list[i].id
  }
  return lastAssistantMsgId.value
})

// 选择卡（user_choice）：按 questionId 取该题的实时状态（payload/终态/提交标记）。
// 与工作流面板同一模式——渲染层只读 store，不持有副本。
const choiceState = (qid) => store.choiceState(qid)

/** 过滤出无法关联到 toolCallId 的遗留选择卡（历史恢复 / toolCallId 丢失时兜底渲染） */
function orphanQuestionIds(msg) {
  if (!msg.questionIds?.length) return []
  // 45bf51b：toolCalls ∪ parts 都能把选择题绑到对应卡片，避免刷新后落到消息底部。
  const tcIds = new Set()
  for (const tc of (msg.toolCalls || [])) {
    if (tc && tc.id != null) tcIds.add(String(tc.id))
  }
  for (const p of (msg.parts || [])) {
    if (p && p.type === 'tool' && p.id != null) tcIds.add(String(p.id))
  }
  // 已经由 choiceIdsForToolPart 内联渲染的 qid 不得再以 orphan 出现（双卡）。
  const inlineQids = new Set()
  for (const part of renderParts(msg)) {
    if (part && part.type === 'tool') {
      for (const qid of choiceIdsForToolPart(part)) inlineQids.add(qid)
    }
  }
  return msg.questionIds.filter(qid => {
    if (inlineQids.has(qid)) return false
    const c = store.choiceState(qid)
    const linked = c?.toolCallId != null && String(c.toolCallId) !== '' && tcIds.has(String(c.toolCallId))
    return !linked
  })
}

function choiceCardStatus(qid, part) {
  const c = store.choiceState(qid)
  if (c?.status) return c.status
  const calls = (part && Array.isArray(part._group) && part._group.length) ? part._group : (part ? [part] : [])
  const call = calls.find(x => store.choiceIdByToolCallId(x && x.id) === qid) || calls[0]
  const st = call && call.status
  if (st === 'timeout') return 'timeout'
  if (st === 'killed' || st === 'cancelled') return 'cancelled'
  if (st === 'error') return 'error'
  if (st === 'success' || st === 'answered' || st === 'resolved') return 'answered'
  return ''
}

const choiceReminderNow = ref(Date.now())
const waitingChoices = computed(() => {
  const terminal = new Set(['success', 'answered', 'resolved', 'error', 'timeout', 'cancelled', 'killed', 'ended', 'superseded'])
  return [...store.choices.entries()].filter(([id, choice]) => {
    if (!String(id).startsWith('uc-') || choice.submitted || choice.payload?.pending || terminal.has(choice.status)) return false
    const deadline = choice.payload?.timeoutAt
    const timeoutAt = typeof deadline === 'number' ? (deadline < 1e12 ? deadline * 1000 : deadline) : Date.parse(deadline)
    if (Number.isFinite(timeoutAt) && timeoutAt <= choiceReminderNow.value) return false
    const message = messages.value.find(item => item.questionIds?.includes(id))
    if (!message) return false
    const call = [...(message.toolCalls || []), ...(message.parts || [])].find(item => item.id === choice.toolCallId)
    return !terminal.has(call?.status)
  }).map(([id, choice]) => ({ id, question: choice.payload?.question || '' }))
})
let choiceReminderTimer = null
watch(() => waitingChoices.value.length, count => {
  if (count && !choiceReminderTimer) choiceReminderTimer = setInterval(() => { choiceReminderNow.value = Date.now() }, 1000)
  if (!count && choiceReminderTimer) { clearInterval(choiceReminderTimer); choiceReminderTimer = null }
}, { immediate: true })
function goToChoice(id) {
  const container = scrollRef.value
  const card = [...(container?.querySelectorAll('[data-choice-id]') || [])].find(node => node.dataset.choiceId === id)
  if (!card || !container) return
  cancelScrollRestore()
  isAtBottom.value = false
  container.scrollTo({ top: container.scrollTop + card.getBoundingClientRect().top - container.getBoundingClientRect().top - 32, behavior: window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' })
  card.querySelector('button:not([disabled]), input:not([disabled])')?.focus({ preventScroll: true })
}

// 新的选择题下发时（questionIds 首次出现），若用户停在底部则跟随滚底，
// 避免卡片落在视口之外；用户上翻阅读时不打扰（与 workflowId watcher 同策略）。
watch(() => store.choices, () => {
  if (isAtBottom.value && store.replying) scrollToBottomIfAtBottom()
}, { deep: true })

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
  if (isAtBottom.value && !restoringScroll.value) scrollToBottom()
}

// 消息列表长度变化时（新消息到达）智能滚动
watch(() => {
  const latest = messages.value.at(-1)
  return [latest?.id, latest?.content, latest?.parts?.length, latest?.toolCalls?.length]
}, () => {
  if (store.messagesLoading || store.loadingMore || loadingPreviousPage.value || restoringScroll.value) return
  if (!isAtBottom.value) hasUnreadContent.value = true
  scrollToBottomIfAtBottom()
})

// 工作流面板内联锚定在创建它的那一轮助手消息之后；当它首次出现时若用户已在底部，
// 跟随滚到底部以免新面板落在视口下方（用户上翻阅读时不打扰）。
watch(sessionWorkflowId, (id) => {
  if (id) scrollToBottomIfAtBottom()
})
// 保存旧会话的阅读锚点，首访跟随最新，返回时恢复阅读位置。
watch(() => store.draftKey, (_key, previousKey) => {
  captureViewport(previousKey, true)
  cancelScrollRestore()
  slashDismissed.value = false
  hasUnreadContent.value = false
  const saved = store.composerDraft.viewport
  isAtBottom.value = saved?.atBottom ?? true
  restoringScroll.value = Boolean(saved && !saved.atBottom)
})
// 首屏加载完成：首访看最新，返回已访问会话则按消息锚点恢复。
watch(() => store.scrollToBottomOnLoad, (v) => {
  if (!v) return
  store.scrollToBottomOnLoad = false
  restoreViewport()
})

// 流式期间：仅在用户在底部时才持续跟滚；用户上翻即暂停
watch(() => store.replying, (val) => {
  if (val) {
    // 流式开始：启动智能定时滚动
    _scrollTimer = setInterval(scrollToBottomIfAtBottom, 200)
  } else {
    // 流式结束：停止定时器，仅在仍位于底部时跟随。
    clearInterval(_scrollTimer)
    _scrollTimer = null
    scrollToBottomIfAtBottom()
  }
})

let _scrollTimer = null

// 组件卸载时清理定时器，防止内存泄漏
onUnmounted(() => {
  captureViewport()
  cancelScrollRestore()
  document.removeEventListener('pointerdown', onDocPointerDown, true)
  for (const timer of codeCopyTimers) clearTimeout(timer)
  clearInterval(choiceReminderTimer)
  clearTimeout(copyTimer)
  cancelAnimationFrame(scrollFrame)
  if (_scrollTimer) {
    clearInterval(_scrollTimer)
    _scrollTimer = null
  }
})

function send() {
  if (!canSend.value) return
  const text = draft.value.trim()
  const target = store.getDraft(store.draftKey)
  const attachPayload = attachments.value.map(a => ({ name: a.name, type: a.type, size: a.size, dataUrl: a.dataUrl }))
  const result = store.replying ? store.appendToCurrentReply(text) : store.sendMessage(text || '[附件]', attachPayload)
  if (!result?.accepted) {
    target.notice = result?.reason || '消息尚未发送，内容已保留'
    return
  }
  target.text = ''
  target.attachments = []
  target.notice = ''
  scrollToBottomManual()
}

function quickSend(text) {
  if (draft.value.trim() || attachments.value.length) {
    draft.value = [draft.value, text].filter(Boolean).join('\n')
    return
  }
  draft.value = text
  send()
}

function onKeydown(value, ctx) {
  const e = (ctx && ctx.e) || ctx || value
  if (!e || !e.key) return
  if (e.isComposing || e.keyCode === 229) return

  if (slashOpen.value) {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      if (slashIndex.value < slashItems.value.length - 1) slashIndex.value += 1
      return
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault()
      if (slashIndex.value > 0) slashIndex.value -= 1
      return
    }
    if (e.key === 'Tab') {
      e.preventDefault()
      const item = slashItems.value[slashIndex.value]
      if (item) completeSlash(item)
      return
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      dismissSlash()
      return
    }
    if (e.key === 'Enter') {
      e.preventDefault()
      const item = slashItems.value[slashIndex.value]
      if (item) onSlashSelect(item)
      return
    }
  }

  if (e.key !== 'Enter') return

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
  position: relative;
  display: flex;
  flex-direction: column;
  background: var(--bp-chat-bg, #fafbfe);
  min-width: 0;
  min-height: 0;
}
.chat-topbar {
  height: 68px;
  flex-shrink: 0;
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 0 24px;
  background: var(--bp-surface);
  border-bottom: var(--bp-hairline);
  z-index: 2;
}
.chat-topbar :deep(.t-tag) {
  background: transparent;
  border: none;
  color: var(--bp-label-secondary);
  font-weight: 450;
  padding: 0;
}
.topbar-title {
  font-size: 15px;
  font-weight: 600;
  letter-spacing: var(--bp-tracking-title);
  color: var(--bp-label);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.chat-body {
  flex: 1;
  overflow-y: auto;
  padding: 28px clamp(16px, 4vw, 40px) 20px;
  overscroll-behavior: contain;
  scroll-padding-bottom: 24px;
  position: relative;
}
.scroll-to-bottom-btn {
  position: absolute;
  bottom: 12px;
  left: 50%;
  transform: translateX(-50%);
  width: max-content;
  max-width: 90%;
  height: 34px;
  border-radius: var(--bp-radius-pill);
  background: var(--bp-surface);
  border: none;
  box-shadow: var(--bp-shadow-md);
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  color: var(--bp-label-secondary);
  font-size: 12px;
  gap: 6px;
  padding: 0 14px;
  transition: transform 140ms var(--bp-ease-out),
              opacity 140ms var(--bp-ease-out),
              color 140ms ease,
              background 140ms ease;
  z-index: 10;
}
@media (hover: hover) and (pointer: fine) {
  .scroll-to-bottom-btn:hover {
    background: var(--bp-accent);
    color: var(--bp-label-on-accent);
  }
}
.scroll-to-bottom-btn:active {
  transform: translateX(-50%) scale(0.97);
}
.scroll-to-bottom-btn:focus-visible {
  outline: 2px solid var(--bp-accent);
  outline-offset: 2px;
}
.scroll-bottom-fade-enter-active,
.scroll-bottom-fade-leave-active {
  transition: opacity var(--bp-duration) var(--bp-ease-out),
              transform var(--bp-duration) var(--bp-ease-out);
}
.scroll-bottom-fade-enter-from,
.scroll-bottom-fade-leave-to {
  opacity: 0;
  transform: translateX(-50%) translateY(8px);
}
.wf-inline {
  max-width: 800px;
  margin: 18px auto 0;
  padding: 0;
}
/* 选择卡与工作流卡片同容器约定；多题连排时收紧间距，视觉上成组 */
.cc-inline {
  margin-top: 0;
  width: 100%;
  display: block;
}
.cc-inline + .cc-inline {
  margin-top: 10px;
}
.wf-inline + .cc-inline,
.cc-inline + .wf-inline {
  margin-top: 10px;
}
.load-more-bar {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  padding: 10px 0 14px;
  font-size: 13px;
  letter-spacing: 0;
  color: var(--bp-label-secondary);
}
.load-more-loading {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}
.load-more-btn {
  background: var(--bp-surface-fill);
  border: none;
  border-radius: var(--bp-radius-pill);
  padding: 5px 14px;
  font-size: 13px;
  color: var(--bp-label-secondary);
  cursor: pointer;
  transition: background 140ms ease,
              color 140ms ease,
              transform 140ms var(--bp-ease-out);
}
@media (hover: hover) and (pointer: fine) {
  .load-more-btn:hover {
    background: var(--bp-accent-soft);
    color: var(--bp-accent);
  }
}
.load-more-btn:active {
  transform: scale(0.97);
}
.load-more-btn:focus-visible {
  outline: 2px solid var(--bp-accent);
  outline-offset: 2px;
}
.load-more-end {
  color: var(--bp-label-secondary);
}
.msg-row {
  display: flex;
  padding: 2px 0;
  max-width: 800px;
  margin: 0 auto;
}
.msg-row.assistant {
  flex-direction: column;
  align-items: stretch;
  margin-top: 32px;
}
.msg-header {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}
.msg-header-avatar {
  width: 22px;
  height: 22px;
  border-radius: 50%;
  background: transparent;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 14px;
  flex-shrink: 0;
}
.msg-header-name {
  font-size: 13px;
  font-weight: 500;
  letter-spacing: 0;
  color: var(--bp-label-secondary);
}
.msg-row.user {
  justify-content: flex-end;
  margin-top: 22px;
}
.msg-bubble {
  font-size: 15px;
  line-height: 1.7;
  letter-spacing: 0;
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
  max-width: 78%;
  align-items: flex-end;
}
.msg-toolcalls,
.msg-toolcall-item {
  width: 100%;
  margin: 0;
}
.msg-row.user .msg-bubble {
  background: var(--bp-accent-soft);
  color: var(--bp-label);
  border-radius: 16px;
  padding: 12px 16px;
}
.msg-row.assistant .msg-bubble {
  background: transparent;
  color: var(--bp-label);
  padding: 0;
  width: 100%;
}
.markdown-body {
  white-space: normal;
  font-size: 15px;
  line-height: 1.85;
  letter-spacing: 0;
  word-break: break-word;
  padding: 4px 0 2px;
}
.markdown-body :deep(h1),
.markdown-body :deep(h2),
.markdown-body :deep(h3),
.markdown-body :deep(h4) {
  font-weight: 600;
  line-height: var(--bp-leading-title);
  letter-spacing: var(--bp-tracking-title);
  margin: 16px 0 8px;
  color: var(--bp-label);
}
.markdown-body :deep(h1) { font-size: 20px; letter-spacing: var(--bp-tracking-display); }
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
  border-top: var(--bp-hairline);
  margin: 16px 0;
}
.markdown-body :deep(blockquote) {
  margin: 8px 0;
  padding: 2px 12px;
  border-left: 3px solid var(--bp-separator-opaque);
  color: var(--bp-label-secondary);
}
.markdown-body :deep(code) {
  background: var(--bp-surface-fill);
  padding: 2px 5px;
  border-radius: var(--bp-radius-xs);
  font-size: 13px;
  font-family: var(--bp-font-mono);
}
.markdown-body :deep(pre) {
  background: var(--bp-surface-fill);
  padding: 12px 14px;
  border-radius: var(--bp-radius-md);
  overflow-x: auto;
  margin: 10px 0;
}
.markdown-body :deep(pre code) {
  background: transparent;
  padding: 0;
  font-size: 13px;
}
.markdown-body :deep(a) { color: var(--bp-accent); text-decoration: none; }
.markdown-body :deep(a:hover) { text-decoration: underline; }
.markdown-body :deep(table) {
  border-collapse: collapse;
  margin: 10px 0;
  display: block;
  max-width: 100%;
  overflow-x: auto;
}
.markdown-body :deep(th),
.markdown-body :deep(td) {
  border: var(--bp-hairline);
  padding: 6px 10px;
  text-align: left;
}
.markdown-body :deep(> *:first-child) { margin-top: 0; }
.markdown-body :deep(> *:last-child) { margin-bottom: 0; }
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
  background: var(--bp-accent);
  animation: chat-blink 1.2s infinite ease-in-out;
}
.typing-indicator i:nth-child(2) { animation-delay: 0.2s; }
.typing-indicator i:nth-child(3) { animation-delay: 0.4s; }
.typing-text {
  color: var(--bp-label-secondary);
  font-size: 13px;
  letter-spacing: 0;
}
.append-unconfirmed {
  display: inline-block;
  margin-left: 6px;
  padding: 0 6px;
  border-radius: 8px;
  font-size: 11px;
  line-height: 17px;
  color: var(--bp-warning);
  background: var(--bp-warning-soft);
  vertical-align: middle;
}
.stream-caret {
  display: inline-block;
  width: 2px;
  height: 14px;
  margin-left: 2px;
  background: var(--bp-accent);
  vertical-align: text-bottom;
  animation: chat-caret 1s steps(1) infinite;
}
@keyframes chat-blink {
  0%, 80%, 100% { opacity: 0.35; }
  40% { opacity: 1; }
}
@keyframes chat-caret {
  0%, 50% { opacity: 1; }
  50.01%, 100% { opacity: 0; }
}
.empty-greeting {
  min-height: 100%;
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
  letter-spacing: var(--bp-tracking-display);
  line-height: var(--bp-leading-title);
  color: var(--bp-label);
}
.greet-sub {
  margin-top: 8px;
  color: var(--bp-label-secondary);
  font-size: 13px;
  letter-spacing: var(--bp-tracking-caption);
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
  background: var(--bp-surface);
  box-shadow: var(--bp-shadow-sm);
  border: none;
  border-radius: var(--bp-radius-pill);
  font: inherit;
  font-size: 13px;
  letter-spacing: 0;
  color: var(--bp-label);
  cursor: pointer;
  transition: transform 140ms var(--bp-ease-out),
              background 140ms ease,
              color 140ms ease;
}
@media (hover: hover) and (pointer: fine) {
  .greet-chip:hover {
    background: var(--bp-accent-soft);
    color: var(--bp-accent);
  }
}
.greet-chip:active {
  transform: scale(0.97);
}
.greet-chip:focus-visible {
  outline: 2px solid var(--bp-accent);
  outline-offset: 2px;
}
.chat-input-area {
  flex-shrink: 0;
  padding: 12px clamp(16px, 4vw, 40px) 12px;
  background: var(--bp-chat-bg, #fafbfe);
}
.composer {
  position: relative;
  max-width: 800px;
  margin: 0 auto;
}
.input-box {
  border-radius: 20px;
  padding: 12px 14px 8px;
  background: var(--bp-surface);
  box-shadow: 0 4px 20px rgba(30, 45, 80, 0.035);
  border: 1px solid rgba(60, 60, 67, 0.12);
  transition: border-color var(--bp-duration) var(--bp-ease-out);
}
.input-box:focus-within {
  border-color: var(--bp-accent);
  box-shadow: 0 0 0 3px var(--bp-accent-soft);
}
.composer-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 2px;
  min-height: 36px;
}
.composer-model {
  margin-left: auto;
  max-width: 46%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 12px;
  color: var(--bp-label-secondary);
  letter-spacing: 0;
}
.input-box :deep(.t-textarea) {
  width: 100%;
}
.input-box :deep(.t-textarea__inner) {
  font-size: 15px;
  line-height: 1.55;
  letter-spacing: 0;
  padding: 2px 4px 6px;
  resize: none;
  background: transparent !important;
  border: none !important;
  box-shadow: none !important;
  color: var(--bp-label);
}
.input-box :deep(.t-textarea__inner::placeholder) {
  color: var(--bp-label-tertiary);
}
.input-actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
  margin-left: auto;
}
.composer-bar .composer-model + .input-actions {
  margin-left: 8px;
}
.composer-btn {
  min-width: 64px;
  height: 34px;
  gap: 5px;
  border-radius: 10px;
  border: none;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  font-size: 13px;
  font-weight: 500;
  color: #ffffff;
  background: var(--bp-accent);
  flex-shrink: 0;
  padding: 0 10px;
  transition: transform 120ms var(--bp-ease-out),
              background 120ms ease,
              color 120ms ease,
              opacity 120ms ease;
}
.composer-btn:active:not(:disabled) {
  transform: scale(0.97);
}
.composer-btn:focus-visible {
  outline: 2px solid var(--bp-accent);
  outline-offset: 2px;
}
@media (hover: hover) and (pointer: fine) {
  .composer-btn.send:hover:not(:disabled) {
    background: var(--bp-accent-hover, #4f46d5);
  }
}
.composer-btn.send:disabled {
  background: #eef0f2;
  color: var(--bp-label-quaternary);
  cursor: default;
}
.composer-btn.stop {
  background: var(--bp-surface);
  color: var(--bp-danger);
  box-shadow: inset 0 0 0 1px var(--bp-danger);
}
@media (hover: hover) and (pointer: fine) {
  .composer-btn.stop:hover {
    background: var(--bp-danger-soft);
  }
}
.attach-btn {
  position: relative;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  border-radius: 999px;
  cursor: pointer;
  color: var(--bp-label-secondary);
  font-size: 16px;
  background: #f2f3f5;
  flex-shrink: 0;
  transition: background 140ms ease,
              color 140ms ease,
              transform 120ms var(--bp-ease-out);
}
@media (hover: hover) and (pointer: fine) {
  .attach-btn:hover {
    background: var(--bp-surface-fill-hover);
    color: var(--bp-label);
  }
}
.attach-btn:active {
  transform: scale(0.97);
}
.attach-btn:focus-within {
  outline: 2px solid var(--bp-accent);
  outline-offset: 2px;
}
.attach-input {
  position: absolute;
  inset: 0;
  opacity: 0;
  cursor: pointer;
  font-size: 0;
}
.attach-strip {
  display: flex;
  gap: 6px;
  overflow-x: auto;
  padding: 0 4px 8px;
  scrollbar-width: none;
}
.attach-strip::-webkit-scrollbar { display: none; }
.attach-chip {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 3px 8px 3px 6px;
  border-radius: var(--bp-radius-pill);
  background: var(--bp-surface-fill);
  font-size: 12px;
  color: var(--bp-label-secondary);
  white-space: nowrap;
  max-width: 240px;
  flex-shrink: 0;
}
.attach-chip .t-icon:first-child {
  color: var(--bp-accent);
  font-size: 14px;
}
.attach-chip .attach-remove {
  font-size: 12px;
  line-height: 1;
  color: var(--bp-label-secondary);
  cursor: pointer;
  opacity: 1;
  border: none;
  background: transparent;
  padding: 0;
  display: inline-flex;
  align-items: center;
  transition: opacity 120ms ease, color 120ms ease;
}
.attach-chip .attach-remove:focus-visible {
  outline: 2px solid var(--bp-accent);
  outline-offset: 2px;
  opacity: 1;
}
@media (hover: hover) and (pointer: fine) {
  .attach-chip .attach-remove { opacity: 0; }
  .attach-chip:hover .attach-remove,
  .attach-chip:focus-within .attach-remove { opacity: 1; }
  .attach-chip .attach-remove:hover { color: var(--bp-danger); }
}
@media (prefers-reduced-motion: reduce) {
  .composer-btn,
  .attach-btn,
  .load-more-btn,
  .greet-chip,
  .scroll-to-bottom-btn {
    transition: background 120ms ease, color 120ms ease, opacity 120ms ease;
  }
  .attach-btn:active,
  .composer-btn:active,
  .load-more-btn:active,
  .greet-chip:active {
    transform: none;
  }
  .scroll-to-bottom-btn:active { transform: translateX(-50%); }
  .typing-indicator i,
  .stream-caret {
    animation: none !important;
  }
  .typing-indicator i { opacity: 0.85; }
  .stream-caret { opacity: 1; }
  .scroll-bottom-fade-enter-from,
  .scroll-bottom-fade-leave-to {
    transform: translateX(-50%);
  }
}
@media (prefers-reduced-transparency: reduce) {
  .chat-input-area { background: var(--bp-chat-bg, #fafbfe); }
}
@media (prefers-contrast: more) {
  .msg-row.user .msg-bubble,
  .input-box,
  .scroll-to-bottom-btn {
    border: 1px solid var(--bp-label);
  }
  .input-box:focus-within {
    border-color: var(--bp-accent);
  }
}

.topbar-heading { min-width: 0; flex: 1; }
.topbar-subtitle { display: flex; align-items: center; gap: 8px; min-width: 0; margin-top: 3px; color: var(--bp-label-secondary); font-size: 11px; }
.topbar-model { border-left: var(--bp-hairline); padding-left: 8px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.topbar-subtitle > span:first-child { flex-shrink: 0; max-width: 40%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.topbar-icon, .topbar-workspace { display: inline-flex; flex-shrink: 0; align-items: center; justify-content: center; gap: 6px; height: 34px; border: none; border-radius: 8px; background: transparent; color: var(--bp-label-secondary); cursor: pointer; }
.topbar-icon { width: 34px; font-size: 18px; }
.topbar-workspace { padding: 0 10px; font-size: 12px; background: var(--bp-surface-fill); }
.topbar-icon:hover, .topbar-workspace:hover { color: var(--bp-accent); background: var(--bp-accent-soft); }
.reply-status { color: var(--bp-accent); font-size: 12px; white-space: nowrap; display: flex; align-items: center; gap: 6px; }
.reply-status > span { width: 6px; height: 6px; background: currentColor; border-radius: 50%; }
.scroll-bottom-dock { position: relative; height: 0; flex-shrink: 0; z-index: 5; }
.history-state { min-height: 65%; display: flex; align-items: center; justify-content: center; flex-direction: column; gap: 12px; text-align: center; color: var(--bp-label-secondary); }
.history-state > .t-icon { font-size: 32px; color: var(--bp-label-tertiary); }
.history-state h3, .history-state p { margin: 0; }
.history-state h3 { font-size: 16px; font-weight: 500; color: var(--bp-label); }
.history-state p { font-size: 13px; }
.history-notice, .composer-notice { display: flex; align-items: flex-start; gap: 8px; padding: 10px 12px; background: var(--bp-warning-soft, #fff8e6); color: var(--bp-label-secondary); border-radius: 10px; font-size: 12px; line-height: 1.6; }
.history-notice { max-width: 800px; margin: 0 auto 18px; }
.composer-notice { margin-bottom: 10px; }
.history-notice > span, .composer-notice > span { flex: 1; }
.history-notice .t-icon, .composer-notice .t-icon { flex-shrink: 0; margin-top: 3px; }
.history-notice button, .composer-notice button { border: 0; background: transparent; color: var(--bp-accent); white-space: nowrap; cursor: pointer; padding: 0 2px; font: inherit; }
.composer-notice .notice-dismiss { color: var(--bp-label-secondary); }
.msg-actions { display: flex; align-items: center; gap: 10px; min-height: 28px; margin-top: 6px; color: var(--bp-label-tertiary); font-size: 11px; }
.message-copy { display: inline-flex; align-items: center; gap: 4px; padding: 4px 6px; border: 0; background: transparent; color: var(--bp-label-secondary); cursor: pointer; border-radius: 5px; font-size: 11px; }
.message-copy:hover { background: var(--bp-surface-fill); color: var(--bp-accent); }
.composer-hint { min-width: 0; color: var(--bp-label-secondary); font-size: 11px; }
.composer-footer { display: flex; gap: 8px; justify-content: space-between; max-width: 800px; margin: 8px auto 0; font-size: 10px; color: var(--bp-label-tertiary); }
.attach-preview { width: 32px; height: 32px; object-fit: cover; border-radius: 5px; }
.attach-info { display: flex; flex-direction: column; overflow: hidden; gap: 2px; }
.attach-info > span { overflow: hidden; text-overflow: ellipsis; }
.attach-info > small { font-size: 10px; color: var(--bp-label-secondary); }
.markdown-body :deep(img) { max-width: 100%; height: auto; border-radius: 10px; }
.topbar-icon:focus-visible, .topbar-workspace:focus-visible, .message-copy:focus-visible, .composer-notice button:focus-visible, .history-notice button:focus-visible { outline: 2px solid var(--bp-accent); outline-offset: 2px; }
@media (hover: hover) and (pointer: fine) {
  .msg-actions { opacity: 0.55; transition: opacity 120ms ease; }
  .msg-row:hover .msg-actions, .msg-row:focus-within .msg-actions { opacity: 1; }
}
@media (max-width: 767px) {
  .chat-topbar { height: 60px; gap: 6px; padding: 0 12px; }
  .topbar-workspace { padding: 0 8px; }
  .topbar-workspace span, .reply-status { display: none; }
  .chat-body { padding: 18px 16px; }
  .chat-input-area { padding: 10px 12px 8px; }
  .greet-title { font-size: 22px; }
  .greet-sub { line-height: 1.7; max-width: 280px; }
  .greet-chips { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 8px; }
  .greet-chip { padding: 10px 12px; border-radius: 12px; line-height: 1.6; }
  .input-box { padding: 10px 10px 6px; border-radius: 16px; }
  .composer-footer > span { display: none; }
  .composer-hint { font-size: 10px; }
  .msg-row.user .msg-content-wrap { max-width: 90%; }
}
.attachment-drop-overlay { position: absolute; inset: 10px; z-index: 50; border: 2px dashed var(--bp-accent); border-radius: 18px; background: color-mix(in srgb, var(--bp-surface) 96%, transparent); display: flex; align-items: center; justify-content: center; flex-direction: column; gap: 12px; pointer-events: none; color: var(--bp-accent); }
.attachment-drop-overlay > .t-icon { font-size: 40px; }
.attachment-drop-overlay > strong { font-size: 18px; }
.attachment-drop-overlay > span { font-size: 12px; color: var(--bp-label-secondary); }
.choice-reminder { display: flex; align-items: center; gap: 8px; width: 100%; flex-shrink: 0; border: 0; border-bottom: var(--bp-hairline); background: var(--bp-accent-soft); color: var(--bp-accent); padding: 10px 24px; text-align: left; font-size: 12px; cursor: pointer; }
.choice-reminder > span { flex: 1; }
.choice-reminder > strong { font-size: 11px; font-weight: 500; }
.choice-reminder:focus-visible { outline: 2px solid var(--bp-accent); outline-offset: -3px; }
.markdown-body :deep(pre) { position: relative; padding-top: 42px; }
.markdown-body :deep(.code-copy-button) { position: absolute; top: 8px; right: 10px; padding: 4px 8px; border: var(--bp-hairline); border-radius: 6px; background: var(--bp-surface); color: var(--bp-label-secondary); font-family: var(--bp-font-sans); font-size: 11px; cursor: pointer; }
.markdown-body :deep(.code-copy-button:hover) { color: var(--bp-accent); border-color: var(--bp-accent); }
.markdown-body :deep(.code-copy-button:focus-visible) { outline: 2px solid var(--bp-accent); outline-offset: 2px; }
.stream-error-banner { display: flex; gap: 8px; align-items: center; padding: 10px 24px; background: var(--bp-warning-soft, #fff8e6); color: var(--bp-label-secondary); font-size: 12px; line-height: 1.6; }
.stream-error-banner > span { flex: 1; }
.stream-error-banner button { flex-shrink: 0; border: 0; color: var(--bp-accent); background: transparent; font: inherit; cursor: pointer; }
.stream-error-banner button:focus-visible { outline: 2px solid var(--bp-accent); outline-offset: 2px; }
.composer-btn.stop:disabled { opacity: 0.6; cursor: wait; }
.msg-run-notice { display: flex; align-items: flex-start; gap: 6px; margin-top: 10px; padding: 8px 10px; border-radius: 8px; font-size: 12px; line-height: 1.65; background: var(--bp-surface-fill); color: var(--bp-label-secondary); }
.msg-run-notice > .t-icon { flex-shrink: 0; margin-top: 3px; }
.msg-run-notice.error { color: var(--bp-danger); background: var(--bp-danger-soft); }
.msg-run-notice.warning { background: var(--bp-warning-soft, #fff8e6); }
</style>
