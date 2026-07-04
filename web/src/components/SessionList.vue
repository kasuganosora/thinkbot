<template>
  <div class="session-panel" data-testid="session-panel" aria-label="会话列表面板">
    <div class="session-header">
      <div class="cur-bot">
        <span class="cur-bot-avatar">{{ store.activeBot?.avatar }}</span>
        <span class="cur-bot-name" data-testid="session-current-bot">{{ store.activeBot?.name || '未选择 Bot' }}</span>
      </div>
      <t-button theme="primary" variant="text" size="small" data-testid="session-create-btn" @click="store.loadMessages()">
        <template #icon><t-icon name="refresh" /></template>
        刷新
      </t-button>
    </div>

    <div class="session-list" data-testid="session-list" role="listbox" aria-label="Bot 列表">
      <template v-if="store.bots.length">
        <div
          v-for="bot in store.bots"
          :key="bot.id"
          class="session-item"
          :class="{ active: bot.id === store.activeBotId }"
          :data-testid="`session-item-${bot.id}`"
          role="option"
          :aria-selected="bot.id === store.activeBotId"
          @click="store.selectBot(bot.id)"
        >
          <div class="sess-avatar">{{ bot.avatar || '🤖' }}</div>
          <div class="sess-body">
            <div class="sess-title">{{ bot.name }}</div>
            <div class="sess-time">{{ bot.running ? '运行中' : '已停止' }}</div>
          </div>
        </div>
      </template>
      <t-empty v-else description="暂无 Bot" style="margin-top: 40px" data-testid="session-empty-state" />
    </div>
  </div>
</template>

<script setup>
import { useBotStore } from '@/stores/bot'

const store = useBotStore()
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
.sess-avatar {
  font-size: 20px;
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
</style>
