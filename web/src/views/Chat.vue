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
  if (!store.bots.length) await store.fetchBots()

  const requestedBot = store.bots.find(bot => String(bot.id) === requestedId)
  const targetId = requestedBot?.id || store.activeBotId || store.bots[0]?.id

  if (!targetId) {
    initialized = true
    return
  }
  if (String(store.activeBotId) !== String(targetId)) store.selectBot(targetId)

  if (route.name !== 'chat-bot' || String(route.params.botId) !== String(targetId)) {
    syncingRoute = true
    await router.replace({ name: 'chat-bot', params: { botId: targetId } })
    syncingRoute = false
  }
  initialized = true
}

onMounted(initializeBotFromRoute)

watch(() => route.params.botId, () => {
  if (initialized && !syncingRoute) initializeBotFromRoute()
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
}
</style>
