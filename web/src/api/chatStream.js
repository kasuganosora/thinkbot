import toolLabels from '../i18n/toolLabels.js'

export class ChatStreamError extends Error {
  constructor(message, details = {}) {
    super(message)
    this.name = 'ChatStreamError'
    Object.assign(this, details)
  }
}

const clone = value => value == null ? value : JSON.parse(JSON.stringify(value))

/** Shared fetch-SSE reader. EOF without a terminal event is a disconnection, never success. */
export async function readChatStream(response, handlers = {}, initial = {}) {
  let traceId = initial.traceId || ''
  let sessionId = initial.sessionId || ''
  let sequence = Number(initial.sinceSeq) || 0
  let fullText = initial.message?.content || ''
  let toolCalls = clone(initial.message?.toolCalls || [])
  const toolMap = new Map(toolCalls.map(call => [call.id, call]))
  const failure = (message, details = {}) => new ChatStreamError(message, { traceId, sessionId, streamSeq: sequence, recoverable: true, ...details })
  if (!response.ok) {
    const body = await response.json().catch(() => ({}))
    throw failure(body.message || `HTTP ${response.status}`, { httpStatus: response.status, recoverable: response.status >= 500, userText: body.data?.userText })
  }
  if (!response.body) throw failure('回复连接未返回数据')
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let event = { type: '', data: [], id: '' }
  let terminal = null

  const updateSequence = value => {
    const next = Number(value)
    if (Number.isFinite(next) && next > sequence) {
      sequence = next
      handlers.onSequence?.(sequence)
    }
  }
  const checkScope = data => {
    if (sessionId && data.sessionId && String(sessionId) !== String(data.sessionId)) throw failure('回复会话不匹配', { recoverable: false })
    if (traceId && data.traceId && String(traceId) !== String(data.traceId)) throw failure('回复任务不匹配', { recoverable: false })
    traceId = data.traceId || traceId
    sessionId = data.sessionId || sessionId
  }
  function dispatch() {
    const frame = event
    event = { type: '', data: [], id: '' }
    if (!frame.data.length) return
    let data
    try { data = JSON.parse(frame.data.join('\n')) } catch { throw failure('回复数据不完整，请重新连接') }
    checkScope(data)
    // A concurrent history refresh may already contain events buffered on this
    // connection. Advance the parser to that authoritative cursor before replay.
    const current = handlers.currentSnapshot?.()
    if (current && Number(current.streamSeq) > sequence) {
      checkScope(current)
      sequence = Number(current.streamSeq)
      fullText = current.content || ''
      toolCalls = clone(current.toolCalls || [])
      toolMap.clear()
      for (const call of toolCalls) toolMap.set(call.id, call)
    }
    const id = Number(frame.id)
    const sequenced = Number.isFinite(id) && id > 0
    if (sequenced && id <= sequence && !['start', 'snapshot', 'done', 'error'].includes(frame.type)) return
    switch (frame.type) {
      case 'start':
        handlers.onStart?.(traceId, data)
        break
      case 'snapshot': {
        const message = data.message
        if (!message || message.role !== 'assistant') throw failure('回复快照格式无效')
        checkScope(message)
        if (Number(message.streamSeq) < sequence) break
        fullText = message.content || ''
        toolCalls = clone(message.toolCalls || [])
        toolMap.clear()
        for (const call of toolCalls) toolMap.set(call.id, call)
        handlers.onSnapshot?.(clone(message))
        updateSequence(message.streamSeq)
        break
      }
      case 'text_delta':
        fullText += data.text || ''
        handlers.onTextDelta?.(data.text || '')
        break
      case 'tool_call': {
        const call = { id: data.toolCallId || `tool-${frame.id}`, name: data.tool, title: toolLabels[data.tool] || data.tool, input: data.input, status: 'running', output: { stdout: '', stderr: '', exitCode: null, truncated: false } }
        const previous = toolMap.get(call.id)
        if (previous) Object.assign(previous, clone(call))
        else { const saved = clone(call); toolCalls.push(saved); toolMap.set(saved.id, saved) }
        handlers.onToolCall?.(call)
        break
      }
      case 'tool_progress': {
        const payload = { ...(typeof data.payload === 'object' && data.payload ? data.payload : {}), stream: data.payload?.stream || data.stream || 'stdout', chunk: data.payload?.chunk || data.chunk || '' }
        const call = toolMap.get(data.toolCallId)
        if (call) {
          const stream = payload.stream === 'stderr' ? 'stderr' : 'stdout'
          const output = call.output && typeof call.output === 'object' ? call.output : { stdout: '', stderr: '', exitCode: null, truncated: false }
          call.output = { ...output, [stream]: (output[stream] || '') + payload.chunk }
        }
        handlers.onToolProgress?.(data.toolCallId, payload)
        break
      }
      case 'tool_result': {
        const payload = { ...data, status: data.status || (data.error != null ? 'error' : 'success') }
        const call = toolMap.get(data.toolCallId)
        if (call) {
          Object.assign(call, { status: payload.status, summary: data.summary })
          if (data.output !== undefined) call.output = clone(data.output)
          if (data.error != null && call.output && typeof call.output === 'object') call.output = { ...call.output, stderr: (call.output.stderr || '') + String(data.error) }
        }
        handlers.onToolResult?.(data.toolCallId, payload)
        break
      }
      case 'done': {
        if (typeof data.text === 'string') fullText = data.text
        if (Array.isArray(data.toolCalls)) toolCalls = clone(data.toolCalls)
        updateSequence(frame.id)
        updateSequence(data.streamSeq)
        terminal = { ...data, traceId, sessionId, text: fullText, toolCalls, streamSeq: sequence }
        handlers.onDone?.(terminal)
        break
      }
      case 'error':
        throw failure(data.message || '任务执行失败', { ...data, recoverable: data.recoverable === true })
      default:
        break
    }
    updateSequence(frame.id)
  }
  try {
    while (!terminal) {
      const { done, value } = await reader.read()
      if (done) throw failure('回复连接已断开，请重新连接以获取最新进度')
      buffer += decoder.decode(value, { stream: true })
      let newline
      while (!terminal && (newline = buffer.indexOf('\n')) >= 0) {
        const line = buffer.slice(0, newline).replace(/\r$/, '')
        buffer = buffer.slice(newline + 1)
        if (!line) { dispatch(); continue }
        if (line.startsWith(':')) continue
        const separator = line.indexOf(':')
        const field = separator < 0 ? line : line.slice(0, separator)
        const raw = separator < 0 ? '' : line.slice(separator + 1)
        const value = raw.startsWith(' ') ? raw.slice(1) : raw
        if (field === 'event') event.type = value
        if (field === 'data') event.data.push(value)
        if (field === 'id') event.id = value
      }
    }
    return terminal
  } finally {
    await reader.cancel().catch(() => {})
    reader.releaseLock()
  }
}
