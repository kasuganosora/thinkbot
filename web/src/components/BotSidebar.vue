<template>
  <aside class="bot-sidebar" data-testid="bot-sidebar" aria-label="Bot 列表侧边栏">
    <div class="sidebar-top">
      <div class="logo-row">
        <BrandMark class="logo-icon" />
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
        <t-icon name="add" class="add-btn pressable" data-testid="bot-create-btn" aria-label="新建 Bot" @click="onCreateBot" />
      </t-tooltip>
    </div>

    <div class="bot-list" data-testid="bot-list" role="listbox" aria-label="Bot 列表">
      <div
        v-for="bot in filteredBots"
        :key="bot.id"
        class="bot-item pressable"
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
        <div class="user-card pressable" data-testid="sidebar-user-card" role="button" aria-label="打开用户与系统设置菜单">
          <div class="sidebar-avatar">{{ userInitial }}</div>
          <div class="user-name" data-testid="sidebar-user-name">{{ userStore.user?.nickname || '用户' }}</div>
          <t-icon name="setting" class="user-setting" data-testid="sidebar-user-menu" />
        </div>
      </t-dropdown>
    </div>
  </aside>
</template>

<script setup>
import BrandMark from '@/components/BrandMark.vue'
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

async function onCreateBot() {
  const bot = await store.createBot()
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
          onConfirm: async () => {
            try {
              await store.deleteBot(bot.id)
              MessagePlugin.success('已删除')
              dlg.destroy()
            } catch (e) {
              MessagePlugin.error(`删除失败：${e?.message || e || '未知错误'}`)
            }
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
  width: var(--bp-sidebar-width);
  flex-shrink: 0;
  height: 100%;
  background: var(--bp-surface-translucent);
  color: var(--bp-label);
  display: flex;
  flex-direction: column;
  backdrop-filter: saturate(180%) blur(var(--bp-blur));
  -webkit-backdrop-filter: saturate(180%) blur(var(--bp-blur));
  border-right: var(--bp-hairline);
}
.sidebar-top {
  padding: 18px 16px 6px;
}
.logo-row {
  display: flex;
  align-items: center;
  gap: 8px;
}
.logo-icon {
  width: 22px;
  height: 22px;
  color: var(--bp-label);
}
.logo-text {
  font-size: 15px;
  font-weight: 650;
  letter-spacing: var(--bp-tracking-title);
  color: var(--bp-label);
}
.sidebar-search {
  padding: 8px 12px 10px;
}
.section-label {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 8px 16px 6px;
  font-size: 11px;
  font-weight: 590;
  letter-spacing: 0.04em;
  color: var(--bp-label-secondary);
}
.add-btn {
  cursor: pointer;
  font-size: 16px;
  color: var(--bp-label-secondary);
  border-radius: 6px;
  padding: 2px;
}
.add-btn:hover {
  color: var(--bp-accent);
  background: var(--bp-accent-soft);
}
.bot-list {
  flex: 1;
  overflow-y: auto;
  padding: 0 8px 8px;
}
.bot-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 8px;
  border-radius: var(--bp-radius-md);
  cursor: pointer;
}
.bot-item:hover {
  background: var(--bp-surface-fill-hover);
}
.bot-item.active {
  background: var(--bp-surface-fill-active);
}
.bot-avatar {
  font-size: 20px;
  width: 30px;
  text-align: center;
  flex-shrink: 0;
}
.bot-meta {
  flex: 1;
  min-width: 0;
}
.bot-name {
  font-size: 13.5px;
  font-weight: 510;
  color: var(--bp-label);
  letter-spacing: var(--bp-tracking-body);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.bot-desc {
  font-size: 11px;
  letter-spacing: var(--bp-tracking-caption);
  color: var(--bp-label-tertiary);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.bot-more {
  opacity: 0;
  cursor: pointer;
  color: var(--bp-label-tertiary);
}
.bot-item:hover .bot-more {
  opacity: 1;
}
.sidebar-bottom {
  padding: 10px 12px 12px;
  border-top: var(--bp-hairline);
}
.user-card {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 6px;
  border-radius: var(--bp-radius-md);
  cursor: pointer;
}
.user-card:hover {
  background: var(--bp-surface-fill-hover);
}
.sidebar-avatar {
  width: 28px;
  height: 28px;
  flex-shrink: 0;
  border-radius: 50%;
  background: var(--bp-accent);
  color: var(--bp-label-on-accent);
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 12px;
  font-weight: 600;
}
.user-name {
  flex: 1;
  font-size: 13px;
  font-weight: 510;
  color: var(--bp-label);
}
.user-setting {
  cursor: pointer;
  color: var(--bp-label-tertiary);
}
.user-setting:hover {
  color: var(--bp-label);
}
</style>
