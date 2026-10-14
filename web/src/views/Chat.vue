<template>
  <div class="chat-page">
    <div v-if="layout.isNarrow && layout.navigationOpen" class="navigation-backdrop" @click="layout.closeNavigation()" aria-hidden="true"></div>
    <div
      v-show="!layout.isNarrow || layout.navigationOpen"
      ref="navigationRef"
      class="chat-navigation"
      :class="{ 'is-drawer': layout.isNarrow }"
      :inert="layout.isNarrow && layout.toolsOpen"
      :role="layout.isNarrow ? 'dialog' : undefined"
      :aria-modal="layout.isNarrow ? true : undefined"
      aria-label="Bot 和会话导航"
      @keydown.esc.stop="layout.closeNavigation()"
      @keydown.tab="trapNavigationFocus"
    >
      <div v-if="layout.isNarrow" class="navigation-toolbar">
        <div class="navigation-switch" role="group" aria-label="导航内容">
          <button :class="{ active: layout.navigationSection === 'bots' }" :aria-pressed="layout.navigationSection === 'bots'" @click="layout.navigationSection = 'bots'">我的 Bot</button>
          <button :class="{ active: layout.navigationSection === 'sessions' }" :aria-pressed="layout.navigationSection === 'sessions'" @click="layout.navigationSection = 'sessions'">会话</button>
        </div>
        <button class="navigation-close" aria-label="关闭导航" @click="layout.closeNavigation()"><t-icon name="close" /></button>
      </div>
      <BotSidebar v-show="!layout.isNarrow || layout.navigationSection === 'bots'" />
      <SessionList v-show="layout.isNarrow ? layout.navigationSection === 'sessions' : layout.sessionsVisible" />
    </div>
    <ChatWindow :inert="layout.isNarrow && (layout.navigationOpen || layout.toolsOpen)" />
    <ChatToolPanel />
  </div>
</template>

<script setup>
import { ref, watch, onMounted, onBeforeUnmount, nextTick } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useBotStore } from '@/stores/bot'
import { useLayoutStore } from '@/stores/layout'
import BotSidebar from '@/components/BotSidebar.vue'
import SessionList from '@/components/SessionList.vue'
import ChatWindow from '@/components/ChatWindow.vue'
import ChatToolPanel from '@/components/ChatToolPanel.vue'

const route = useRoute()
const router = useRouter()
const store = useBotStore()
const layout = useLayoutStore()
const navigationRef = ref(null)
let navigationTrigger = null
function onResize() { layout.viewportWidth = window.innerWidth }
function focusableNavigation() {
  return Array.from(navigationRef.value?.querySelectorAll('button, input, [tabindex="0"]') || [])
    .filter(el => !el.disabled && el.getClientRects().length)
}
function trapNavigationFocus(event) {
  if (!layout.isNarrow || !layout.navigationOpen) return
  const nodes = focusableNavigation()
  const first = nodes[0]
  const last = nodes[nodes.length - 1]
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus() }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus() }
}
watch(() => layout.navigationOpen, async open => {
  if (open) {
    navigationTrigger = document.activeElement
    await nextTick()
    focusableNavigation()[0]?.focus()
  } else {
    await nextTick()
    if (!layout.toolsOpen && navigationTrigger?.isConnected) navigationTrigger.focus()
  }
})
onMounted(() => {
  onResize()
  window.addEventListener('resize', onResize)
})
onBeforeUnmount(() => window.removeEventListener('resize', onResize))
let syncingRoute = false
let initialized = false
let routeGeneration = 0

async function initializeBotFromRoute() {
  const generation = ++routeGeneration
  const requestedId = String(route.params.botId || '')
  // 会话 ID 一律按字符串处理：后端可能返回非数字 ID（如 "sess-xxx"），
  // 早期用 Number() 强转会得到 NaN，与列表里的 id 严格比较永远匹配不上，
  // 表现为深链接进来选不中会话。
  const requestedSessionId = route.params.sessionId ? String(route.params.sessionId) : null
  if (!store.bots.length) await store.fetchBots()
  if (generation !== routeGeneration) return

  const requestedBot = store.bots.find(bot => String(bot.id) === requestedId)
  const targetId = requestedBot?.id || store.activeBotId || store.bots[0]?.id

  if (!targetId) {
    initialized = true
    return
  }

  if (String(store.activeBotId) !== String(targetId)) {
    // Set the target before changing Bot: the store's watcher loads its sessions.
    // Looking in the previous Bot's list here loses cross-Bot deep links.
    store.setPendingSession(requestedSessionId)
    store.selectBot(targetId)
  } else if (requestedSessionId) {
    // 同一 bot，仅 sessionId 变化（如从侧边栏深链接跳转）
    await store.openSessionById(requestedSessionId)
  }
  if (generation !== routeGeneration) return

  if (route.name !== 'chat-bot' || String(route.params.botId) !== String(targetId)) {
    syncingRoute = true
    await router.replace({
      name: 'chat-bot',
      params: { botId: targetId, sessionId: requestedSessionId || undefined }
    })
    syncingRoute = false
  }
  initialized = true
}

onMounted(initializeBotFromRoute)

watch(() => [route.params.botId, route.params.sessionId], () => {
  if (!syncingRoute) initializeBotFromRoute()
})

watch(() => store.activeBotId, (botId) => {
  if (!initialized || !botId || String(route.params.botId || '') === String(botId)) return
  router.push({ name: 'chat-bot', params: { botId } })
})
</script>

<style scoped>
.chat-page {
  display: flex;
  height: 100%;
  width: 100%;
  background: var(--bp-bg);
  min-width: 0;
  overflow: hidden;
}
.chat-navigation { display: flex; height: 100%; flex-shrink: 0; min-width: 0; }
.navigation-backdrop { position: fixed; inset: 0; z-index: 80; background: rgba(20, 23, 38, .32); backdrop-filter: blur(3px); }
.chat-navigation.is-drawer { position: fixed; inset: 0 auto 0 0; width: min(360px, calc(100vw - 24px)); z-index: 81; flex-direction: column; background: var(--bp-surface); box-shadow: var(--bp-shadow-lg); }
.is-drawer :deep(.bot-sidebar), .is-drawer :deep(.session-panel) { width: 100%; flex: 1; min-height: 0; height: auto; border: 0; }
.navigation-toolbar { display: flex; align-items: center; gap: 12px; padding: 12px; border-bottom: var(--bp-hairline); }
.navigation-switch { display: flex; flex: 1; padding: 3px; background: var(--bp-surface-fill); border-radius: var(--bp-radius-md); }
.navigation-switch button { flex: 1; padding: 8px 12px; border: 0; border-radius: var(--bp-radius-sm); background: none; color: var(--bp-label-secondary); cursor: pointer; }
.navigation-switch button.active { background: var(--bp-surface); color: var(--bp-accent); box-shadow: var(--bp-shadow-xs); font-weight: 600; }
.navigation-close { display: grid; place-items: center; width: 36px; height: 36px; border: 0; border-radius: var(--bp-radius-sm); background: transparent; color: var(--bp-label-secondary); cursor: pointer; }
.navigation-close:hover { background: var(--bp-surface-fill); }
.navigation-toolbar button:focus-visible { outline: 2px solid var(--bp-accent); outline-offset: 2px; }
</style>
