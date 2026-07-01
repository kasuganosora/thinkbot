<template>
  <aside class="bot-sidebar" data-testid="bot-sidebar" aria-label="Bot 列表侧边栏">
    <div class="sidebar-top">
      <div class="logo-row">
        <span class="logo-icon">🤖</span>
        <span class="logo-text">Bot 平台</span>
      </div>
    </div>

    <div class="sidebar-search">
      <t-input v-model="keyword" placeholder="搜索 Bot" size="small" data-testid="bot-search-input">
        <template #prefix-icon><t-icon name="search" /></template>
      </t-input>
    </div>

    <div class="section-label">
      <span>我的 Bot</span>
      <t-tooltip content="新建 Bot">
        <t-icon name="add" class="add-btn" data-testid="bot-create-btn" aria-label="新建 Bot" @click="onCreateBot" />
      </t-tooltip>
    </div>

    <div class="bot-list" data-testid="bot-list" role="listbox" aria-label="Bot 列表">
      <div
        v-for="bot in filteredBots"
        :key="bot.id"
        class="bot-item"
        :class="{ active: bot.id === store.activeBotId }"
        :data-testid="`bot-item-${bot.id}`"
        :data-bot-name="bot.name"
        role="option"
        :aria-selected="bot.id === store.activeBotId"
        @click="store.selectBot(bot.id)"
      >
        <span class="bot-avatar">{{ bot.avatar }}</span>
        <div class="bot-meta">
          <div class="bot-name">{{ bot.name }}</div>
          <div class="bot-desc">{{ bot.desc }}</div>
        </div>
        <t-dropdown :options="botMenu(bot)" trigger="click" @click.stop>
          <t-icon name="more" class="bot-more" data-testid="bot-item-menu" aria-label="Bot 操作菜单" @click.stop />
        </t-dropdown>
      </div>
    </div>

    <div class="sidebar-bottom">
      <t-dropdown :options="userMenu" trigger="click" placement="top" :min-column-width="160">
        <div class="user-card" data-testid="sidebar-user-card" role="button" aria-label="打开用户与系统设置菜单">
          <div class="sidebar-avatar">{{ userInitial }}</div>
          <div class="user-name" data-testid="sidebar-user-name">{{ userStore.user?.nickname || '用户' }}</div>
          <t-icon name="setting" class="user-setting" data-testid="sidebar-user-menu" />
        </div>
      </t-dropdown>
    </div>
  </aside>
</template>

<script setup>
import { ref, computed } from 'vue'
import { useRouter } from 'vue-router'
import { DialogPlugin, MessagePlugin } from 'tdesign-vue-next'
import { useBotStore } from '@/stores/bot'
import { useUserStore } from '@/stores/user'

const router = useRouter()
const store = useBotStore()
const userStore = useUserStore()
const keyword = ref('')

const userInitial = computed(() => {
  const name = userStore.user?.nickname || userStore.user?.username || 'U'
  return String(name).trim().charAt(0).toUpperCase() || 'U'
})

const filteredBots = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  if (!k) return store.bots
  return store.bots.filter(b => b.name.toLowerCase().includes(k) || b.desc.toLowerCase().includes(k))
})

function onCreateBot() {
  const bot = store.createBot()
  store.selectBot(bot.id)
  router.push({ name: 'bot-settings', params: { id: bot.id } })
}

function botMenu(bot) {
  return [
    { content: '设置', value: 'edit', onClick: () => router.push({ name: 'bot-settings', params: { id: bot.id } }) },
    {
      content: '删除',
      value: 'delete',
      theme: 'error',
      onClick: () => {
        const dlg = DialogPlugin.confirm({
          header: '删除 Bot',
          body: `确认删除「${bot.name}」？该操作不可恢复。`,
          theme: 'warning',
          onConfirm: () => {
            store.deleteBot(bot.id)
            MessagePlugin.success('已删除')
            dlg.destroy()
          }
        })
      }
    }
  ]
}

const userMenu = computed(() => {
  const items = [
    { content: '用户设置', value: 'user', onClick: () => router.push({ name: 'user-settings' }) },
    { content: '系统设置', value: 'system', onClick: () => router.push({ name: 'system-settings' }) }
  ]
  if (userStore.user?.role === 'admin') {
    items.push(
      { content: '── 管理后台 ──', value: 'divider', disabled: true },
      { content: '用户管理', value: 'admin-users', onClick: () => router.push({ name: 'admin-users' }) },
      { content: '技能管理', value: 'admin-skills', onClick: () => router.push({ name: 'admin-skills' }) },
      { content: '系统配置', value: 'admin-config', onClick: () => router.push({ name: 'admin-config' }) },
      { content: '统计概览', value: 'admin-stats', onClick: () => router.push({ name: 'admin-stats' }) },
      { content: '系统监控', value: 'admin-system', onClick: () => router.push({ name: 'admin-system' }) }
    )
  }
  items.push({
    content: '退出登录',
    value: 'logout',
    theme: 'error',
    onClick: () => {
      userStore.logout()
      router.push({ name: 'login' })
    }
  })
  return items
})
</script>

<style scoped>
.bot-sidebar {
  width: 240px;
  flex-shrink: 0;
  height: 100%;
  background: var(--bp-sidebar-bg);
  color: var(--bp-sidebar-text);
  display: flex;
  flex-direction: column;
}
.sidebar-top {
  padding: 16px 16px 8px;
}
.logo-row {
  display: flex;
  align-items: center;
  gap: 8px;
}
.logo-icon {
  font-size: 22px;
}
.logo-text {
  font-size: 16px;
  font-weight: 600;
  color: #fff;
}
.sidebar-search {
  padding: 8px 12px;
}
.section-label {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 10px 16px 6px;
  font-size: 12px;
  color: var(--bp-sidebar-muted);
}
.add-btn {
  cursor: pointer;
  font-size: 16px;
  transition: color 0.2s;
}
.add-btn:hover {
  color: #00a870;
}
.bot-list {
  flex: 1;
  overflow-y: auto;
  padding: 0 8px;
}
.bot-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 8px;
  border-radius: 8px;
  cursor: pointer;
  transition: background 0.15s;
}
.bot-item:hover {
  background: var(--bp-sidebar-hover);
}
.bot-item.active {
  background: var(--bp-sidebar-active);
}
.bot-avatar {
  font-size: 22px;
  width: 32px;
  text-align: center;
  flex-shrink: 0;
}
.bot-meta {
  flex: 1;
  min-width: 0;
}
.bot-name {
  font-size: 14px;
  color: #fff;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.bot-desc {
  font-size: 11px;
  color: var(--bp-sidebar-muted);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.bot-more {
  opacity: 0;
  cursor: pointer;
  transition: opacity 0.15s;
}
.bot-item:hover .bot-more {
  opacity: 1;
}
.sidebar-bottom {
  padding: 10px 12px;
  border-top: 1px solid rgba(255, 255, 255, 0.08);
}
.user-card {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 6px;
  border-radius: 8px;
  cursor: pointer;
}
.user-card:hover {
  background: var(--bp-sidebar-hover);
}
.sidebar-avatar {
  width: 32px;
  height: 32px;
  flex-shrink: 0;
  border-radius: 50%;
  background: #00a870;
  color: #fff;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 14px;
  font-weight: 600;
}
.user-name {
  flex: 1;
  font-size: 14px;
  color: #fff;
}
.user-setting {
  cursor: pointer;
  color: var(--bp-sidebar-muted);
}
.user-setting:hover {
  color: #fff;
}
</style>
