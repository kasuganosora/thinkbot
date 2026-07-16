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
  // 当前流式请求 traceId（用于后端主动中止）
  let _activeTraceId = ''

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
    const traceId = _activeTraceId
    const botId = activeBotId.value
    _activeTraceId = ''
    if (traceId && botId) {
      chatApi.abort(botId, traceId).catch(() => {})
    }

    if (_abortController) {
      _abortController.abort()
      _abortController = null
    }
    const updated = [...messages.value]
    for (let i = updated.length - 1; i >= 0; i--) {
      const m = updated[i]
      if (m.role !== 'assistant') continue
      if (!Array.isArray(m.toolCalls) || !m.toolCalls.length) break
      const calls = m.toolCalls.map(tc => tc.status === 'running'
        ? { ...tc, status: 'killed', summary: tc.summary || '用户已停止' }
        : tc)
      updated[i] = { ...m, toolCalls: calls }
      break
    }
    messages.value = updated
    replying.value = false
  }

  /** 供 UI 中断按钮调用 */
  function stopReply() { _abortStreaming() }

  function truncateTail(text, max = 200 * 1024) {
    if (!text || text.length <= max) return { value: text || '', truncated: false }
    return { value: text.slice(text.length - max), truncated: true }
  }

  function normalizeToolOutput(output, fallback = null) {
    if (output && typeof output === 'object') {
      return {
        stdout: output.stdout || '',
        stderr: output.stderr || '',
        exitCode: output.exitCode ?? null,
        truncated: output.truncated === true,
        workdir: output.workdir || output.cwd || '',
      }
    }
    if (fallback && typeof fallback === 'object') return { ...fallback }
    return { stdout: '', stderr: '', exitCode: null, truncated: false }
  }

  function upsertToolCall(messageId, call) {
    const mIdx = messages.value.findIndex(m => m.id === messageId)
    if (mIdx < 0 || !call?.id) return
    const updated = [...messages.value]
    const msg = { ...updated[mIdx] }
    const list = Array.isArray(msg.toolCalls) ? [...msg.toolCalls] : []
    const idx = list.findIndex(x => x.id === call.id)
    if (idx < 0) {
      list.push({
        id: call.id,
        name: call.name,
        title: call.title || call.name,
        status: call.status || 'running',
        input: call.input,
        output: normalizeToolOutput(call.output),
      })
    } else {
      const prev = list[idx]
      list[idx] = {
        ...prev,
        ...call,
        output: normalizeToolOutput(call.output, normalizeToolOutput(prev.output)),
      }
    }
    msg.toolCalls = list
    updated[mIdx] = msg
    messages.value = updated
  }

  function appendToolProgress(messageId, toolCallId, payload) {
    const mIdx = messages.value.findIndex(m => m.id === messageId)
    if (mIdx < 0 || !toolCallId) return
    const updated = [...messages.value]
    const msg = { ...updated[mIdx] }
    const list = Array.isArray(msg.toolCalls) ? [...msg.toolCalls] : []
    const idx = list.findIndex(x => x.id === toolCallId)
    if (idx < 0) return

    const stream = payload?.stream === 'stderr' ? 'stderr' : 'stdout'
    const chunk = payload?.chunk || ''
    const call = { ...list[idx] }
    const out = normalizeToolOutput(call.output)
    const merged = (out[stream] || '') + chunk
    const { value, truncated } = truncateTail(merged)
    out[stream] = value
    if (truncated) out.truncated = true
    call.output = out
    call.status = call.status || 'running'
    list[idx] = call

    msg.toolCalls = list
    updated[mIdx] = msg
    messages.value = updated
  }

  function finishToolCall(messageId, toolCallId, payload) {
    const mIdx = messages.value.findIndex(m => m.id === messageId)
    if (mIdx < 0 || !toolCallId) return
    const updated = [...messages.value]
    const msg = { ...updated[mIdx] }
    const list = Array.isArray(msg.toolCalls) ? [...msg.toolCalls] : []
    let idx = list.findIndex(x => x.id === toolCallId)
    if (idx < 0) {
      list.push({ id: toolCallId, name: payload?.tool, title: payload?.tool, status: 'running', output: normalizeToolOutput(null) })
      idx = list.length - 1
    }

    const call = { ...list[idx] }
    const isErr = payload?.error != null
    call.status = payload?.status || (isErr ? 'error' : 'success')
    if (payload?.summary != null) call.summary = payload.summary

    const prevOut = normalizeToolOutput(call.output)
    if (payload?.output !== undefined) {
      call.output = normalizeToolOutput(payload.output, prevOut)
    } else {
      call.output = prevOut
    }
    if (isErr) {
      const mergedErr = (call.output.stderr || '') + String(payload.error)
      const { value, truncated } = truncateTail(mergedErr)
      call.output.stderr = value
      if (truncated) call.output.truncated = true
    }

    list[idx] = call
    msg.toolCalls = list
    updated[mIdx] = msg
    messages.value = updated
  }

  // ---- 发送消息 ----
  // @param {string} content 消息文本
  // @param {Array}  [attachments] 附件列表 [{name, type, size, dataUrl}]
  function sendMessage(content, attachments) {
    if (!activeBot.value || replying.value) return
    const botId = activeBotId.value

    const userTmpId = uid()
    const userMsg = {
      id: userTmpId,
      role: 'user',
      content,
      createdAt: new Date().toISOString(),
      _temp: true
    }
    messages.value = [...messages.value, userMsg]

    const assistantTmpId = uid()
    const assistantMsg = {
      id: assistantTmpId,
      role: 'assistant',
      content: '',
      toolCalls: [],
      createdAt: new Date().toISOString(),
      _temp: true
    }
    messages.value = [...messages.value, assistantMsg]

    replying.value = true
    _abortController = new AbortController()
    _activeTraceId = ''

    chatApi.send(botId, content, {
      onStart: (traceId) => {
        _activeTraceId = traceId || ''
      },
      onTextDelta: (delta) => {
        const idx = messages.value.findIndex(m => m.id === assistantTmpId)
        if (idx < 0) return
        const updated = [...messages.value]
        updated[idx] = { ...updated[idx], content: (updated[idx].content || '') + delta }
        messages.value = updated
      },
      onToolCall: (call) => {
        upsertToolCall(assistantTmpId, call)
      },
      onToolProgress: (toolCallId, payload) => {
        appendToolProgress(assistantTmpId, toolCallId, payload)
      },
      onToolResult: (toolCallId, payload) => {
        finishToolCall(assistantTmpId, toolCallId, payload)
      },
      signal: _abortController.signal,
      attachments: attachments || [],
    })
      .then((resp) => {
        if (resp?.traceId) _activeTraceId = resp.traceId
        const updated = [...messages.value]
        const aIdx = updated.findIndex(m => m.id === assistantTmpId)
        if (aIdx >= 0) {
          updated[aIdx] = {
            ...updated[aIdx],
            content: resp.text || updated[aIdx].content,
            _temp: false
          }
        }
        const uIdx = updated.findIndex(m => m.id === userTmpId)
        if (uIdx >= 0) {
          updated[uIdx] = { ...updated[uIdx], _temp: false }
        }
        messages.value = updated

        if (Array.isArray(resp.toolCalls)) {
          for (const tc of resp.toolCalls) {
            upsertToolCall(assistantTmpId, tc)
            finishToolCall(assistantTmpId, tc.id, tc)
          }
        }
      })
      .catch((err) => {
        if (err?.name === 'AbortError') return
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
        _activeTraceId = ''
      })
  }

  return {
    bots, loading, error, replying, activeBotId,
    activeBot, messages, messagesLoading, hasMore,
    fetchBots, selectBot,
    createBot, updateBot, deleteBot,
    loadMessages, loadMoreMessages, sendMessage, stopReply
  }
})
