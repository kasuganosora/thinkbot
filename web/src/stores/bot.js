import { defineStore } from 'pinia'
import { ref, computed, watch } from 'vue'
import { botApi, chatApi, sessionApi, workflowApi } from '@/api/services'
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
  // 每页消息数：首屏与上翻分页统一使用，与后端 defaultPageSize 保持一致
  const PAGE_SIZE = 20
  const messages = ref([])
  const messagesLoading = ref(false)
  // 上翻加载更早消息中（与首屏 messagesLoading 区分，供顶部加载指示器使用）
  const loadingMore = ref(false)
  const hasMore = ref(false)
  const nextCursor = ref('')
  // 首屏加载（进入会话/切换会话）完成后置 true，驱动 ChatWindow 强制滚到底部。
  // 与"上翻加载更早消息"区分——后者是 prepend，需保持当前滚动位置，不应触发滚底。
  const scrollToBottomOnLoad = ref(false)

  // SSE 流式状态
  const replying = ref(false)
  // 当前正在流式的 SSE AbortController（用于取消）
  let _abortController = null
  // 当前流式请求 traceId（用于后端主动中止）
  let _activeTraceId = ''
  // loadMessages 请求代号：单调递增，用于识别「已被更晚请求取代」的过期响应。
  // 多数调用方 fire-and-forget，快速切会话时慢响应会覆盖新会话消息。
  let _loadMessagesGen = 0

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
        } else {
          // 目标就是当前会话：仍刷新首屏（保证进入即展示最后一页=最新消息）
          await loadMessages()
        }
      } else {
        // 没有任何会话：清空消息，避免残留上一个 bot/会话的内容
        activeSessionId.value = null
        messages.value = []
        hasMore.value = false
        nextCursor.value = ''
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
    // 历史消息中的 running 工具需要按 streaming 标记区分：
    //   - streaming=true：该轮仍在产出（用户刷新页面回来），running 是真实状态，保留转圈。
    //   - 否则：该轮已收尾，仍是 running 说明进程中途死了、终态没写回来。
    //     必须降级掉，否则卡片会永久转圈。
    // 用 'killed' 而非自造的 'interrupted'：ToolCallCard/ToolCallGroup 只为
    // success/error/killed/timeout 提供了图标与配色，未知状态会掉到 default 显示异常。
    const settleRunning = (item) => (
      item && item.status === 'running' && !msg.streaming
        ? { ...item, status: 'killed', summary: item.summary || '回复已中断' }
        : item
    )

    if (Array.isArray(msg.parts) && msg.parts.length) {
      // 已有有序 parts（来自新格式 API 或流式构建）
      const parts = msg.parts.map(p => (p.type === 'tool' ? settleRunning(p) : p))
      const toolCalls = Array.isArray(msg.toolCalls) ? msg.toolCalls.map(settleRunning) : msg.toolCalls
      return { ...msg, parts, toolCalls }
    }
    const parts = []
    if (msg.content) parts.push({ type: 'text', content: msg.content })
    const calls = Array.isArray(msg.toolCalls) ? msg.toolCalls.map(settleRunning) : []
    for (const tc of calls) {
      parts.push({ type: 'tool', ...tc })
    }
    return { ...msg, parts, toolCalls: calls.length ? calls : msg.toolCalls }
  }

  async function loadMessages() {
    const botId = activeBotId.value
    if (!botId) {
      // 同样推进代号，作废在途请求，避免其响应回来后又把消息填回来
      _loadMessagesGen++
      messages.value = []
      return
    }
    // 记录发起请求时的 bot/会话与请求序号：多数调用方（selectSession /
    // createSession / deleteSession）是 fire-and-forget，快速切换会话时慢响应
    // 可能后到并覆盖新会话的消息（点了会话 B 却显示 A 的内容）。
    // 与 loadMoreMessages 保持同一防护思路。
    const reqBotId = botId
    const reqSessionId = activeSessionId.value
    const myGen = ++_loadMessagesGen
    // 仅「本次请求已被更晚的请求取代」才算过期；只比对 bot/session 会在
    // 「切走再切回同一会话」时误判为未过期，故以单调递增序号为准。
    const isStale = () => myGen !== _loadMessagesGen

    messagesLoading.value = true
    // 重新加载首屏时重置分页游标，避免沿用上个会话的游标
    hasMore.value = false
    nextCursor.value = ''
    try {
      const page = await chatApi.history(botId, null, PAGE_SIZE, reqSessionId)
      if (isStale()) return
      // 后端返回倒序（最新在前），前端需要正序（旧在前）
      messages.value = (page.messages || []).reverse().map(buildPartsForMessage)
      hasMore.value = page.hasMore || false
      nextCursor.value = page.nextCursor || ''
    } catch (e) {
      console.error('加载消息历史失败', e)
      if (!isStale()) messages.value = []
    } finally {
      // 只有最新一次请求负责关闭 loading：旧请求提前返回时不得误关，
      // 否则新会话仍在加载中却先显示空列表。
      if (!isStale()) messagesLoading.value = false
    }
    // 已被取代的请求不再触发续流与滚动副作用
    if (isStale()) return
    // 重连后恢复仍在后台运行的任务（断连不杀后台任务）：重连续流 + 允许手动终止
    await resumeInFlightTasks()
    if (isStale()) return
    // 恢复本会话的工作流卡片（刷新页面后 activeWorkflowId 会丢，见函数注释）
    await restoreSessionWorkflow(reqBotId, reqSessionId, isStale)
    if (isStale()) return
    // 首屏加载完成：通知 ChatWindow 强制滚到底部（进入应展示最后一页=最新消息）
    scrollToBottomOnLoad.value = true
  }

  /**
   * 恢复当前会话的工作流卡片。
   *
   * 为什么需要：`activeWorkflowId`只在 SSE 的 tool_progress / tool_result 里赋值，
   * 是纯内存态—— 刷新页面后它变回空串，工作流卡片就消失了，
   * 但工作流本身仍在后台运行（可能还要跑几十分钟）。
   * 这里在会话消息载入后按bot+会话查回最近一条工作流，把卡片挂回来。
   *
   * 只在当前没有 activeWorkflowId 时才恢复：实时 SSE 拿到的 ID 永远比查询结果新，
   * 不能被这个异步查询覆盖掉（用户可能已经开了新一轮工作流）。
   */
  async function restoreSessionWorkflow(reqBotId, reqSessionId, isStale) {
    if (!reqBotId) return
    if (activeWorkflowId.value) return
    try {
      const res = await workflowApi.sessionWorkflow(reqBotId, reqSessionId)
      // 请求期间可能已切走会话/新工作流已由 SSE 赋值，两种情况都不能覆盖
      if (isStale && isStale()) return
      if (activeWorkflowId.value) return
      const wid = res?.workflow?.id
      if (wid) activeWorkflowId.value = wid
    } catch {
      // 查不到工作流是常态（大多数会话没有工作流），静默即可
    }
  }

  /**
   * 向上翻页加载更早的一页消息（每页 PAGE_SIZE 条）。
   * 返回实际新增的条数，供调用方（ChatWindow）判断是否需要恢复滚动位置。
   */
  async function loadMoreMessages() {
    const botId = activeBotId.value
    if (!botId || !hasMore.value || messagesLoading.value || loadingMore.value) return 0
    loadingMore.value = true
    // 记录发起请求时的会话，响应回来若已切换会话则整页丢弃，防止串会话
    const reqBotId = botId
    const reqSessionId = activeSessionId.value
    try {
      const page = await chatApi.history(botId, nextCursor.value, PAGE_SIZE, reqSessionId)
      if (activeBotId.value !== reqBotId || activeSessionId.value !== reqSessionId) return 0
      const older = (page.messages || []).reverse().map(buildPartsForMessage)
      // 按 id 去重：游标边界可能返回与当前列表重叠的消息
      const existing = new Set(messages.value.map(m => m.id))
      const fresh = older.filter(m => !existing.has(m.id))
      if (fresh.length) messages.value = [...fresh, ...messages.value]
      hasMore.value = page.hasMore || false
      nextCursor.value = page.nextCursor || ''
      return fresh.length
    } catch (e) {
      console.error('加载更多消息失败', e)
      return 0
    } finally {
      loadingMore.value = false
    }
  }

  // 正在重连续流中的 traceID（防止 loadMessages 被多次触发时重复 resume）
  const _resuming = new Set()

  // 断连后重连：恢复仍在后台运行的任务（断连不腰斩后台长任务）。
  // 查询 activeTasks，对每条仍在跑的 traceID 重连续流并把进度渲染进一个占位 assistant 消息，
  // 同时设置 replying + _activeTraceId，使「停止生成」按钮可精确命中该任务予以终止。
  // 重连续流单个 traceID（后台仍在跑的任务 / 工作流终态后续跑）。
// 渲染逻辑与 sendMessage 一致，抽出来供 resumeInFlightTasks 与 resumeContinuation 复用。
async function _resumeTrace(traceId) {
  if (!traceId || _resuming.has(traceId)) return
  const botId = activeBotId.value
  if (!botId) return
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
      onToolProgress: (toolCallId, payload) => {
        appendToolProgress(assistantTmpId, toolCallId, payload)
        // 同上：阻塞式 task 的 workflowId 只能从进度事件拿到（result 要等到终态）
        const pid = extractWorkflowId(payload)
        if (pid) activeWorkflowId.value = pid
      },
      onToolResult: (toolCallId, payload) => {
        finishToolCall(assistantTmpId, toolCallId, payload)
        const wid = extractWorkflowId(payload)
        if (wid) activeWorkflowId.value = wid
        // 重连续流同样要合并状态快照，否则刷新后面板拿不到工作流实时状态
        mergeWorkflowSnapshot(wid, payload)
      },
    })
    // 重连续流正常结束：把占位消息转正
    const updated = [...messages.value]
    const idx = updated.findIndex(m => m.id === assistantTmpId)
    if (idx >= 0) updated[idx] = { ...updated[idx], _temp: false }
    messages.value = updated
  } catch (e) {
    console.warn('resume trace failed', traceId, e)
  } finally {
    replying.value = false
    _activeTraceId = ''
    _resuming.delete(traceId)
  }
}

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
    await _resumeTrace(traceId)
  }
}

// 工作流终态后续跑：后端已把工作流结果作为系统消息注入原会话
// （traceID = sessionID），这里按会话 resume 接收 agent 续跑的流式回复。
// 后端只在「阻塞等待方已超时/取消」时才注入，故正常情况下不会重复触发。
async function resumeContinuation(sessionId) {
  if (!sessionId) return
  await _resumeTrace(sessionId)
}

  // 切换 bot 时由 loadSessions 统一负责加载对应会话的消息（含首屏滚底）。
  // 早期此处额外挂了一个 loadMessages 的 watcher，会在 sessionId 尚未就绪时
  // 用空 session 发起一次查询并与正确查询竞态覆盖，已移除。

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

  // 只有 task_status 的返回才是工作流状态快照。
  //
  // 两个关键区分：
  //
  // 1. task 工具（创建工作流）也会返回一个含 status 的对象，形如
  //      { message: "工作流已创建，正在分析需求并分解任务...", status: "analyzing", workflowId: "wf-..." }
  //    那是**创建时刻的固定值**，不是实时状态。早期守卫只判断 payload.status 是否存在，
  //    于是这个永远为 "analyzing" 的创建返回被当成状态快照写进 activeWorkflowStatus，
  //    把面板头部永久钉在「分析中」——即便工作流早已 running、节点都跑完好几个。
  //
  // 2. payload.status 不是工作流状态，而是**工具执行状态**（success/error）：
  //    SSE 层在构造 tool_result 时会覆写它。工作流状态在 payload.output 里。
  function mergeWorkflowSnapshot(wid, payload) {
    if (!wid || !payload || typeof payload !== 'object') return
    if (payload.tool !== 'task_status') return
    // 防止旧 workflow 的残留快照覆盖当前活跃 workflow 的状态
    if (wid !== activeWorkflowId.value) return
    const snap = payload.output
    if (!snap || typeof snap !== 'object' || !snap.status) return
    activeWorkflowStatus.value = { ...snap, _ts: Date.now() }
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
        // task 工具「提交即阻塞」：tool_result 要等工作流跑完（可能数十分钟）才到达，
        // 因此必须从进度事件里就取到 workflowId，否则整个执行期间面板都不会出现。
        const pid = extractWorkflowId(payload)
        if (pid) activeWorkflowId.value = pid
      },
      onToolResult: (toolCallId, payload) => {
        finishToolCall(assistantTmpId, toolCallId, payload)
        // task 工具返回里携带 workflowId（如 "wf-xxxx"），提取后驱动工作流面板展示
        const wid = extractWorkflowId(payload)
        if (wid) activeWorkflowId.value = wid
        mergeWorkflowSnapshot(wid, payload)
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
    activeBot, messages, messagesLoading, loadingMore, hasMore,
    activeWorkflowId,
    activeWorkflowStatus,
    scrollToBottomOnLoad,
    fetchBots, selectBot,
    createBot, updateBot, deleteBot,
    loadMessages, loadMoreMessages, sendMessage, stopReply, appendToCurrentReply, resumeInFlightTasks, resumeContinuation,
    // 会话管理
    sessions, sessionsLoading, activeSessionId,
    loadSessions, createSession, deleteSession, selectSession,
    setPendingSession, openSessionById
  }
})
