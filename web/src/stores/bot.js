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

  // ---- 用户选择卡片（user_choice 工具，n7 契约）----
  // choices: Map<questionId, { payload, submitted, status }>（reactive 包 Map，取值始终 .value.get(...)）
  // 设计意图：
  //  - 与 workflowId 锚定机制完全同构：消息上打 questionId 标记，渲染层据此内联卡片；
  //  - 状态放 store 而非组件：提交成功/终态要在"刷新页面后"仍可恢复（submitStatus 持久化于消息），
  //    且同题多卡片实例（理论上不该出现）共享同一状态源，不会各自为政。
  const choices = ref(new Map())
  // 本会话内已成功提交的 questionId 集合（内联渲染判重的轻量索引）
  const submittedChoiceIds = ref(new Set())

  /**
   * 从 tool_progress 事件 payload 里提取 user_choice 卡片负载。
   *
   * 实际线上形态（**别按想象写**，踩过一次）：后端
   * `PublishToolProgress` 发的是 `{toolCallId, tool, invocationId, payload:{...}}`，
   * 而 services.js 的 tool_progress 分支把内层 `parts.payload` **展开**后
   * 才交给回调（额外补 stream/chunk），且**不带** `tool` 字段。所以到这里的
   * payload 是扁平的 UserChoiceEventPayload（见 tools/user_choice.go）：
   *   { type:'user_choice', questionId, question, options:[{id,label,description}],
   *     mode:'single'|'multi', inputHint, timeout, timeoutAt, via, ... }
   * → 判别键是 **type**，不是 tool（曾按 tool 判，导致卡片永远不注册）。
   *
   * 另外两种形态作为兼容保留：显式 choice 包裹；以及 tool 字段确实存在的调用方。
   * 提取失败返回 null（绝大多数工具进度事件都不是选择卡）。
   */
  function extractChoicePayload(payload) {
    if (!payload || typeof payload !== 'object') return null
    // 形态一：显式 choice 包裹
    const c = payload.choice
    if (c && typeof c === 'object' && c.questionId != null) {
      return normalizeChoicePayload(c)
    }
    // 形态二：内层 payload 未被展开（防御：上游若改成整体透传也能认）
    const inner = payload.payload
    if (inner && typeof inner === 'object' && inner.type === 'user_choice' && inner.questionId != null) {
      return normalizeChoicePayload(inner)
    }
    // 形态三：扁平展开（当前真实形态）
    const isChoice = payload.type === 'user_choice' || payload.tool === 'user_choice'
    if (isChoice && payload.questionId != null) {
      return normalizeChoicePayload(payload)
    }
    return null
  }

  /**
   * 把各形态的选择卡负载归一成 ChoiceCard 的 props 契约。
   * mode 统一成 'single' | 'multiple'：后端枚举是 single/multi（见 user_choice 工具
   * 的 mode enum），卡片组件按 'multiple' 判多选——不翻译的话多选题会退化成单选。
   * options 统一成 [{id,label,description}]，缺 id 时按下标补 `o{i}`
   * （与 interaction.RegisterQuestion 的补齐规则一致，见 internal/interaction）。
   */
  function normalizeChoicePayload(src) {
    const rawMode = String(src.mode || '')
    const mode = (rawMode === 'multi' || rawMode === 'multiple') ? 'multiple' : 'single'
    const options = Array.isArray(src.options)
      ? src.options.map((o, i) => (o && typeof o === 'object'
        ? { id: String(o.id ?? `o${i}`), label: String(o.label ?? ''), description: o.description || '' }
        : { id: `o${i}`, label: String(o ?? ''), description: '' }))
      : []
    return {
      questionId: String(src.questionId),
      question: src.question || '',
      mode,
      options,
      inputHint: src.inputHint || src.input_hint || '',
      timeoutAt: src.timeoutAt ?? src.timeout_at ?? null,
      // pending 必须显式布尔：registerChoice 做 {...prev.payload, ...payload} 合并，
      // 临时卡 pending:true 若被正式卡省略该字段，合并会一直保持 pending。
      pending: Boolean(src.pending),
    }
  }

  /**
   * 注册/更新一张选择卡：写入 choices、并把 questionId 锚定到承载它的 assistant 消息
   * （与 tagMessageWorkflow 同构）。重复事件（SSE 重放）幂等。
   *
   * 同一 toolCallId 可能先注册临时卡（onToolCall，questionId=call.id 占位）、
   * 后被正式卡（onToolProgress，服务端生成的真实 questionId）覆盖。
   * 此函数负责清理已被新 questionId 取代的旧条目，防止 choiceIdByToolCallId
   * 命中过期的临时数据。
   *
   * @param {string} messageId - assistant 消息 ID
   * @param {object} payload - normalizeChoicePayload 产物（含 questionId）
   * @param {string} [toolCallId] - 触发此卡的工具调用 ID（用于内联渲染到对应 ToolCallCard 后）
   */
  function registerChoice(messageId, payload, toolCallId) {
    const qid = payload?.questionId
    if (!qid) return
    const next = new Map(choices.value)
    const prev = next.get(qid) || {}
    next.set(qid, {
      payload: { ...prev.payload, ...payload },
      submitted: prev.submitted || false,
      status: payload?.status || prev.status || '',
      toolCallId: toolCallId || prev.toolCallId || '',
    })
    // 安全网：如果新 payload 带有真实的服务端生成 questionId（uc- 前缀），
    // 强制清除 pending。防止 SSE 透传/合并边界条件下 pending:true 残留导致卡片永久锁死。
    // 注意：call_ 前缀是 tempChoiceFromInput 的占位符，此时清 pending 会导致
    // 卡片过早可交互、用户点击后仍 404（真实 ID 尚未到达），所以不在此处清除。
    const merged = next.get(qid)
    if (merged && merged.payload && merged.payload.questionId.startsWith('uc-')) {
      merged.payload = { ...merged.payload, pending: false }
    }
    // 清理同一 toolCallId 的旧条目（临时卡被正式卡取代时的遗留）
    if (toolCallId) {
      for (const [oldQid, c] of next) {
        if (oldQid !== qid && c.toolCallId === toolCallId) {
          next.delete(oldQid)
          // 同步从消息的 questionIds 中移除旧条目，防止孤儿渲染或 choiceIdByToolCallId 命中过期数据
          const mIdx = messages.value.findIndex(m => m.id === messageId)
          if (mIdx >= 0) {
            const updated = [...messages.value]
            const msg = { ...updated[mIdx] }
            if (Array.isArray(msg.questionIds)) {
              msg.questionIds = msg.questionIds.filter(id => id !== oldQid)
              updated[mIdx] = msg
              messages.value = updated
            }
          }
          break // 同一 toolCallId 最多一条旧条目
        }
      }
    }
    choices.value = next
    tagMessageChoice(messageId, qid)
  }

  /** 把 questionId 写进进行中的 tool call，刷新后 choicePayloadFromTool 才能恢复未完成的卡 */
  function stampChoiceQuestionId(messageId, toolCallId, questionId) {
    if (!messageId || !toolCallId || !questionId) return
    const mIdx = messages.value.findIndex(m => m.id === messageId)
    if (mIdx < 0) return
    const updated = [...messages.value]
    const msg = { ...updated[mIdx] }
    const list = Array.isArray(msg.toolCalls) ? [...msg.toolCalls] : []
    const idx = list.findIndex(x => x.id === toolCallId)
    if (idx < 0) return
    const prev = list[idx]
    const out = (prev.output && typeof prev.output === 'object') ? { ...prev.output } : {}
    out.questionId = questionId
    const call = { ...prev, questionId, output: out }
    list[idx] = call
    msg.toolCalls = list
    const parts = Array.isArray(msg.parts) ? [...msg.parts] : []
    const pIdx = parts.findIndex(p => p.type === 'tool' && p.id === toolCallId)
    if (pIdx >= 0) {
      parts[pIdx] = { ...parts[pIdx], questionId, output: out }
    } else {
      parts.push({ type: 'tool', ...call })
    }
    msg.parts = parts
    updated[mIdx] = msg
    messages.value = updated
  }

  /** 根据 toolCallId 查找对应的 choice questionId（内联渲染用） */
  function choiceIdByToolCallId(toolCallId) {
    if (!toolCallId) return null
    const want = String(toolCallId)
    for (const [qid, c] of choices.value) {
      if (c && c.toolCallId != null && String(c.toolCallId) === want) return qid
    }
    return null
  }

  /**
   * 从 tool_result 载荷里提取 user_choice 的终态信息。
   * n7 契约：user_choice 工具的 result.output 形如
   *   { status: 'answered'|'timeout'|'cancelled', questionId, selectedIds, freeText }
   * 提取不到时返回 null（绝大多数工具结果都不是选择卡）。
   */
  function extractChoiceTerminal(payload) {
    if (!payload || typeof payload !== 'object') return null
    // 只关心 user_choice 工具的结果：按工具名过滤，防止误伤其他工具
    const tool = String(payload.tool || '')
    const out = payload.output
    const isChoiceResult = tool === 'user_choice' || tool.endsWith(':user_choice')
      || (out && typeof out === 'object' && out.questionId != null && (out.status === 'answered' || out.status === 'timeout' || out.status === 'cancelled'))
    if (!isChoiceResult) return null
    if (!out || typeof out !== 'object') return null
    const qid = out.questionId != null ? String(out.questionId) : ''
    if (!qid) return null
    return {
      payload: { questionId: qid },
      status: out.status || (payload.status === 'error' ? 'cancelled' : 'answered'),
      // 用户实际作答内容（终态卡片回显用；本地已提交时通常与本地状态一致，仅作佐证）
      answer: {
        selectedIds: Array.isArray(out.selectedIds) ? out.selectedIds.map(String) : [],
        freeText: typeof out.freeText === 'string' ? out.freeText : '',
      },
    }
  }

  /** 把 questionId 锚定到 assistant 消息（渲染层据此内联 ChoiceCard） */
  function tagMessageChoice(messageId, qid) {
    if (!qid) return
    const mIdx = messages.value.findIndex(m => m.id === messageId)
    if (mIdx < 0) return
    const updated = [...messages.value]
    const msg = { ...updated[mIdx] }
    if (Array.isArray(msg.questionIds) && msg.questionIds.includes(qid)) return
    msg.questionIds = Array.isArray(msg.questionIds) ? [...msg.questionIds, qid] : [qid]
    updated[mIdx] = msg
    messages.value = updated
  }

  /** 标记某题已成功提交（本地提交成功即调用；后端 tool_result 佐证时也调用） */
  function markChoiceSubmitted(qid, answer) {
    if (!qid) return
    const next = new Set(submittedChoiceIds.value)
    next.add(qid)
    submittedChoiceIds.value = next
    const nextMap = new Map(choices.value)
    const prev = nextMap.get(qid)
    // answer（用户实际所选）随状态一并保存：终态卡片要显示"选了什么"，
    // 刷新后本地 ref 已丢，必须能在 tool_result 到达/历史恢复时回填
    if (prev) nextMap.set(qid, { ...prev, submitted: true, status: 'answered', answer: answer || prev.answer || null })
    choices.value = nextMap
  }

  /** 工具终态落库：timeout/cancelled/resolved 等终态同步到卡片状态 */
  function markChoiceTerminal(qid, status) {
    if (!qid || !choices.value.has(qid)) return
    const next = new Map(choices.value)
    const prev = next.get(qid)
    next.set(qid, { ...prev, status })
    choices.value = next
  }

  /** 切换 bot / 新对话时清空选择卡状态（防止旧会话卡片串门） */
  function resetChoices() {
    choices.value = new Map()
    submittedChoiceIds.value = new Set()
  }

  /** 按 questionId 查询选择卡状态（渲染层消费） */
  function choiceState(qid) {
    return choices.value.get(String(qid)) || null
  }


  /** 按 ID 查找会话（字符串比较，兼容数字/字符串两种 ID 形态） */
  function findSession(sid) {
    if (sid == null || sid === '') return undefined
    return sessions.value.find(s => String(s.id) === String(sid))
  }

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
        // ID 一律按字符串比较：URL 里带来的 sessionId 是字符串，列表里的可能是数字，
        // 严格相等会漏匹配（表现为深链接指定的会话选不中）。
        if (pendingSessionId.value && findSession(pendingSessionId.value)) {
          target = pendingSessionId.value
        } else if (activeSessionId.value && findSession(activeSessionId.value)) {
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
    sessions.value = sessions.value.filter(s => String(s.id) !== String(sid))
    if (String(activeSessionId.value) === String(sid)) {
      activeSessionId.value = sessions.value.length > 0 ? sessions.value[0].id : null
      // 切到其他会话后加载消息
      if (activeSessionId.value) loadMessages()
    }
    return true // 成功
  }

  function isPlaceholderTitle(t) {
    const v = String(t || '').trim()
    return !v || v === '新会话' || v === '默认会话'
  }

  function titleFromFirstMessage(text) {
    let v = String(text || '').replace(/\s+/g, ' ').trim()
    if (!v || v.startsWith('/')) return ''
    if (v === '[附件]') return '附件'
    const chars = Array.from(v)
    return chars.length > 30 ? chars.slice(0, 30).join('') + '…' : v
  }

  function patchSession(sid, fields) {
    const idx = sessions.value.findIndex(x => String(x.id) === String(sid))
    if (idx < 0) return
    const updated = [...sessions.value]
    updated[idx] = { ...updated[idx], ...fields }
    sessions.value = updated
  }

  async function renameSession(sid, title) {
    const t = String(title || '').trim()
    if (!sid || !t) return null
    const capped = Array.from(t).slice(0, 40).join('')
    const sess = await sessionApi.update(sid, { title: capped })
    patchSession(sid, { title: (sess && sess.title) || capped })
    return sess
  }

  function maybeAutoTitleActiveSession(text) {
    const sid = activeSessionId.value
    if (!sid) return
    const sess = sessions.value.find(x => String(x.id) === String(sid))
    if (!sess || !isPlaceholderTitle(sess.title)) return
    const title = titleFromFirstMessage(text)
    if (!title) return
    patchSession(sid, { title })
    renameSession(sid, title).catch(() => {})
  }

  function selectSession(sid) {
    if (String(activeSessionId.value) === String(sid)) return
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
    const exists = findSession(sid)
    if (exists && String(activeSessionId.value) !== String(sid)) {
      // 用列表里的原始 ID，避免把字符串 ID 混进原本是数字的会话集合
      selectSession(exists.id)
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
  // 无 streaming 标记时的兜底判据：消息创建时间已超过该窗口才认为不可能再有产出。
  const RUNNING_GRACE_MS = 60 * 1000
  function isMessageCold(msg) {
    const ts = Date.parse(msg.createdAt || msg.created_at || '')
    if (!Number.isFinite(ts)) return true // 连时间都没有 → 只能按历史数据处理
    return Date.now() - ts > RUNNING_GRACE_MS
  }

  function buildPartsForMessage(msg) {
    if (msg.role !== 'assistant') return msg
    // 历史消息中的 running 工具需要按 streaming 标记区分：
    //   - streaming=true：该轮仍在产出（用户刷新页面回来），running 是真实状态，保留转圈。
    //   - 否则：该轮已收尾，仍是 running 说明进程中途死了、终态没写回来。
    //     必须降级掉，否则卡片会永久转圈。
    // 用 'killed' 而非自造的 'interrupted'：ToolCallCard/ToolCallGroup 只为
    // success/error/killed/timeout 提供了图标与配色，未知状态会掉到 default 显示异常。
    //
    // 注意 streaming **缺失**（旧数据 / mock / 非分页接口）不等于「本轮已收尾」：
    // 早期实现用 `!msg.streaming` 一刀切，把没带该标记的消息里真正在跑的工具
    // 直接判死。这里区分三种情况：
    //   - streaming === true            → 明确仍在产出，保留 running
    //   - streaming === false           → 后端明确已收尾，running 属残留，降级
    //   - streaming 缺失（undefined/null）→ 无法判断，仅当消息已「冷却」才降级
    // 另外正在重连续流（_resuming）的 trace 一律不降级，它的进度还会推过来。
    const settleRunning = (item) => {
      if (!item || item.status !== 'running') return item
      if (msg.streaming === true) return item
      if (msg.streaming == null && !isMessageCold(msg)) return item
      if (msg.traceId && _resuming.has(msg.traceId)) return item
      return { ...item, status: 'killed', summary: item.summary || '回复已中断' }
    }

    let rawParts = msg.parts
    if (typeof rawParts === 'string' && rawParts.trim()) {
      try { rawParts = JSON.parse(rawParts) } catch { rawParts = null }
    }
    if (Array.isArray(rawParts) && rawParts.length) {
      // 已有有序 parts（来自新格式 API 或流式构建）—— 不要用 content+toolCalls 重排，否则交错顺序丢失。
      const parts = ensureTextPartIds(rawParts.map(p => (p.type === 'tool' ? settleRunning(p) : p)))
      const toolCalls = Array.isArray(msg.toolCalls) ? msg.toolCalls.map(settleRunning) : msg.toolCalls
      const settled = { ...msg, parts, toolCalls }
      // 历史恢复：从落库的工具调用里重建选择卡（含提交/超时终态）。
      // 注意：此刻消息尚未写入 messages.value，tagMessageChoice 在旧数组里找不到
      // 本条消息（必然 no-op），所以这里必须**直接把 qids 写进 settled**，
      // 否则刷新后卡片因缺 questionIds 锚定而不渲染（实测踩过的坑）。
      const qids = restoreChoicesForMessage(settled)
      if (qids.length) {
        settled.questionIds = Array.isArray(settled.questionIds)
          ? Array.from(new Set([...settled.questionIds, ...qids]))
          : qids
      }
      return settled
    }
    // 旧消息无 parts：只能 content 在前、工具在后（交错顺序已不可恢复）。
    const parts = []
    if (msg.content) parts.push({ type: 'text', id: 'text-0', content: msg.content })
    const calls = Array.isArray(msg.toolCalls) ? msg.toolCalls.map(settleRunning) : []
    for (const tc of calls) {
      parts.push({ type: 'tool', ...tc })
    }
    const settled = { ...msg, parts, toolCalls: calls.length ? calls : msg.toolCalls }
    const qids = restoreChoicesForMessage(settled)
    if (qids.length) {
      settled.questionIds = Array.isArray(settled.questionIds)
        ? Array.from(new Set([...settled.questionIds, ...qids]))
        : qids
    }
    return settled
  }

  /**
   * 从落库的 assistant 消息里恢复 user_choice 选择卡（刷新页面后重建卡片状态）。
   * 与 restoreSessionWorkflow 同一动机：choices 是纯内存态，刷新即丢，
   * 但题目与用户已作答的事实必须能从历史里还原。
   * 判定来源：
   *  - payload：tool 调用的 input（n7 工具参数：questionId/question/mode/options/...）
   *  - 终态：该 tool call 的 status（success=已作答、timeout=已超时、killed/error=已取消）
   * @returns {string[]} 本条消息上发现的选择卡 questionId 列表（供调用方锚定）
   */
  function restoreChoicesForMessage(msg) {
    const found = []
    if (!msg || msg.role !== 'assistant') return found
    const byId = new Map()
    const addCall = (tc) => {
      if (!tc) return
      const id = String(tc.id || tc.toolCallId || '')
      if (id) byId.set(id, { ...tc, id })
      else byId.set('anon-' + byId.size, tc)
    }
    for (const tc of (msg.toolCalls || [])) addCall(tc)
    for (const p of (msg.parts || [])) {
      if (p && p.type === 'tool') addCall(p)
    }
    for (const tc of byId.values()) {
      let payload = choicePayloadFromTool(tc)
      const toolCallId = String(tc.id || tc.toolCallId || '')
      if (!payload) {
        const name = String(tc.name || tc.tool || '')
        const isChoice = name === 'user_choice' || name === 'sandbox_user_choice' || name.endsWith(':user_choice')
        if (isChoice) {
          payload = tempChoiceFromInput(tc.input, toolCallId || '_pending_')
          if (payload) {
            const st0 = tc.status
            if (st0 && st0 !== 'running') payload.pending = false
          }
        }
      }
      if (!payload) continue
      registerChoice(msg.id, payload, toolCallId)
      found.push(payload.questionId)
      // 终态回填：落库的 status 就是这道题的最终状态
      const st = tc.status
      if (st === 'success' || st === 'answered' || st === 'resolved') {
        // 落库的 tool_result output 里带有用户实际作答（selectedIds/freeText），
        // 取出来回填 answer，刷新后的终态卡片才能显示"选了什么"
        const out = (tc.output && typeof tc.output === 'object') ? tc.output : null
        markChoiceSubmitted(payload.questionId, out ? {
          selectedIds: Array.isArray(out.selectedIds) ? out.selectedIds.map(String) : [],
          freeText: typeof out.freeText === 'string' ? out.freeText : '',
        } : undefined)
      }
      else if (st === 'timeout') markChoiceTerminal(payload.questionId, 'timeout')
      else if (st === 'killed' || st === 'error' || st === 'cancelled') markChoiceTerminal(payload.questionId, 'cancelled')
    }
    return found
  }

  /**
   * 从工具调用（tool_call 事件或落库的 toolCalls 项）提取 user_choice 负载。
   *
   * 关键点：**questionId 不在 input 里**。input 是 LLM 生成的工具入参
   * （question/options/mode/input_hint/timeout_secs，见 tools/user_choice.go 的
   * userChoiceInput），questionId 是服务端 idgen 生成的，只出现在
   * ① tool_progress 的渲染 payload（走 extractChoicePayload），
   * ② tool_result 的 output（answered/timeout 都带）。
   * 所以刷新后从历史恢复必须读 output.questionId —— 早先版本读 input.questionId，
   * 结果历史里的卡片一张都恢复不出来。
   *
   * 同理 input.options 没有 id（id 是注册时按下标补的），这里交给
   * normalizeChoicePayload 用同一规则 `o{i}` 补齐，才能和 output.selectedIds 对上。
   */
  function choicePayloadFromTool(item) {
    if (!item || typeof item !== 'object') return null
    const name = String(item.name || item.tool || '')
    const input = (item.input && typeof item.input === 'object') ? item.input : {}
    const output = (item.output && typeof item.output === 'object') ? item.output : {}
    const isChoiceTool = name === 'user_choice' || name.endsWith(':user_choice') || name === 'sandbox_user_choice'
    // 工具名认不出时的兜底：output 带 questionId 且 input 像一道选择题
    const looksLikeChoice = output.questionId != null && (Array.isArray(input.options) || input.question)
    if (!isChoiceTool && !looksLikeChoice) return null
    // questionId 依次从 output（落库历史/终态）、input（防御：未来若显式回传）里取
    const qid = output.questionId ?? input.questionId ?? item.questionId
    if (qid == null || qid === '') return null
    return normalizeChoicePayload({
      questionId: qid,
      question: input.question,
      mode: input.mode,
      options: input.options,
      inputHint: input.inputHint ?? input.input_hint,
      timeoutAt: input.timeoutAt ?? input.expiresAt ?? null,
    })
  }

  /**
   * 从 user_choice 工具的原始 input 构造临时选择卡（无 questionId 时用）。
   * questionId 用 tempId 占位（通常是 call.id），等 tool_progress 携带正式
   * questionId 到达后 registerChoice 会覆盖（同一 toolCallId →
   * choiceIdByToolCallId 解析到新 qid，旧条目沦为 orphan）。
   *
   * 这消除了「工具调用已显示、但 ChoiceCard 要等首个 progress 事件才渲染」的空窗期，
   * 解决用户反馈的"实时流式不显示选项、要刷新才看到"问题。
   */
  function tempChoiceFromInput(input, tempId) {
    if (!input || typeof input !== 'object') return null
    const hasOptions = Array.isArray(input.options) && input.options.length > 0
    const hasQuestion = !!input.question
    if (!hasOptions && !hasQuestion) return null
    return normalizeChoicePayload({
      questionId: tempId || '_pending_',
      question: input.question,
      mode: input.mode,
      options: input.options,
      inputHint: input.inputHint ?? input.input_hint,
      timeoutAt: input.timeoutAt ?? null,
      pending: true,
    })
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
    // 选择卡状态同样按会话隔离：换会话/换 bot 后旧题目不得串门渲染
    resetChoices()
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
      if (wid) {
        activeWorkflowId.value = wid
        findAndTagWorkflowMessage(wid)
      }
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
  // 每个续流的 AbortController：切换 bot / 点「停止生成」/ 组件销毁时可中断，
  // 否则这些 fetch 会一直挂在后台（SSE 长连接），既泄漏连接也继续往已切走的
  // 会话里写消息。
  const _resumeControllers = new Map()

  /** 中断全部重连续流（切 bot、停止生成时调用） */
  function _abortResumes() {
    for (const ctrl of _resumeControllers.values()) {
      try { ctrl.abort() } catch { /* 已中断可忽略 */ }
    }
    _resumeControllers.clear()
  }

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

  // 文本/工具写入一律走 store 级 appendTextPart / upsertToolCall /
  // appendToolProgress / finishToolCall，保证 parts 与 toolCalls 同步。
  // （此前这里有一份私有副本，进度和终态只改 toolCalls，ChatWindow 在
  // parts.length>0 时走交错渲染，卡片会一直 running、丢掉 stdout。）

  replying.value = true
  _activeTraceId = traceId
  const ctrl = new AbortController()
  _resumeControllers.set(traceId, ctrl)
  try {
    await chatApi.resume(botId, traceId, {
      signal: ctrl.signal,
      onTextDelta: (delta) => appendTextPart(assistantTmpId, delta),
      onToolCall: (call) => {
        upsertToolCall(assistantTmpId, call)
        // user_choice 工具：重连续流时同样注册选择卡（断连期间下发的题目不能丢）
        const cp = choicePayloadFromTool(call)
        if (cp) {
          registerChoice(assistantTmpId, cp, call.id)
        } else if (call.name === 'user_choice' || call.name === 'sandbox_user_choice') {
          const tmp = tempChoiceFromInput(call.input, call.id)
          if (tmp) registerChoice(assistantTmpId, tmp, call.id)
        }
      },
      onToolProgress: (toolCallId, payload) => {
        appendToolProgress(assistantTmpId, toolCallId, payload)
        // 同上：阻塞式 task 的 workflowId 只能从进度事件拿到（result 要等到终态）
        const pid = extractWorkflowId(payload)
        if (pid) {
          activeWorkflowId.value = pid
          tagMessageWorkflow(assistantTmpId, pid)
        }
        // user_choice：重连续流中的进度事件同样可能刷新卡片（如剩余超时时间）
        const cp = extractChoicePayload(payload)
        if (cp) {
          cp.pending = false
          registerChoice(assistantTmpId, cp, toolCallId)
          stampChoiceQuestionId(assistantTmpId, toolCallId, cp.questionId)
        }
      },
      onToolResult: (toolCallId, payload) => {
        finishToolCall(assistantTmpId, toolCallId, payload)
        const wid = extractWorkflowId(payload)
        if (wid) {
          activeWorkflowId.value = wid
          tagMessageWorkflow(assistantTmpId, wid)
        }
        // 重连续流同样要合并状态快照，否则刷新后面板拿不到工作流实时状态
        mergeWorkflowSnapshot(wid, payload)
        // user_choice 终态（超时/取消/完成）：重连后落定的终态必须同步进卡片
        const cst = extractChoiceTerminal(payload)
        if (cst) {
          registerChoice(assistantTmpId, cst.payload, toolCallId)
          if (cst.status === 'answered') markChoiceSubmitted(cst.payload.questionId, cst.answer)
          else markChoiceTerminal(cst.payload.questionId, cst.status)
        }
      },
    })
    // 重连续流正常结束：把占位消息转正
    const updated = [...messages.value]
    const idx = updated.findIndex(m => m.id === assistantTmpId)
    if (idx >= 0) updated[idx] = { ...updated[idx], _temp: false }
    messages.value = updated
  } catch (e) {
    // 主动中断（切 bot / 停止生成 / 卸载）不是错误，不必刷警告
    if (e?.name !== 'AbortError') console.warn('resume trace failed', traceId, e)
  } finally {
    _resumeControllers.delete(traceId)
    _resuming.delete(traceId)
    // 可能有多个续流并发，只有全部结束（且发送主路径也不在跑）才复位 replying，
    // 否则先结束的那条会把仍在流式的 UI 提前切回「可发送」态。
    if (!_resuming.size && !_abortController) replying.value = false
    if (_activeTraceId === traceId) _activeTraceId = ''
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

  // 不能串行 await：每个续流是 SSE 长连接，会一直挂到该任务结束（可能几十分钟），
  // 串行等待会把 loadMessages 后面的工作流恢复与首屏滚底整个卡死。
  // 这里只负责把续流拉起来，各自独立推进；错误已在 _resumeTrace 内部处理。
  for (const traceId of traceIds) {
    _resumeTrace(traceId).catch(() => {})
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
    // 重连续流也要一起断，否则「停止生成」后后台 SSE 仍在往列表里写内容
    _abortResumes()
    const updated = [...messages.value]
    for (let i = updated.length - 1; i >= 0; i--) {
      const m = updated[i]
      if (m.role !== 'assistant') continue
      if (!Array.isArray(m.toolCalls) || !m.toolCalls.length) break
      const runningIds = new Set(m.toolCalls.filter(tc => tc.status === 'running').map(tc => tc.id))
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
      if (runningIds.size) {
        for (const [qid, c] of choices.value) {
          if (c && runningIds.has(c.toolCallId)) markChoiceTerminal(qid, 'cancelled')
        }
      }
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

    const dropLocalBubble = () => {
      messages.value = messages.value.filter(m => m.id !== userMsg.id)
    }

    /**
     * 降级为普通发送。必须等本轮真正结束：sendMessage 开头有
     * `if (replying.value) return`，在流式未结束时直接调用会被静默丢弃，
     * 用户的这句补充就凭空消失了。这里挂一次性 watch 等 replying 落下再发。
     */
    let fallbackDone = false
    const fallbackSend = () => {
      if (fallbackDone) return
      fallbackDone = true
      if (!replying.value) { sendMessage(text); return }
      const stop = watch(replying, (v) => {
        if (v) return
        stop()
        sendMessage(text)
      })
    }

    chatApi.append(botId, traceId, text, activeSessionId.value)
      .then((resp) => {
        // 只有后端**明确回绝**（accepted === false）才降级重发：此时可以确定
        // 这条补充没有进入本轮，不会重复。
        if (resp && resp.accepted === false) {
          dropLocalBubble()
          fallbackSend()
          return
        }
        // resp 为空/字段缺失属于「结果未知」，与网络错误同等处理（见下）
        if (!resp) markAppendUnconfirmed(userMsg.id)
      })
      .catch((e) => {
        // 网络/超时错误无法区分「请求没发出去」和「后端已收下但响应丢了」，
        // 盲目重发会把同一句补充发两遍（原实现的重复消息来源）。
        // 这里保留本地气泡并打上未确认标记，交由用户决定是否重发。
        console.warn('append to current reply failed', e)
        markAppendUnconfirmed(userMsg.id)
      })
  }

  /** 标记某条补充消息「投递结果未知」，供 UI 提示用户自行确认/重发 */
  function markAppendUnconfirmed(msgId) {
    const idx = messages.value.findIndex(m => m.id === msgId)
    if (idx < 0) return
    const updated = [...messages.value]
    updated[idx] = { ...updated[idx], _appendUnconfirmed: true }
    messages.value = updated
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

  function nextTextPartId(parts) {
    const used = new Set()
    for (const p of (parts || [])) {
      if (p && p.type === 'text' && p.id != null && p.id !== '') used.add(String(p.id))
    }
    let n = 0
    while (used.has('text-' + n)) n++
    return 'text-' + n
  }

  function ensureTextPartIds(parts) {
    if (!Array.isArray(parts)) return parts
    const out = []
    for (const p of parts) {
      if (p && p.type === 'text' && (p.id == null || p.id === '')) {
        out.push({ ...p, id: nextTextPartId(out) })
      } else {
        out.push(p)
      }
    }
    return out
  }

  function appendTextPart(messageId, delta) {
    if (delta == null || delta === '') return
    const idx = messages.value.findIndex(m => m.id === messageId)
    if (idx < 0) return
    const updated = [...messages.value]
    const msg = { ...updated[idx] }
    msg.content = (msg.content || '') + delta
    const parts = Array.isArray(msg.parts) ? [...msg.parts] : []
    const last = parts[parts.length - 1]
    if (last && last.type === 'text') {
      const id = last.id || nextTextPartId(parts.slice(0, -1))
      parts[parts.length - 1] = { ...last, id, content: (last.content || '') + delta }
    } else {
      parts.push({ type: 'text', id: nextTextPartId(parts), content: delta })
    }
    msg.parts = parts
    updated[idx] = msg
    messages.value = updated
  }

  function upsertToolCall(messageId, call) {
    const mIdx = messages.value.findIndex(m => m.id === messageId)
    if (mIdx < 0 || !call?.id) return
    const updated = [...messages.value]
    const msg = { ...updated[mIdx] }
    const list = Array.isArray(msg.toolCalls) ? [...msg.toolCalls] : []
    const idx = list.findIndex(x => x.id === call.id)
    const prev = idx >= 0 ? list[idx] : null
    const incoming = {
      ...prev,
      ...call,
      id: call.id,
      name: call.name || (prev && prev.name) || call.tool,
      title: call.title || (prev && prev.title) || call.name || (prev && prev.name) || call.tool,
      status: call.status || (prev && prev.status) || 'running',
      input: call.input !== undefined ? call.input : (prev && prev.input),
      output: normalizeToolOutput(call.output, prev ? normalizeToolOutput(prev.output) : null),
    }
    if (idx < 0) list.push(incoming)
    else list[idx] = incoming
    msg.toolCalls = list
    const merged = idx < 0 ? list[list.length - 1] : list[idx]
    const parts = Array.isArray(msg.parts) ? [...msg.parts] : []
    const pIdx = parts.findIndex(p => p.type === 'tool' && p.id === call.id)
    if (pIdx >= 0) {
      parts[pIdx] = { ...parts[pIdx], ...merged, type: 'tool', id: call.id }
    } else {
      parts.push({ type: 'tool', ...merged, type: 'tool', id: call.id })
    }
    msg.parts = parts
    updated[mIdx] = msg
    messages.value = updated
  }

  function appendToolProgress(messageId, toolCallId, payload) {
    const mIdx = messages.value.findIndex(m => m.id === messageId)
    if (mIdx < 0 || !toolCallId) return
    const updated = [...messages.value]
    const msg = { ...updated[mIdx] }
    const list = Array.isArray(msg.toolCalls) ? [...msg.toolCalls] : []
    let idx = list.findIndex(x => x.id === toolCallId)
    if (idx < 0) {
      list.push({
        id: toolCallId,
        name: payload?.tool,
        title: toolLabels[payload?.tool] || payload?.tool,
        status: 'running',
        output: normalizeToolOutput(null),
      })
      idx = list.length - 1
    }

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

  // 把 workflowId 锚定到创建它的那条 assistant 消息，使工作流卡片内联渲染在该轮之后，
  // 而不是永远钉在整段对话最底部（先前的 hover 行为）。
  function tagMessageWorkflow(messageId, wid) {
    if (!wid) return
    const mIdx = messages.value.findIndex(m => m.id === messageId)
    if (mIdx < 0) return
    const updated = [...messages.value]
    const msg = { ...updated[mIdx] }
    if (msg.workflowId === wid) return
    msg.workflowId = wid
    updated[mIdx] = msg
    messages.value = updated
  }

  // 刷新后消息从 DB 载入、activeWorkflowId 已恢复时，反向扫描消息找到承载该工作流的
  // 助手消息并锚定，否则内联卡片因无 message.workflowId 匹配而不显示。
  function findAndTagWorkflowMessage(wid) {
    if (!wid) return
    for (let i = messages.value.length - 1; i >= 0; i--) {
      const m = messages.value[i]
      if (m.role !== 'assistant') continue
      const tcs = m.toolCalls || (m.parts || []).filter(p => p.type === 'tool')
      for (const tc of tcs) {
        if (extractWorkflowId(tc) === wid) {
          tagMessageWorkflow(m.id, wid)
          return
        }
      }
    }
  }

  /** 同步 parts 数组中指定工具 part 的状态（供 appendToolProgress / finishToolCall 复用）。
   *  若 parts 里还没有这张卡（finish 先于 upsert、或只写了 toolCalls），补一条，避免交错视图丢卡。
   */
  function syncPartFromToolCall(msg, toolCallId, call) {
    const parts = Array.isArray(msg.parts) ? [...msg.parts] : []
    const pIdx = parts.findIndex(p => p.type === 'tool' && p.id === toolCallId)
    if (pIdx >= 0) {
      parts[pIdx] = { ...parts[pIdx], ...call, type: 'tool', id: toolCallId }
    } else {
      parts.push({ type: 'tool', id: toolCallId, ...call, type: 'tool', id: toolCallId })
    }
    msg.parts = parts
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
    maybeAutoTitleActiveSession(content)

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
        // user_choice 工具：调用即下发选择卡（进度/结果要等用户作答，可能很久）
        const cp = choicePayloadFromTool(call)
        if (cp) {
          registerChoice(assistantTmpId, cp, call.id)
        } else if (call.name === 'user_choice' || call.name === 'sandbox_user_choice') {
          // questionId 由服务端生成、仅在 tool_progress 中下发。
          // 为避免「工具卡片已显示但 ChoiceCard 要等 progress 才出现」的空窗期，
          // 从 input 构造临时选择卡（用 call.id 作占位 questionId），
          // 等 progress 事件到达后会用正式 questionId 覆盖（同一 toolCallId）。
          const tmp = tempChoiceFromInput(call.input, call.id)
          if (tmp) registerChoice(assistantTmpId, tmp, call.id)
        }
      },
      onToolProgress: (toolCallId, payload) => {
        appendToolProgress(assistantTmpId, toolCallId, payload)
        // task 工具「提交即阻塞」：tool_result 要等工作流跑完（可能数十分钟）才到达，
        // 因此必须从进度事件里就取到 workflowId，否则整个执行期间面板都不会出现。
        const pid = extractWorkflowId(payload)
        if (pid) {
          activeWorkflowId.value = pid
          tagMessageWorkflow(assistantTmpId, pid)
        }
        // user_choice：进度事件也可能携带卡片负载（超时刷新等），同样注册
        const cp = extractChoicePayload(payload)
        if (cp) {
          // 防御性清除 pending：tempChoiceFromInput 设了 pending:true，
          // 进度事件的 normalizeChoicePayload 虽然会产出 pending:false，
          // 但若 payload 经 SSE 透传后字段丢失/格式微差，合并可能保留旧值。
          // 此处显式 false 确保进度到达后卡片一定解锁（不再显示"题目准备中…"）。
          cp.pending = false
          registerChoice(assistantTmpId, cp, toolCallId)
          stampChoiceQuestionId(assistantTmpId, toolCallId, cp.questionId)
        }
      },
      onToolResult: (toolCallId, payload) => {
        finishToolCall(assistantTmpId, toolCallId, payload)
        // task 工具返回里携带 workflowId（如 "wf-xxxx"），提取后驱动工作流面板展示
        const wid = extractWorkflowId(payload)
        if (wid) {
          activeWorkflowId.value = wid
          tagMessageWorkflow(assistantTmpId, wid)
        }
        mergeWorkflowSnapshot(wid, payload)
        // user_choice 终态：超时/取消/已完成（用户可能已在别处作答）
        const cst = extractChoiceTerminal(payload)
        if (cst) {
          registerChoice(assistantTmpId, cst.payload, toolCallId)
          if (cst.status === 'answered') markChoiceSubmitted(cst.payload.questionId, cst.answer)
          else markChoiceTerminal(cst.payload.questionId, cst.status)
        }
      },
      signal: _abortController.signal,
      attachments: attachments || [],
    })
      .then((resp) => {
        if (resp?.traceId) _activeTraceId = resp.traceId

        // 斜杠命令响应：/clear 等命令在 done 事件中携带 command:true
        if (resp?.command === 'clear') {
          // 清空本地消息列表（DB 已在后端清完）
          messages.value = []
          // 同时重置工作流面板状态，避免旧工作流卡片残留
          activeWorkflowId.value = ''
          activeWorkflowStatus.value = null
          return
        }
        // 其他命令（help 等）正常显示回复文本，走下方常规流程

        const updated = [...messages.value]
        const aIdx = updated.findIndex(m => m.id === assistantTmpId)
        if (aIdx >= 0) {
          // done.text 是整段拼接后的 assistant 文本（services.js 用 SSE done 覆盖
          // 本地累积的 fullText）。不得写进最后一个 text part，否则「文本→工具→文本」
          // 会变成「文本→工具→全文再倒一次」。
          const prev = updated[aIdx]
          const parts = Array.isArray(prev.parts) ? [...prev.parts] : []
          const textIdxs = []
          let hasTool = false
          for (let i = 0; i < parts.length; i++) {
            if (parts[i] && parts[i].type === 'text') textIdxs.push(i)
            if (parts[i] && parts[i].type === 'tool') hasTool = true
          }
          let content = prev.content || ''
          if (textIdxs.length === 0 && resp.text) {
            parts.push({ type: 'text', id: nextTextPartId(parts), content: resp.text })
            content = resp.text
          } else if (textIdxs.length === 1 && !hasTool && resp.text) {
            const i = textIdxs[0]
            parts[i] = { ...parts[i], id: parts[i].id || nextTextPartId(parts.filter((_, j) => j !== i)), content: resp.text }
            content = resp.text
          }
          updated[aIdx] = { ...prev, content, parts, _temp: false }
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
            parts: [{ type: 'text', id: 'text-0', content: failText }]
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
    // user_choice 选择卡（n7 契约）：渲染层用 choiceState(qid) 取每题状态
    choices, submittedChoiceIds, choiceState, choiceIdByToolCallId,
    registerChoice, markChoiceSubmitted, markChoiceTerminal, resetChoices,
    scrollToBottomOnLoad,
    fetchBots, selectBot,
    createBot, updateBot, deleteBot,
    loadMessages, loadMoreMessages, sendMessage, stopReply, appendToCurrentReply, resumeInFlightTasks, resumeContinuation,
    // 会话管理
    sessions, sessionsLoading, activeSessionId,
    loadSessions, createSession, deleteSession, renameSession, selectSession,
    setPendingSession, openSessionById
  }
})
