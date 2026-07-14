import { defineStore } from 'pinia'
import { ref, computed, watch } from 'vue'
import { botApi, chatApi } from '@/api/services'

let idSeed = Date.now()
const uid = () => `_tmp_${++idSeed}`

export const useBotStore = defineStore('bot', () => {
  const bots = ref([])
  const loading = ref(false)
  const error = ref(null)
  const activeBotId = ref('')

  // ---- 消息 ----
  // 完全来自后端，不用 localStorage
  const messages = ref([])
  const messagesLoading = ref(false)
  const hasMore = ref(false)
  const nextCursor = ref('')

  // SSE 流式状态
  const replying = ref(false)
  // 当前正在流式的 SSE AbortController（用于取消）
  let _abortController = null

  // ---- Bot 列表 ----
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

  const activeBot = computed(() => bots.value.find(b => b.id === activeBotId.value))

  function selectBot(id) {
    // 切换 bot 时中止进行中的 SSE
    _abortStreaming()
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

  // ---- 消息加载（从后端 API） ----
  async function loadMessages() {
    const botId = activeBotId.value
    if (!botId) {
      messages.value = []
      return
    }
    messagesLoading.value = true
    try {
      const page = await chatApi.history(botId)
      // 后端返回倒序（最新在前），前端需要正序（旧在前）
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

  // 切换 bot 时重新加载消息
  watch(() => activeBotId.value, () => {
    loadMessages()
  })

  // ---- 中止流式 ----
  function _abortStreaming() {
    if (_abortController) {
      _abortController.abort()
      _abortController = null
    }
    replying.value = false
  }

  // ---- 发送消息 ----
  // @param {string} content 消息文本
  // @param {Array}  [attachments] 附件列表 [{name, type, size, dataUrl}]
  function sendMessage(content, attachments) {
    if (!activeBot.value || replying.value) return
    const botId = activeBotId.value

    // 1) 乐观追加用户消息（后端异步保存，不等待）
    const userTmpId = uid()
    const userMsg = {
      id: userTmpId,
      role: 'user',
      content,
      createdAt: new Date().toISOString(),
      _temp: true // 标记为临时消息
    }
    messages.value = [...messages.value, userMsg]

    // 2) 追加 assistant 占位
    const assistantTmpId = uid()
    const assistantMsg = {
      id: assistantTmpId,
      role: 'assistant',
      content: '',
      createdAt: new Date().toISOString(),
      _temp: true
    }
    messages.value = [...messages.value, assistantMsg]

    // 3) 启动 SSE 流式请求
    replying.value = true
    _abortController = new AbortController()

    chatApi.send(botId, content, (delta) => {
      // SSE text_delta：追加文本到 assistant 占位消息
      const idx = messages.value.findIndex(m => m.id === assistantTmpId)
      if (idx < 0) return
      const updated = [...messages.value]
      updated[idx] = { ...updated[idx], content: updated[idx].content + delta }
      messages.value = updated
    }, _abortController.signal, attachments || [])
      .then((resp) => {
        // SSE 完成：一次性更新 assistant 最终文本 + user 标记为非临时
        const updated = [...messages.value]
        const aIdx = updated.findIndex(m => m.id === assistantTmpId)
        if (aIdx >= 0) {
          updated[aIdx] = {
            ...updated[aIdx],
            content: resp.text || updated[aIdx].content,
            toolCalls: resp.toolCalls || [],
            _temp: false
          }
        }
        const uIdx = updated.findIndex(m => m.id === userTmpId)
        if (uIdx >= 0) {
          updated[uIdx] = { ...updated[uIdx], _temp: false }
        }
        messages.value = updated
      })
      .catch((err) => {
        if (err?.name === 'AbortError') return // 用户主动取消
        const idx = messages.value.findIndex(m => m.id === assistantTmpId)
        if (idx >= 0 && !messages.value[idx].content) {
          const updated = [...messages.value]
          updated[idx] = { ...updated[idx], content: '（回复失败，请稍后重试）', _temp: false }
          messages.value = updated
        }
      })
      .finally(() => {
        replying.value = false
        _abortController = null
      })
  }

  return {
    bots, loading, error, replying, activeBotId,
    activeBot, messages, messagesLoading, hasMore,
    fetchBots, selectBot,
    createBot, updateBot, deleteBot,
    loadMessages, loadMoreMessages, sendMessage
  }
})
