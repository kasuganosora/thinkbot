<template>
  <div class="session-panel" data-testid="session-panel" aria-label="会话列表面板">
    <div class="session-header">
      <div class="cur-bot">
        <span class="cur-bot-avatar">{{ store.activeBot?.avatar }}</span>
        <span class="cur-bot-name" data-testid="session-current-bot">{{ store.activeBot?.name || '未选择 Bot' }}</span>
      </div>
      <div class="header-actions">
        <t-button theme="primary" variant="text" size="small" data-testid="session-create-btn" @click="onCreate">
          <template #icon><t-icon name="add" /></template>
          新建
        </t-button>
        <t-button theme="default" variant="text" size="small" @click="store.loadSessions()">
          <template #icon><t-icon name="refresh" /></template>
        </t-button>
      </div>
    </div>

    <div class="session-list" data-testid="session-list" role="listbox" aria-label="会话列表">
      <template v-if="store.sessions.length">
        <div
          v-for="s in store.sessions"
          :key="s.id"
          class="session-item pressable"
          :class="{ active: s.id === store.activeSessionId, renaming: renamingId === s.id }"
          :data-testid="`session-item-${s.id}`"
          role="option"
          :aria-selected="s.id === store.activeSessionId"
          @click="onSelect(s)"
        >
          <div class="sess-body">
            <input
              v-if="renamingId === s.id"
              :ref="(el) => setRenameRef(s.id, el)"
              v-model="renameDraft"
              class="sess-rename"
              :data-testid="`session-rename-input-${s.id}`"
              maxlength="40"
              @click.stop
              @dblclick.stop
              @keydown.enter.prevent="commitRename(s)"
              @keydown.esc.prevent="cancelRename"
              @blur="commitRename(s)"
            />
            <div
              v-else
              class="sess-title"
              :data-testid="`session-title-${s.id}`"
              @dblclick.stop="startRename(s)"
            >{{ s.title || '新会话' }}</div>
            <div class="sess-meta">
              <span v-if="s.messageCount > 0" class="sess-count">{{ s.messageCount }} 条消息</span>
              <span class="sess-time">{{ formatTime(s.lastMsgAt || s.createdAt) }}</span>
            </div>
          </div>
          <t-button
            v-if="s.id !== store.activeSessionId && renamingId !== s.id"
            class="sess-delete"
            theme="default" variant="text" size="small" shape="circle"
            :data-testid="`session-delete-${s.id}`"
            @click.stop="onDelete(s)"
          >
            <template #icon><t-icon name="delete" /></template>
          </t-button>
        </div>
      </template>
      <t-empty
        v-else-if="!store.sessionsLoading"
        description="暂无会话，点击上方新建开始对话"
        style="margin-top: 40px"
        data-testid="session-empty-state"
      />
      <div v-else class="loading-wrap"><t-loading /></div>
    </div>
  </div>
</template>

<script setup>
import { nextTick, ref } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { useRouter } from 'vue-router'
import { useBotStore } from '@/stores/bot'

const store = useBotStore()
const router = useRouter()

const renamingId = ref(null)
const renameDraft = ref('')
const renameInputs = new Map()
let renameSaving = false

function setRenameRef(id, el) {
  if (el) renameInputs.set(id, el)
  else renameInputs.delete(id)
}

function onSelect(s) {
  if (renamingId.value === s.id) return
  store.selectSession(s.id)
  router.push({ name: 'chat-bot', params: { botId: store.activeBotId, sessionId: s.id } })
}

function startRename(s) {
  renamingId.value = s.id
  renameDraft.value = s.title && s.title !== '新会话' && s.title !== '默认会话' ? s.title : ''
  nextTick(() => {
    const el = renameInputs.get(s.id)
    if (el && typeof el.focus === 'function') {
      el.focus()
      el.select?.()
    }
  })
}

function cancelRename() {
  renamingId.value = null
  renameDraft.value = ''
}

async function commitRename(s) {
  if (renameSaving) return
  const next = String(renameDraft.value || '').trim()
  const prev = s.title || '新会话'
  if (!next || next === prev) {
    cancelRename()
    return
  }
  renameSaving = true
  try {
    await store.renameSession(s.id, next)
  } catch (e) {
    MessagePlugin.error(typeof e === 'string' ? e : e?.message || '改名失败')
  } finally {
    renameSaving = false
    cancelRename()
  }
}

async function onCreate() {
  try {
    const sess = await store.createSession('新会话')
    if (sess) router.push({ name: 'chat-bot', params: { botId: store.activeBotId, sessionId: sess.id } })
  } catch (e) {
    MessagePlugin.error(typeof e === 'string' ? e : e?.message || '创建失败')
  }
}

function onDelete(session) {
  const dialog = DialogPlugin.confirm({
    header: '删除确认',
    body: `确定删除「${session.title || '新会话'}」？该会话下的所有消息将被清除。`,
    theme: 'warning',
    onConfirm: async () => {
      try {
        await store.deleteSession(session.id)
        MessagePlugin.success('已删除')
      } catch (e) {
        console.error('onDelete failed', e)
        MessagePlugin.error(typeof e === 'string' ? e : e?.message || '删除失败')
      } finally {
        dialog.hide()
      }
    },
    onCancel: () => { dialog.hide() },
  })
}

function formatTime(iso) {
  if (!iso) return ''
  const d = new Date(iso)
  const now = new Date()
  const isToday = d.toDateString() === now.toDateString()
  if (isToday) return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
  const yesterday = new Date(now)
  yesterday.setDate(yesterday.getDate() - 1)
  if (d.toDateString() === yesterday.toDateString()) return '昨天'
  return d.toLocaleDateString('zh-CN', { month: '2-digit', day: '2-digit' })
}
</script>

<style scoped>
.session-panel {
  width: var(--bp-session-width);
  flex-shrink: 0;
  height: 100%;
  background: var(--bp-bg-subtle);
  border-right: var(--bp-hairline);
  display: flex;
  flex-direction: column;
}
.session-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 0 12px 0 14px;
  height: 52px;
  flex-shrink: 0;
  border-bottom: var(--bp-hairline);
  background: var(--bp-surface-toolbar);
  backdrop-filter: saturate(180%) blur(var(--bp-blur));
  -webkit-backdrop-filter: saturate(180%) blur(var(--bp-blur));
}
.cur-bot {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.cur-bot-avatar {
  font-size: 18px;
}
.cur-bot-name {
  font-size: 14px;
  font-weight: 600;
  letter-spacing: var(--bp-tracking-title);
  color: var(--bp-label);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.header-actions {
  display: flex;
  align-items: center;
  gap: 2px;
  flex-shrink: 0;
}
.session-list {
  flex: 1;
  overflow-y: auto;
  padding: 8px;
}
.session-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 9px 10px;
  border-radius: var(--bp-radius-md);
  cursor: pointer;
}
.session-item:hover {
  background: var(--bp-surface-fill-hover);
}
.session-item:hover .sess-delete {
  opacity: 1;
}
.session-item.active {
  background: var(--bp-surface);
  box-shadow: var(--bp-shadow-sm);
}
.session-item.renaming {
  cursor: text;
}
.sess-body {
  flex: 1;
  min-width: 0;
}
.sess-title {
  font-size: 13px;
  font-weight: 510;
  letter-spacing: var(--bp-tracking-body);
  color: var(--bp-label);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.sess-rename {
  display: block;
  width: 100%;
  font: inherit;
  font-size: 13px;
  font-weight: 510;
  color: var(--bp-label);
  background: var(--bp-surface);
  border: var(--bp-hairline);
  border-radius: var(--bp-radius-xs);
  padding: 2px 6px;
  outline: none;
  box-shadow: 0 0 0 3px var(--bp-accent-soft);
}
.sess-meta {
  display: flex;
  gap: 8px;
  font-size: 11px;
  letter-spacing: var(--bp-tracking-caption);
  color: var(--bp-label-tertiary);
  margin-top: 2px;
}
.sess-count {
  color: var(--bp-label-tertiary);
}
.sess-time {
  flex-shrink: 0;
}
.sess-delete {
  opacity: 0;
  transition: opacity var(--bp-duration) var(--bp-ease-out);
  color: var(--bp-label-tertiary) !important;
}
.sess-delete:hover {
  color: var(--bp-danger) !important;
}
.loading-wrap {
  display: flex;
  justify-content: center;
  margin-top: 40px;
}
</style>
