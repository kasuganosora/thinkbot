<template>
  <div class="session-panel" data-testid="session-panel" aria-label="会话列表面板">
    <div class="session-header">
      <div class="cur-bot">
        <span class="cur-bot-avatar">{{ store.activeBot?.avatar }}</span>
        <span class="cur-bot-name" data-testid="session-current-bot">{{ store.activeBot?.name || '未选择 Bot' }}</span>
      </div>
      <t-button theme="primary" variant="text" size="small" data-testid="session-create-btn" @click="store.createSession()">
        <template #icon><t-icon name="add" /></template>
        新会话
      </t-button>
    </div>

    <div class="session-list" data-testid="session-list" role="listbox" aria-label="会话列表">
      <template v-if="store.sessions.length">
        <div
          v-for="sess in store.sessions"
          :key="sess.id"
          class="session-item"
          :class="{ active: sess.id === store.activeSessionId }"
          :data-testid="`session-item-${sess.id}`"
          :data-session-title="sess.title"
          role="option"
          :aria-selected="sess.id === store.activeSessionId"
          @click="store.selectSession(sess.id)"
        >
          <t-icon name="chat" class="sess-icon" />
          <div class="sess-body">
            <div class="sess-title">{{ sess.title }}</div>
            <div class="sess-time">{{ formatTime(sess.updatedAt) }}</div>
          </div>
          <t-icon name="delete" class="sess-del" data-testid="session-delete-btn" aria-label="删除会话" @click.stop="onDelete(sess)" />
        </div>
      </template>
      <t-empty v-else description="暂无会话，点击「新会话」开始" style="margin-top: 40px" data-testid="session-empty-state" />
    </div>
  </div>
</template>

<script setup>
import { DialogPlugin } from 'tdesign-vue-next'
import { useBotStore } from '@/stores/bot'

const store = useBotStore()

function formatTime(ts) {
  const d = new Date(ts)
  const now = Date.now()
  const diff = now - ts
  if (diff < 60_000) return '刚刚'
  if (diff < 3600_000) return `${Math.floor(diff / 60_000)} 分钟前`
  if (diff < 86400_000) return `${Math.floor(diff / 3600_000)} 小时前`
  return `${d.getMonth() + 1}/${d.getDate()}`
}

function onDelete(sess) {
  const dlg = DialogPlugin.confirm({
    header: '删除会话',
    body: `确认删除会话「${sess.title}」？`,
    theme: 'warning',
    onConfirm: () => {
      store.deleteSession(sess.id)
      dlg.destroy()
    }
  })
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
.session-list {
  flex: 1;
  overflow-y: auto;
  padding: 8px;
}
.session-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px;
  border-radius: 8px;
  cursor: pointer;
  transition: background 0.15s;
}
.session-item:hover {
  background: #f0f0f0;
}
.session-item.active {
  background: #e6f4ef;
}
.sess-icon {
  color: #00a870;
  flex-shrink: 0;
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
.sess-time {
  font-size: 11px;
  color: #aaa;
  margin-top: 2px;
}
.sess-del {
  opacity: 0;
  color: #bbb;
  transition: opacity 0.15s, color 0.15s;
}
.session-item:hover .sess-del {
  opacity: 1;
}
.sess-del:hover {
  color: #e34d59;
}
</style>
