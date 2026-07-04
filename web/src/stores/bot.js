import { defineStore } from 'pinia'
import { ref, computed, watch } from 'vue'
import { botApi, chatApi } from '@/api/services'

let idSeed = Date.now()
const uid = () => `msg_${++idSeed}`

export const useBotStore = defineStore('bot', () => {
  const bots = ref([])
  const loading = ref(false)
  const error = ref(null)
  const activeBotId = ref('')

  // 消息列表 — 完全来自后端，不用 localStorage
  const messages = ref([])
  const messagesLoading = ref(false)
  const hasMore = ref(false)
  const nextCursor = ref('')

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

  // ---- Bot 操作 ----
  function selectBot(id) {
    activeBotId.value = id
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
      activeBotId.value = bots.value[0]?.id || ''
    }
  }

  // ---- 消息加载（从后端） ----
  async function loadMessages() {
    const botId = activeBotId.value
    if (!botId) {
      messages.value = []
      return
    }
    messagesLoading.value = true
    try {
      const page = await chatApi.history(botId)
      // 后端返回倒序（最新在前），前端显示需要正序（旧在前）
      messages.value = (page.messages || []).reverse()
      hasMore.value = page.hasMore || false
      nextCursor.value = page.nextCursor || ''
    } catch (e) {
      console.error('加载消息历史失败', e)
      messages.value = []
    } finally {
      messagesLoading.value = false
    }
  }

  // 加载更早的消息（上滑加载更多）
  async function loadMoreMessages() {
    const botId = activeBotId.value
    if (!botId || !hasMore.value || messagesLoading.value) return
    messagesLoading.value = true
    try {
      const page = await chatApi.history(botId, nextCursor.value)
      const older = (page.messages || []).reverse()
      messages.value = [...older, ...messages.value]
      hasMore.value = page.hasMore || false
      nextCursor.value = page.nextCursor || ''
    } catch (e) {
      console.error('加载更多消息失败', e)
    } finally {
      messagesLoading.value = false
    }
  }

  // 切换 bot 时自动加载消息
  watch(() => activeBotId.value, () => {
    loadMessages()
  })

  // ---- 发送消息（走后端 SSE 流式） ----
  const replying = ref(false)

  function sendMessage(content) {
    if (!activeBot.value || replying.value) return
    const botId = activeBotId.value

    // 乐观追加用户消息（后端异步保存，不需等待）
    const userMsg = { id: uid(), role: 'user', content, createdAt: new Date().toISOString() }
    messages.value = [...messages.value, userMsg]

    // 追加 assistant placeholder
    const assistantMsgId = uid()
    const assistantMsg = { id: assistantMsgId, role: 'assistant', content: '', createdAt: new Date().toISOString() }
    messages.value = [...messages.value, assistantMsg]

    replying.value = true
    chatApi.send(botId, content, (delta) => {
      // onDelta: 找到 assistant message 并追加文本
      const idx = messages.value.findIndex(m => m.id === assistantMsgId)
      if (idx < 0) return
      // 替换整个数组以确保 Vue 检测到变化（避免引用缓存问题）
      const updated = [...messages.value]
      updated[idx] = { ...updated[idx], content: updated[idx].content + delta }
      messages.value = updated
    })
      .then((resp) => {
        // 完成：用最终文本覆盖（确保一致性）
        const idx = messages.value.findIndex(m => m.id === assistantMsgId)
        if (idx >= 0) {
          const updated = [...messages.value]
          updated[idx] = {
            ...updated[idx],
            content: resp.text || updated[idx].content,
            toolCalls: resp.toolCalls || []
          }
          messages.value = updated
        }
      })
      .catch(() => {
        const idx = messages.value.findIndex(m => m.id === assistantMsgId)
        if (idx >= 0 && !messages.value[idx].content) {
          const updated = [...messages.value]
          updated[idx] = { ...updated[idx], content: '（回复失败，请稍后重试）' }
          messages.value = updated
        }
      })
      .finally(() => { replying.value = false })
  }

  return {
    bots, loading, error, replying, activeBotId,
    activeBot, messages, messagesLoading, hasMore,
    fetchBots, selectBot,
    createBot, updateBot, deleteBot,
    loadMessages, loadMoreMessages, sendMessage
  }
})
