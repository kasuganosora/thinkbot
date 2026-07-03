import { defineStore } from 'pinia'
import { ref, computed, triggerRef, shallowRef } from 'vue'
import { botApi, chatApi } from '@/api/services'

let idSeed = 1000
const uid = () => `id_${++idSeed}`

// 前端本地 session 管理（后端为 bot 级 chat/history，暂无 session CRUD）
function loadSessions(botId) {
  try {
    return JSON.parse(localStorage.getItem(`bp_sessions_${botId}`) || '[]')
  } catch { return [] }
}

// 防抖 saveSessions — 流式更新时避免高频 localStorage I/O
let _saveTimer = null
function saveSessions(botId, sessions) {
  localStorage.setItem(`bp_sessions_${botId}`, JSON.stringify(sessions))
}
function saveSessionsDebounced(botId, sessions) {
  clearTimeout(_saveTimer)
  _saveTimer = setTimeout(() => saveSessions(botId, sessions), 200)
}

export const useBotStore = defineStore('bot', () => {
  const bots = ref([])
  const loading = ref(false)
  const error = ref(null)
  const activeBotId = ref('')
  const activeSessionId = ref('')
  // shallowRef：不深度 reactive 化内部数组/对象，
  // 手动 triggerRef 控制更新时机，避免 computed 引用缓存导致下游不刷新
  const sessionsCache = shallowRef({})

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

  // 手动通知 sessionsCache 已变更（shallowRef 不自动追踪内部变化）
  function notifySessions() {
    triggerRef(sessionsCache)
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
  // 消息列表 — 使用独立版本计数器确保流式更新时 computed 能重算
  const _msgVersion = ref(0)
  const messages = computed(() => {
    void _msgVersion.value
    return activeSession.value?.messages || []
  })

  // ---- Bot 操作（走后端 API） ----
  function selectBot(id) {
    activeBotId.value = id
    // triggerRef 确保 sessions computed 能拿到新 botId 对应的数据
    notifySessions()
    const list = sessions.value
    activeSessionId.value = list.length > 0 ? list[0].id : ''
  }

  function selectSession(id) {
    activeSessionId.value = id
  }

  async function createBot(payload = {}) {
    const bot = await botApi.create({
      name: payload.name || '新建 Bot',
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
    notifySessions()
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
    notifySessions()
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
    notifySessions()
  }

  // ---- 发送消息（走后端 SSE 流式） ----
  const replying = ref(false)

  function sendMessage(content) {
    if (!activeBot.value || replying.value) return
    const botId = activeBotId.value

    if (!activeSession.value) createSession()

    const sess = activeSession.value
    if (!sess) return

    // 用户消息
    sess.messages.push({ id: uid(), role: 'user', content })
    if (sess.messages.length === 1) {
      sess.title = content.slice(0, 18)
    }
    sess.updatedAt = Date.now()
    _msgVersion.value++
    saveSessions(botId, sessionsCache.value[botId])

    // 流式 placeholder，逐字更新
    const msgId = uid()
    sess.messages.push({ id: msgId, role: 'assistant', content: '' })
    _msgVersion.value++
    saveSessions(botId, sessionsCache.value[botId])

    replying.value = true
    chatApi.send(botId, content, (delta) => {
      // onDelta 回调：追加文本到 assistant 消息
      const idx = sess.messages.findIndex(x => x.id === msgId)
      if (idx < 0) return
      const old = sess.messages[idx]
      // splice 替换元素 + 版本号递增 → 强制 messages computed 重算 → 模板重渲染
      sess.messages.splice(idx, 1, { ...old, content: old.content + delta })
      _msgVersion.value++
      // 流式期间防抖持久化，避免高频 localStorage 写入
      saveSessionsDebounced(botId, sessionsCache.value[botId])
    })
      .then((resp) => {
        const idx = sess.messages.findIndex(x => x.id === msgId)
        if (idx >= 0) {
          sess.messages.splice(idx, 1, {
            ...sess.messages[idx],
            content: resp.text || sess.messages[idx].content,
            toolCalls: resp.toolCalls || []
          })
          _msgVersion.value++
        }
        saveSessions(botId, sessionsCache.value[botId])
      })
      .catch(() => {
        const idx = sess.messages.findIndex(x => x.id === msgId)
        if (idx >= 0 && !sess.messages[idx].content) {
          sess.messages.splice(idx, 1, { ...sess.messages[idx], content: '（回复失败，请稍后重试）' })
          _msgVersion.value++
        }
        saveSessions(botId, sessionsCache.value[botId])
      })
      .finally(() => { replying.value = false })
  }

  return {
    bots, loading, error, replying, activeBotId, activeSessionId,
    activeBot, sessions, activeSession, messages,
    fetchBots, selectBot, selectSession,
    createBot, updateBot, deleteBot,
    createSession, deleteSession, sendMessage
  }
})
