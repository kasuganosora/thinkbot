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

    <!-- 会话列表 -->
    <div class="session-list" data-testid="session-list" role="listbox" aria-label="会话列表">
      <template v-if="store.sessions.length">
        <div
          v-for="s in store.sessions"
          :key="s.id"
          class="session-item"
          :class="{ active: s.id === store.activeSessionId }"
          :data-testid="`session-item-${s.id}`"
          role="option"
          :aria-selected="s.id === store.activeSessionId"
          @click="onSelect(s)"
        >
          <div class="sess-body">
            <div class="sess-title">{{ s.title || '新会话' }}</div>
            <div class="sess-meta">
              <span v-if="s.messageCount > 0" class="sess-count">{{ s.messageCount }} 条消息</span>
              <span class="sess-time">{{ formatTime(s.lastMsgAt || s.createdAt) }}</span>
            </div>
          </div>
          <t-button
            v-if="s.id !== store.activeSessionId"
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
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { useRouter } from 'vue-router'
import { useBotStore } from '@/stores/bot'

const store = useBotStore()
const router = useRouter()

function onSelect(s) {
  store.selectSession(s.id)
  // 同步 URL，使会话深链接可分享（#/chat/bot/:botId/:sessionId）
  router.push({ name: 'chat-bot', params: { botId: store.activeBotId, sessionId: s.id } })
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
        // 无论成功失败都关闭对话框
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
  width: 260px;
  flex-shrink: 0;
  height: 100%;
  background: #fafafa;
  border-right: 1px solid #ececec;
  display: flex;
  flex-direction: column;
}
.session-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 14px 14px;
  border-bottom: 1px solid #ececec;
}
.cur-bot {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.cur-bot-avatar {
  font-size: 20px;
}
.cur-bot-name {
  font-size: 15px;
  font-weight: 600;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.header-actions {
  display: flex;
  align-items: center;
  gap: 4px;
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
  padding: 10px;
  border-radius: 8px;
  cursor: pointer;
  transition: background 0.15s;
}
.session-item:hover {
  background: #f0f0f0;
}
.session-item:hover .sess-delete {
  opacity: 1;
}
.session-item.active {
  background: #e6f4ef;
}
.sess-body {
  flex: 1;
  min-width: 0;
}
.sess-title {
  font-size: 13px;
  color: #1d1d1f;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.sess-meta {
  display: flex;
  gap: 8px;
  font-size: 11px;
  color: #aaa;
  margin-top: 2px;
}
.sess-count {
  color: #888;
}
.sess-time {
  flex-shrink: 0;
}
.sess-delete {
  opacity: 0;
  transition: opacity 0.15s;
  color: #999 !important;
}
.sess-delete:hover {
  color: #d63c3c !important;
}
.loading-wrap {
  display: flex;
  justify-content: center;
  margin-top: 40px;
}
</style>
