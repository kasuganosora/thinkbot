<template>
  <aside class="bot-sidebar" :class="{ compact: layout.botCollapsed }" data-testid="bot-sidebar" aria-label="Bot 列表侧边栏">
    <div class="sidebar-top">
      <button v-if="layout.botCollapsed" class="brand-button" type="button" title="展开 Bot 导航" aria-label="展开 Bot 导航" @click="layout.toggleBots()">
        <img class="logo-icon" src="/icon-app.svg" width="30" height="30" alt="" />
      </button>
      <div v-else class="logo-row">
        <img class="logo-icon" src="/icon-app.svg" width="30" height="30" alt="thinkbot" />
        <span class="logo-text">Bot 平台</span>
        <button v-if="!layout.isNarrow" class="nav-icon" type="button" title="收起 Bot 导航" aria-label="收起 Bot 导航" @click="layout.toggleBots()">
          <t-icon name="chevron-left" />
        </button>
      </div>
    </div>

    <div class="sidebar-search">
      <button v-if="layout.botCollapsed" class="nav-icon rail-action" type="button" title="搜索 Bot" aria-label="展开并搜索 Bot" @click="expandSearch">
        <t-icon name="search" />
      </button>
      <t-input v-else ref="searchInput" v-model="keyword" placeholder="搜索 Bot" size="small" clearable data-testid="bot-search-input" aria-label="搜索 Bot">
        <template #prefix-icon><t-icon name="search" /></template>
      </t-input>
    </div>

    <div class="section-label">
      <span v-if="!layout.botCollapsed">我的 Bot <span class="section-count">{{ store.bots.length }}</span></span>
      <button class="nav-icon" type="button" :disabled="creating" title="新建 Bot" data-testid="bot-create-btn" aria-label="新建 Bot" @click="onCreateBot">
        <t-loading v-if="creating" size="14px" />
        <t-icon v-else name="add" />
      </button>
    </div>

    <div class="bot-list" data-testid="bot-list" role="list" aria-label="Bot 列表" :aria-busy="store.loading">
      <div v-if="store.loading && !store.bots.length" class="list-state" role="status" aria-label="正在加载 Bot">
        <t-loading size="small" />
        <span v-if="!layout.botCollapsed">正在加载 Bot…</span>
      </div>
      <div v-if="store.error" class="list-state error-state" role="alert">
        <template v-if="!layout.botCollapsed"><t-icon name="error-circle" /><span>Bot 列表加载失败</span></template>
        <button class="retry-button" type="button" :disabled="store.loading" title="重新加载 Bot 列表" aria-label="重新加载 Bot 列表" @click="store.fetchBots()">
          <t-icon name="refresh" /><span v-if="!layout.botCollapsed">重试</span>
        </button>
      </div>
      <div
        v-for="bot in filteredBots"
        :key="bot.id"
        class="bot-item"
        :class="{ active: String(bot.id) === String(store.activeBotId) }"
        :data-testid="`bot-item-${bot.id}`"
        :data-bot-name="bot.name"
        role="listitem"
      >
        <button
          class="bot-select"
          type="button"
          :aria-current="String(bot.id) === String(store.activeBotId) ? 'true' : undefined"
          :aria-label="bot.name"
          :title="bot.name"
          @click="onSelectBot(bot)"
          @keydown="onBotKeydown"
        >
          <span class="bot-avatar" aria-hidden="true">{{ bot.avatar || '🤖' }}</span>
          <span v-if="!layout.botCollapsed" class="bot-meta">
            <span class="bot-name">{{ bot.name }}</span>
            <span v-if="bot.desc" class="bot-desc">{{ bot.desc }}</span>
          </span>
        </button>
        <t-dropdown v-if="!layout.botCollapsed" :options="botMenu(bot)" trigger="click" placement="bottom-right">
          <button class="nav-icon bot-more" type="button" data-testid="bot-item-menu" :aria-label="`${bot.name}的操作菜单`" title="Bot 操作" aria-haspopup="menu">
            <t-icon name="more" />
          </button>
        </t-dropdown>
      </div>
      <div v-if="!store.loading && !store.error && !filteredBots.length && !layout.botCollapsed" class="list-state">
        <t-icon :name="keyword.trim() ? 'search' : 'chat'" />
        <strong>{{ keyword.trim() ? '没有找到 Bot' : '还没有 Bot' }}</strong>
        <span>{{ keyword.trim() ? '试试其他名称或描述' : '创建一个 Bot，开始你的第一段对话' }}</span>
        <button v-if="keyword.trim()" class="retry-button" type="button" @click="keyword = ''">清除搜索</button>
        <button v-else class="retry-button" type="button" :disabled="creating" @click="onCreateBot">创建 Bot</button>
      </div>
    </div>

    <div class="sidebar-bottom">
      <t-dropdown :options="userMenu" trigger="click" placement="top" :min-column-width="160">
        <button class="user-card" type="button" data-testid="sidebar-user-card" :title="userName" aria-label="打开用户与系统设置菜单" aria-haspopup="menu">
          <span class="sidebar-avatar">{{ userInitial }}</span>
          <span v-if="!layout.botCollapsed" class="user-name" data-testid="sidebar-user-name">{{ userName }}</span>
          <t-icon v-if="!layout.botCollapsed" name="setting" class="user-setting" data-testid="sidebar-user-menu" />
        </button>
      </t-dropdown>
    </div>
  </aside>
</template>

<script setup>
import { ref, computed, nextTick } from 'vue'
import { useRouter } from 'vue-router'
import { DialogPlugin, MessagePlugin } from 'tdesign-vue-next'
import { useBotStore } from '@/stores/bot'
import { useUserStore } from '@/stores/user'
import { useLayoutStore } from '@/stores/layout'

const router = useRouter()
const store = useBotStore()
const userStore = useUserStore()
const layout = useLayoutStore()
const keyword = ref('')
const searchInput = ref(null)
const creating = ref(false)
const userName = computed(() => userStore.user?.nickname || userStore.user?.username || '用户')
const userInitial = computed(() => String(userName.value).trim().charAt(0).toUpperCase() || 'U')
const filteredBots = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  return !k ? store.bots : store.bots.filter(b => `${b.name || ''} ${b.desc || ''}`.toLowerCase().includes(k))
})

async function expandSearch() {
  layout.toggleBots()
  await nextTick()
  searchInput.value?.focus()
}

function onSelectBot(bot) {
  if (String(store.activeBotId) !== String(bot.id)) store.selectBot(bot.id)
  if (layout.isNarrow) layout.navigationSection = 'sessions'
}

function onBotKeydown(event) {
  const keys = ['ArrowDown', 'ArrowUp', 'Home', 'End']
  if (!keys.includes(event.key)) return
  event.preventDefault()
  const buttons = [...event.currentTarget.closest('.bot-list').querySelectorAll('.bot-select')]
  const current = buttons.indexOf(event.currentTarget)
  let target = current + (event.key === 'ArrowDown' ? 1 : -1)
  if (event.key === 'Home') target = 0
  if (event.key === 'End') target = buttons.length - 1
  buttons[(target + buttons.length) % buttons.length]?.focus()
}

async function onCreateBot() {
  if (creating.value) return
  creating.value = true
  try {
    const bot = await store.createBot()
    store.selectBot(bot.id)
    layout.closeNavigation()
    await router.push({ name: 'bot-settings', params: { id: bot.id } })
  } catch (e) {
    MessagePlugin.error(e?.message || '创建 Bot 失败，请重试')
  } finally {
    creating.value = false
  }
}

function openSettings(name, params) {
  layout.closeNavigation()
  router.push({ name, params })
}

function botMenu(bot) {
  return [
    { content: 'Bot 设置', value: 'edit', onClick: () => openSettings('bot-settings', { id: bot.id }) },
    {
      content: '删除 Bot', value: 'delete', theme: 'error',
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

const userMenu = computed(() => [
  { content: '用户设置', value: 'user', onClick: () => openSettings('user-settings') },
  { content: '系统设置', value: 'system', onClick: () => openSettings('system-settings') },
  {
    content: '退出登录', value: 'logout', theme: 'error',
    onClick: () => {
      userStore.logout()
      layout.closeNavigation()
      router.push({ name: 'login' })
    }
  }
])
</script>

<style scoped>
.bot-sidebar {
  width: var(--bp-sidebar-width, 208px);
  flex-shrink: 0;
  height: 100%;
  min-height: 0;
  background: var(--bp-surface-translucent);
  color: var(--bp-label);
  display: flex;
  flex-direction: column;
  border-right: var(--bp-hairline);
}
.bot-sidebar.compact { width: 64px; }
.sidebar-top { height: 64px; padding: 0 14px; display: flex; align-items: center; }
.logo-row { display: flex; align-items: center; gap: 8px; width: 100%; }
.logo-icon { width: 30px; height: 30px; display: block; flex-shrink: 0; }
.logo-text { flex: 1; white-space: nowrap; font-size: 15px; font-weight: 650; letter-spacing: var(--bp-tracking-title); }
.brand-button { padding: 3px; border: 0; border-radius: 8px; background: transparent; cursor: pointer; }
.sidebar-search { padding: 2px 12px 12px; }
.section-label { display: flex; align-items: center; justify-content: space-between; padding: 4px 12px 6px 16px; color: var(--bp-label-secondary); font-size: 11px; font-weight: 600; }
.section-count { color: var(--bp-label-tertiary); margin-left: 4px; font-weight: 400; }
.nav-icon { display: inline-flex; align-items: center; justify-content: center; width: 28px; height: 28px; padding: 0; flex-shrink: 0; border: 0; border-radius: var(--bp-radius-sm); background: transparent; color: var(--bp-label-secondary); font-size: 17px; cursor: pointer; }
.nav-icon:hover { background: var(--bp-surface-fill-hover); color: var(--bp-accent); }
.nav-icon:disabled { opacity: .5; cursor: wait; }
.bot-list { flex: 1; min-height: 0; overflow-y: auto; padding: 0 8px 12px; }
.bot-item { display: flex; align-items: center; border-radius: var(--bp-radius-md); margin-bottom: 4px; min-width: 0; }
.bot-item:hover { background: var(--bp-surface-fill-hover); }
.bot-item.active { background: var(--bp-accent-soft); }
.bot-select { display: flex; align-items: center; gap: 9px; flex: 1; min-width: 0; min-height: 48px; padding: 9px 8px; border: 0; border-radius: var(--bp-radius-md); background: transparent; cursor: pointer; text-align: left; color: inherit; font: inherit; }
.bot-avatar { font-size: 22px; line-height: 28px; width: 30px; flex-shrink: 0; text-align: center; }
.bot-meta { display: flex; flex-direction: column; min-width: 0; gap: 2px; }
.bot-name { font-size: 13px; font-weight: 550; letter-spacing: var(--bp-tracking-body); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.bot-item.active .bot-name { color: var(--bp-accent); font-weight: 650; }
.bot-desc { font-size: 11px; color: var(--bp-label-secondary); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.bot-more { margin-right: 4px; width: 26px; opacity: .55; }
.bot-item:hover .bot-more, .bot-item:focus-within .bot-more { opacity: 1; }
.list-state { display: flex; flex-direction: column; align-items: center; gap: 9px; padding: 30px 10px; text-align: center; color: var(--bp-label-secondary); font-size: 12px; line-height: 1.6; }
.list-state > .t-icon { font-size: 24px; color: var(--bp-label-tertiary); }
.list-state strong { font-size: 13px; font-weight: 550; color: var(--bp-label); }
.error-state { margin-bottom: 8px; padding: 16px 8px; background: var(--bp-danger-soft); border-radius: var(--bp-radius-md); }
.retry-button { display: inline-flex; align-items: center; justify-content: center; gap: 5px; padding: 5px 8px; border: 0; border-radius: var(--bp-radius-xs); background: transparent; color: var(--bp-accent); font: inherit; cursor: pointer; }
.retry-button:hover { background: var(--bp-accent-soft); }
.sidebar-bottom { padding: 10px 10px 12px; border-top: var(--bp-hairline); }
.user-card { display: flex; align-items: center; gap: 9px; width: 100%; min-width: 0; padding: 7px 6px; border: 0; border-radius: var(--bp-radius-md); background: transparent; color: inherit; cursor: pointer; font: inherit; text-align: left; }
.user-card:hover { background: var(--bp-surface-fill-hover); }
.sidebar-avatar { width: 28px; height: 28px; flex-shrink: 0; border-radius: 50%; background: var(--bp-accent); color: var(--bp-label-on-accent); display: flex; align-items: center; justify-content: center; font-size: 12px; font-weight: 600; }
.user-name { flex: 1; font-size: 13px; font-weight: 550; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.user-setting { color: var(--bp-label-secondary); flex-shrink: 0; }
.compact .sidebar-top { justify-content: center; padding: 0; }
.compact .sidebar-search { padding: 2px 12px 10px; }
.compact .rail-action { width: 40px; height: 36px; }
.compact .section-label { padding: 0 18px 6px; }
.compact .bot-select { justify-content: center; padding: 8px; }
.compact .list-state { padding: 20px 0; }
.compact .sidebar-bottom { padding: 10px 8px 12px; }
.compact .user-card { justify-content: center; }
button:focus-visible { outline: 2px solid var(--bp-accent); outline-offset: 2px; }
</style>
