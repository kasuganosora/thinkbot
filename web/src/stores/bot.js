import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { botApi, chatApi } from '@/api/services'

let idSeed = 1000
const uid = () => `id_${++idSeed}`

// 前端本地 session 管理（后端为 bot 级 chat/history，暂无 session CRUD）
function loadSessions(botId) {
  try {
    return JSON.parse(localStorage.getItem(`bp_sessions_${botId}`) || '[]')
  } catch { return [] }
}
function saveSessions(botId, sessions) {
  localStorage.setItem(`bp_sessions_${botId}`, JSON.stringify(sessions))
}

export const useBotStore = defineStore('bot', () => {
  const bots = ref([])
  const loading = ref(false)
  const error = ref(null)
  const activeBotId = ref('')
  const activeSessionId = ref('')
  const sessionsCache = ref({}) // botId → sessions[]

  // ---- 初始化：从后端加载 Bot 列表 ----
  async function fetchBots() {
    loading.value = true
    error.value = null
    try {
      bots.value = await botApi.list()
      if (!activeBotId.value && bots.value.length > 0) {
        activeBotId.value = bots.value[0].id
      }
    } catch (e) {
      error.value = e.message || '加载 Bot 列表失败'
    } finally {
      loading.value = false
    }
  }

  // ---- 计算属性 ----
  const activeBot = computed(() => bots.value.find(b => b.id === activeBotId.value))
  const sessions = computed(() => {
    const botId = activeBotId.value
    if (!botId) return []
    if (!sessionsCache.value[botId]) {
      sessionsCache.value[botId] = loadSessions(botId)
    }
    return sessionsCache.value[botId]
  })
  const activeSession = computed(() => sessions.value.find(s => s.id === activeSessionId.value))

  // ---- Bot 操作（走后端 API） ----
  function selectBot(id) {
    activeBotId.value = id
    const list = sessions.value
    activeSessionId.value = list.length > 0 ? list[0].id : ''
  }

  function selectSession(id) {
    activeSessionId.value = id
  }

  async function createBot(payload = {}) {
    const bot = await botApi.create({
      name: payload.name || '新建 Bot',
      systemPrompt: payload.prompt || '',
      llmMain: payload.llmMain || '',
      llmLight: payload.llmLight || '',
      temperature: payload.temperature ?? 0.7,
      maxTokens: payload.maxTokens ?? 4096,
      ...payload
    })
    bots.value.push(bot)
    return bot
  }

  async function updateBot(id, patch) {
    await botApi.update(id, patch)
    const bot = bots.value.find(b => b.id === id)
    if (bot) Object.assign(bot, patch)
  }

  async function deleteBot(id) {
    await botApi.remove(id)
    const idx = bots.value.findIndex(b => b.id === id)
    if (idx > -1) bots.value.splice(idx, 1)
    if (activeBotId.value === id) {
      selectBot(bots.value[0]?.id || '')
    }
    // 清理本地 session 缓存
    delete sessionsCache.value[id]
    localStorage.removeItem(`bp_sessions_${id}`)
  }

  // ---- Session 操作（前端本地管理） ----
  function createSession() {
    if (!activeBot.value) return
    const botId = activeBotId.value
    const list = sessionsCache.value[botId] || loadSessions(botId)
    const sess = {
      id: uid(),
      title: '新会话',
      updatedAt: Date.now(),
      messages: []
    }
    list.unshift(sess)
    sessionsCache.value[botId] = list
    activeSessionId.value = sess.id
    saveSessions(botId, list)
  }

  function deleteSession(id) {
    const botId = activeBotId.value
    const list = sessionsCache.value[botId]
    if (!list) return
    const idx = list.findIndex(s => s.id === id)
    if (idx > -1) list.splice(idx, 1)
    if (activeSessionId.value === id) {
      activeSessionId.value = list.length > 0 ? list[0].id : ''
    }
    saveSessions(botId, list)
  }

  // ---- 发送消息（走后端 SSE） ----
  function sendMessage(content) {
    if (!activeBot.value) return
    const botId = activeBotId.value

    if (!activeSession.value) createSession()

    const sess = activeSession.value
    if (!sess) return

    sess.messages.push({ id: uid(), role: 'user', content })
    if (sess.messages.length === 1) {
      sess.title = content.slice(0, 18)
    }
    sess.updatedAt = Date.now()
    saveSessions(botId, sessionsCache.value[botId])

    chatApi.send(botId, content)
      .then((resp) => {
        sess.messages.push({
          id: uid(),
          role: 'assistant',
          content: resp.text,
          toolCalls: resp.toolCalls || []
        })
        sess.updatedAt = Date.now()
        saveSessions(botId, sessionsCache.value[botId])
      })
      .catch(() => {
        sess.messages.push({ id: uid(), role: 'assistant', content: '（回复失败，请稍后重试）' })
        saveSessions(botId, sessionsCache.value[botId])
      })
  }

  return {
    bots, loading, error, activeBotId, activeSessionId,
    activeBot, sessions, activeSession,
    fetchBots, selectBot, selectSession,
    createBot, updateBot, deleteBot,
    createSession, deleteSession, sendMessage
  }
})
