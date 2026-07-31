import { defineStore } from 'pinia'
import { ref, computed, watch } from 'vue'
import { botApi, chatApi, sessionApi } from '@/api/services'
import toolLabels from '@/i18n/toolLabels'

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

  // ---- 会话（Session） ----
  const sessions = ref([])
  const sessionsLoading = ref(false)
  const activeSessionId = ref(null) // 当前选中的 session ID
  const pendingSessionId = ref(null) // URL / 外部指定的待选中 session（优先于自动选中）

  // 当前会话中由 bot 通过 task 工具创建的工作流 ID（驱动 SessionWorkflowPanel 展示）
  const activeWorkflowId = ref('')
  // SSE 推送的最新工作流快照（来自 task_status 工具结果）
  // SessionWorkflowPanel 监听此 ref 实时合并，避免纯轮询导致的头部卡片冻结
  const activeWorkflowStatus = ref(null)

  async function loadSessions(botId) {
    const bid = botId || activeBotId.value
    if (!bid) return
    sessionsLoading.value = true
    try {
      const res = await sessionApi.list(bid)
      sessions.value = res.sessions || []
      // 自动选中优先级：URL 指定(pendingSessionId) > 已选中的 > 列表第一个
      if (sessions.value.length > 0) {
        let target = null
        if (pendingSessionId.value && sessions.value.find(s => s.id === pendingSessionId.value)) {
          target = pendingSessionId.value
        } else if (activeSessionId.value && sessions.value.find(s => s.id === activeSessionId.value)) {
          target = activeSessionId.value
        } else {
          target = sessions.value[0].id
        }
        if (String(activeSessionId.value) !== String(target)) {
          activeSessionId.value = target
          await loadMessages()
        }
      }
    } catch (e) {
      console.error('loadSessions failed', e)
    } finally {
      pendingSessionId.value = null
      sessionsLoading.value = false
    }
  }

  async function createSession(title) {
    const bid = activeBotId.value
    if (!bid) return null
    try {
      const sess = await sessionApi.create(bid, title)
      sessions.value.unshift(sess)
      activeSessionId.value = sess.id
      loadMessages()
      return sess
    } catch (e) {
      console.error('createSession failed', e)
      throw e // 向上层抛，让调用方决定是否提示
    }
  }

  async function deleteSession(sid) {
    await sessionApi.remove(sid)
    sessions.value = sessions.value.filter(s => s.id !== sid)
    if (activeSessionId.value === sid) {
      activeSessionId.value = sessions.value.length > 0 ? sessions.value[0].id : null
      // 切到其他会话后加载消息
      if (activeSessionId.value) loadMessages()
    }
    return true // 成功
  }

  function selectSession(sid) {
    if (activeSessionId.value === sid) return
    activeSessionId.value = sid
    loadMessages()
  }

  function setPendingSession(id) {
    pendingSessionId.value = id || null
  }

  // 通过 URL 指定或外部调用选中某个 session（确保会话列表加载完成后再选中）
  async function openSessionById(sid) {
    if (!sid) return
    if (!sessions.value.length) {
      pendingSessionId.value = sid
      await loadSessions(activeBotId.value)
      return
    }
    const exists = sessions.value.find(s => s.id === sid)
    if (exists && String(activeSessionId.value) !== String(sid)) {
      selectSession(sid)
    }
  }

  // 当切换 bot 时自动加载该 bot 的会话列表
  watch(activeBotId, (newId) => {
    if (newId) loadSessions(newId)
  }, { immediate: true })

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
  /** 从后端 API 响应或 legacy {content, toolCalls} 构建有序 parts。
   *  - 后端返回 msg.parts（有序 parts 数组）→ 直接使用（保留文本/工具交错顺序）
   *  - 旧消息无 parts 字段 → 从 content + toolCalls 构建（降级：文本在前、工具在后）
   */
  function buildPartsForMessage(msg) {
    if (msg.role !== 'assistant') return msg
    if (Array.isArray(msg.parts) && msg.parts.length) return msg  // 已有有序 parts（来自新格式 API 或流式构建）
    const parts = []
    if (msg.content) parts.push({ type: 'text', content: msg.content })
    if (Array.isArray(msg.toolCalls)) {
      for (const tc of msg.toolCalls) {
        parts.push({ type: 'tool', ...tc })
      }
    }
    return { ...msg, parts }
  }

  async function loadMessages() {
    const botId = activeBotId.value
    if (!botId) {
      messages.value = []
      return
    }
    messagesLoading.value = true
    try {
      const page = await chatApi.history(botId, null, 30, activeSessionId.value)
      // 后端返回倒序（最新在前），前端需要正序（旧在前）
      messages.value = (page.messages || []).reverse().map(buildPartsForMessage)
      hasMore.value = page.hasMore || false
      nextCursor.value = page.nextCursor || ''
    } catch (e) {
      console.error('加载消息历史失败', e)
      messages.value = []
    } finally {
      messagesLoading.value = false
    }
    // 重连后恢复仍在后台运行的任务（断连不杀后台任务）：重连续流 + 允许手动终止
    await resumeInFlightTasks()
  }

  async function loadMoreMessages() {
    const botId = activeBotId.value
    if (!botId || !hasMore.value || messagesLoading.value) return
    messagesLoading.value = true
    try {
      const page = await chatApi.history(botId, nextCursor.value, 30, activeSessionId.value)
      const older = (page.messages || []).reverse().map(buildPartsForMessage)
      messages.value = [...older, ...messages.value]
      hasMore.value = page.hasMore || false
      nextCursor.value = page.nextCursor || ''
    } catch (e) {
      console.error('加载更多消息失败', e)
    } finally {
      messagesLoading.value = false
    }
  }

  // 正在重连续流中的 traceID（防止 loadMessages 被多次触发时重复 resume）
  const _resuming = new Set()

  // 断连后重连：恢复仍在后台运行的任务（断连不腰斩后台长任务）。
  // 查询 activeTasks，对每条仍在跑的 traceID 重连续流并把进度渲染进一个占位 assistant 消息，
  // 同时设置 replying + _activeTraceId，使「停止生成」按钮可精确命中该任务予以终止。
  async function resumeInFlightTasks() {
    const botId = activeBotId.value
    if (!botId) return
    let traceIds = []
    try {
      traceIds = await chatApi.activeTasks(botId)
    } catch {
      return
    }
    traceIds = traceIds.filter(id => id && !_resuming.has(id))
    if (!traceIds.length) return

    for (const traceId of traceIds) {
      _resuming.add(traceId)
      const assistantTmpId = uid()
      const assistantMsg = {
        id: assistantTmpId,
        role: 'assistant',
        content: '',
        toolCalls: [],
        parts: [],
        _temp: true,
      }
      messages.value = [...messages.value, assistantMsg]

      // 与 sendMessage 一致的累积辅助函数（本地内联，避免改动发送主路径）
      const appendTextPart = (tmpId, delta) => {
        const idx = messages.value.findIndex(m => m.id === tmpId)
        if (idx < 0) return
        const updated = [...messages.value]
        const msg = { ...updated[idx] }
        msg.content = (msg.content || '') + delta
        const parts = Array.isArray(msg.parts) ? [...msg.parts] : []
        const last = parts[parts.length - 1]
        if (last && last.type === 'text') {
          parts[parts.length - 1] = { ...last, content: last.content + delta }
        } else {
          parts.push({ type: 'text', content: delta })
        }
        msg.parts = parts
        updated[idx] = msg
        messages.value = updated
      }
      const upsertToolCall = (tmpId, call) => {
        const idx = messages.value.findIndex(m => m.id === tmpId)
        if (idx < 0) return
        const updated = [...messages.value]
        const msg = { ...updated[idx] }
        const toolCalls = Array.isArray(msg.toolCalls) ? [...msg.toolCalls] : []
        const existing = toolCalls.findIndex(t => t.id === call.id)
        if (existing >= 0) toolCalls[existing] = { ...toolCalls[existing], ...call }
        else toolCalls.push(call)
        msg.toolCalls = toolCalls
        updated[idx] = msg
        messages.value = updated
      }
      const appendToolProgress = (tmpId, toolCallId, payload) => {
        const idx = messages.value.findIndex(m => m.id === tmpId)
        if (idx < 0) return
        const updated = [...messages.value]
        const msg = { ...updated[idx] }
        const toolCalls = Array.isArray(msg.toolCalls) ? [...msg.toolCalls] : []
        const ti = toolCalls.findIndex(t => t.id === toolCallId)
        if (ti >= 0) {
          const t = { ...toolCalls[ti] }
          const out = (typeof t.output === 'object' && t.output) ? { ...t.output } : { stdout: '', stderr: '', exitCode: null, truncated: false }
          const stream = payload.stream === 'stderr' ? 'stderr' : 'stdout'
          out[stream] = (out[stream] || '') + (payload.chunk || '')
          t.output = out
          toolCalls[ti] = t
          msg.toolCalls = toolCalls
          updated[idx] = msg
          messages.value = updated
        }
      }
      const finishToolCall = (tmpId, toolCallId, payload) => {
        const idx = messages.value.findIndex(m => m.id === tmpId)
        if (idx < 0) return
        const updated = [...messages.value]
        const msg = { ...updated[idx] }
        const toolCalls = Array.isArray(msg.toolCalls) ? [...msg.toolCalls] : []
        const ti = toolCalls.findIndex(t => t.id === toolCallId)
        if (ti >= 0) {
          toolCalls[ti] = { ...toolCalls[ti], ...payload, status: payload.status || (payload.error != null ? 'error' : 'success') }
          msg.toolCalls = toolCalls
          updated[idx] = msg
          messages.value = updated
        }
      }

      replying.value = true
      _activeTraceId = traceId
      try {
        await chatApi.resume(botId, traceId, {
          onTextDelta: (delta) => appendTextPart(assistantTmpId, delta),
          onToolCall: (call) => upsertToolCall(assistantTmpId, call),
          onToolProgress: (toolCallId, payload) => appendToolProgress(assistantTmpId, toolCallId, payload),
          onToolResult: (toolCallId, payload) => {
            finishToolCall(assistantTmpId, toolCallId, payload)
            const wid = extractWorkflowId(payload)
            if (wid) activeWorkflowId.value = wid
          },
        })
        // 重连续流正常结束：把占位消息转正
        const updated = [...messages.value]
        const idx = updated.findIndex(m => m.id === assistantTmpId)
        if (idx >= 0) updated[idx] = { ...updated[idx], _temp: false }
        messages.value = updated
      } catch (e) {
        console.warn('resume in-flight task failed', traceId, e)
      } finally {
        replying.value = false
        _activeTraceId = ''
        _resuming.delete(traceId)
      }
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
      // 同步 parts 中 running → killed
      const parts = Array.isArray(m.parts) ? [...m.parts] : []
      let changed = false
      for (let pi = 0; pi < parts.length; pi++) {
        if (parts[pi].type === 'tool' && parts[pi].status === 'running') {
          parts[pi] = { ...parts[pi], status: 'killed', summary: parts[pi].summary || '用户已停止' }
          changed = true
        }
      }
      if (changed) updated[i] = { ...updated[i], parts }
      break
    }
    messages.value = updated
    replying.value = false
  }

  /** 供 UI 中断按钮调用 */
  function stopReply() { _abortStreaming() }

  /**
   * 生成中追加：把用户中途补充的内容注入「同一轮」对话（Claude-CLI 风格）。
   * 与 sendMessage 的区别：不开启新一轮，而是调用 /api/chat/append，
   * 当前流式回复会继续结合这条补充继续生成。若本轮已结束（后端 accepted=false）
   * 或当前并不在生成中，则退化为一次普通的 sendMessage。
   * @param {string} content
   */
  function appendToCurrentReply(content) {
    const text = (content || '').trim()
    if (!text) return
    const traceId = _activeTraceId
    const botId = activeBotId.value
    // 不在生成中，或本轮已结束 → 退化为普通发送
    if (!replying.value || !traceId || !botId) {
      sendMessage(text)
      return
    }
    // 立即在本地渲染这条补充为用户消息气泡（与后端落库一致）
    const userMsg = {
      id: uid(),
      role: 'user',
      content: text,
      createdAt: new Date().toISOString(),
      _temp: false,
      _appended: true,
    }
    messages.value = [...messages.value, userMsg]
    chatApi.append(botId, traceId, text, activeSessionId.value)
      .then((resp) => {
        if (!resp || resp.accepted === false) {
          // 本轮已结束，降级为普通发送
          messages.value = messages.value.filter(m => m.id !== userMsg.id)
          sendMessage(text)
        }
      })
      .catch(() => {
        messages.value = messages.value.filter(m => m.id !== userMsg.id)
        sendMessage(text)
      })
  }

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
    const part = {
      id: call.id,
      name: call.name,
      title: call.title || call.name,
      status: call.status || 'running',
      input: call.input,
      output: normalizeToolOutput(call.output),
    }
    if (idx < 0) {
      list.push(part)
      // ── 有序 parts 追加工具 part ──
      const parts = Array.isArray(msg.parts) ? [...msg.parts] : []
      parts.push({ type: 'tool', ...part })
      msg.parts = parts
    } else {
      const prev = list[idx]
      list[idx] = { ...prev, ...part, output: normalizeToolOutput(call.output, normalizeToolOutput(prev.output)) }
      // 同步更新 parts 中的对应工具 part
      const parts = Array.isArray(msg.parts) ? [...msg.parts] : []
      const pIdx = parts.findIndex(p => p.type === 'tool' && p.id === call.id)
      if (pIdx >= 0) {
        parts[pIdx] = { ...parts[pIdx], ...list[idx] }
        msg.parts = parts
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

    // 同步 parts 中的对应工具 part
    syncPartFromToolCall(msg, toolCallId, call)

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
      list.push({ id: toolCallId, name: payload?.tool, title: toolLabels[payload?.tool] || payload?.tool, status: 'running', output: normalizeToolOutput(null) })
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

    // 同步 parts 中的对应工具 part
    syncPartFromToolCall(msg, toolCallId, call)

    msg.toolCalls = list
    updated[mIdx] = msg
    messages.value = updated
  }

  /** 同步 parts 数组中指定工具 part 的状态（供 appendToolProgress / finishToolCall 复用） */
  function syncPartFromToolCall(msg, toolCallId, call) {
    const parts = Array.isArray(msg.parts) ? [...msg.parts] : []
    const pIdx = parts.findIndex(p => p.type === 'tool' && p.id === toolCallId)
    if (pIdx >= 0) {
      parts[pIdx] = { ...parts[pIdx], ...call }
      msg.parts = parts
    }
  }

  // ---- 发送消息 ----
  // @param {string} content 消息文本
  // @param {Array}  [attachments] 附件列表 [{name, type, size, dataUrl}]
  // 从工具返回 payload 中递归提取工作流 ID（task 工具返回 {workflowId:"wf-..."}）
  function extractWorkflowId(payload) {
    if (!payload || typeof payload !== 'object') return ''
    const stack = [payload]
    while (stack.length) {
      const cur = stack.pop()
      if (Array.isArray(cur)) { for (const it of cur) stack.push(it); continue }
      if (cur && typeof cur === 'object') {
        for (const [k, v] of Object.entries(cur)) {
          if (typeof v === 'string' && /^wf-[\w-]+$/.test(v)) return v
          if ((k === 'workflowId' || k === 'taskId') && typeof v === 'string' && v) return v
          if (v && typeof v === 'object') stack.push(v)
        }
      }
    }
    return ''
  }

  function sendMessage(content, attachments) {
    if (!activeBot.value || replying.value) return
    // 新对话开始：清空上一轮工作流面板（task 触发时再重新显示）
    activeWorkflowId.value = ''
    activeWorkflowStatus.value = null
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
      parts: [],           // 有序 part 列表：type:'text' | type:'tool'，按 LLM 调用顺序排列
      createdAt: new Date().toISOString(),
      _temp: true
    }
    messages.value = [...messages.value, assistantMsg]

    replying.value = true
    _abortController = new AbortController()
    _activeTraceId = ''

    /** 向 assistant 消息的有序 parts 中追加/合并文本内容 */
    function appendTextPart(tmpId, delta) {
      const idx = messages.value.findIndex(m => m.id === tmpId)
      if (idx < 0) return
      const updated = [...messages.value]
      const msg = { ...updated[idx] }
      // 始终同步更新 content 字段（向后兼容）
      msg.content = (msg.content || '') + delta
      // 有序 parts：合并到最后一个 text part 或新建
      const parts = Array.isArray(msg.parts) ? [...msg.parts] : []
      const last = parts[parts.length - 1]
      if (last && last.type === 'text') {
        parts[parts.length - 1] = { ...last, content: last.content + delta }
      } else {
        parts.push({ type: 'text', content: delta })
      }
      msg.parts = parts
      updated[idx] = msg
      messages.value = updated
    }

    chatApi.send(botId, content, {
      sessionId: activeSessionId.value,
      onStart: (traceId) => {
        _activeTraceId = traceId || ''
      },
      onTextDelta: (delta) => {
        appendTextPart(assistantTmpId, delta)
      },
      onToolCall: (call) => {
        upsertToolCall(assistantTmpId, call)
      },
      onToolProgress: (toolCallId, payload) => {
        appendToolProgress(assistantTmpId, toolCallId, payload)
      },
      onToolResult: (toolCallId, payload) => {
        finishToolCall(assistantTmpId, toolCallId, payload)
        // task 工具返回里携带 workflowId（如 "wf-xxxx"），提取后驱动工作流面板展示
        const wid = extractWorkflowId(payload)
        if (wid) activeWorkflowId.value = wid
        // task_status 工具返回的是完整 GetStatus JSON（含 goalIteration/analyzeMessage/status 等）
        // 将其写入 activeWorkflowStatus 供 SessionWorkflowPanel 实时合并，解决头部卡片与工具结果不同步
        // 守卫：只接受当前活跃 workflow 的状态快照，防止旧 workflow 的残留脏数据覆盖新状态
        if (wid && payload && typeof payload === 'object' && payload.status && wid === activeWorkflowId.value) {
          activeWorkflowStatus.value = { ...payload, _ts: Date.now() }
        }
      },
      signal: _abortController.signal,
      attachments: attachments || [],
    })
      .then((resp) => {
        if (resp?.traceId) _activeTraceId = resp.traceId
        const updated = [...messages.value]
        const aIdx = updated.findIndex(m => m.id === assistantTmpId)
        if (aIdx >= 0) {
          const finalContent = resp.text || updated[aIdx].content
          const base = { ...updated[aIdx], content: finalContent, _temp: false }
          // 确保.parts 中最后一个 text part 的内容与最终 content 一致
          const parts = Array.isArray(base.parts) ? [...base.parts] : []
          if (parts.length && parts[parts.length - 1].type === 'text') {
            parts[parts.length - 1] = { ...parts[parts.length - 1], content: finalContent }
          } else if (finalContent) {
            parts.push({ type: 'text', content: finalContent })
          }
          base.parts = parts
          updated[aIdx] = base
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
          const failText = '（回复失败，请稍后重试）'
          updated[idx] = {
            ...updated[idx],
            content: failText,
            _temp: false,
            parts: [{ type: 'text', content: failText }]
          }
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
    activeWorkflowId,
    activeWorkflowStatus,
    fetchBots, selectBot,
    createBot, updateBot, deleteBot,
    loadMessages, loadMoreMessages, sendMessage, stopReply, appendToCurrentReply, resumeInFlightTasks,
    // 会话管理
    sessions, sessionsLoading, activeSessionId,
    loadSessions, createSession, deleteSession, selectSession,
    setPendingSession, openSessionById
  }
})
