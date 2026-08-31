<template>
  <div class="chat-page">
    <BotSidebar />
    <SessionList />
    <ChatWindow />
    <ChatToolPanel />
  </div>
</template>

<script setup>
import { watch, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useBotStore } from '@/stores/bot'
import BotSidebar from '@/components/BotSidebar.vue'
import SessionList from '@/components/SessionList.vue'
import ChatWindow from '@/components/ChatWindow.vue'
import ChatToolPanel from '@/components/ChatToolPanel.vue'

const route = useRoute()
const router = useRouter()
const store = useBotStore()
let syncingRoute = false
let initialized = false

async function initializeBotFromRoute() {
  const requestedId = String(route.params.botId || '')
  // 会话 ID 一律按字符串处理：后端可能返回非数字 ID（如 "sess-xxx"），
  // 早期用 Number() 强转会得到 NaN，与列表里的 id 严格比较永远匹配不上，
  // 表现为深链接进来选不中会话。
  const requestedSessionId = route.params.sessionId ? String(route.params.sessionId) : null
  if (!store.bots.length) await store.fetchBots()

  const requestedBot = store.bots.find(bot => String(bot.id) === requestedId)
  const targetId = requestedBot?.id || store.activeBotId || store.bots[0]?.id

  if (!targetId) {
    initialized = true
    return
  }

  if (String(store.activeBotId) !== String(targetId)) {
    store.selectBot(targetId)
    if (requestedSessionId) await store.openSessionById(requestedSessionId)
  } else if (requestedSessionId) {
    // 同一 bot，仅 sessionId 变化（如从侧边栏深链接跳转）
    await store.openSessionById(requestedSessionId)
  }

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

watch(() => route.params.botId, () => {
  if (initialized && !syncingRoute) initializeBotFromRoute()
})

// 同一 bot 下通过 URL 切换 session（如 /chat/bot/:botId/:sessionId）
watch(() => route.params.sessionId, (sid) => {
  if (!initialized || syncingRoute) return
  if (sid) store.openSessionById(String(sid))
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
}
</style>
