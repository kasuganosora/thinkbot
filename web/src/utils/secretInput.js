// Only this allowlist may enter message/store state. The submitted value never does.
export function secretRequestFromOutput(output) {
  if (!output || typeof output !== 'object') return null
  const data = output.secretRequest || output
  if (data.status !== 'requested' || data.input_mode !== 'web_form'
    || typeof data.question_id !== 'string' || !data.question_id
    || !Number.isFinite(data.expires_at) || data.expires_at <= 0) return null
  return {
    status: 'requested', input_mode: 'web_form', question_id: data.question_id,
    name: typeof data.name === 'string' ? data.name : '',
    purpose: typeof data.purpose === 'string' ? data.purpose : '',
    placeholder: typeof data.placeholder === 'string' ? data.placeholder : '',
    expires_at: data.expires_at,
  }
}

export function secretRequestsForMessage(message) {
  if (message?.role !== 'assistant') return []
  const requests = new Map()
  function visit(call) {
    if (!call || typeof call !== 'object') return
    for (const grouped of (call._group || [])) visit(grouped)
    if (call.name !== 'secret_input' && call.tool !== 'secret_input') return
    const request = secretRequestFromOutput(call.output)
    if (request) requests.set(request.question_id, request)
  }
  for (const call of [...(message.toolCalls || []), ...(message.parts || [])]) visit(call)
  return [...requests.values()]
}

// The API request and the safe chat continuation are deliberately separate.
// Any error, including a missing request after restart, ends this attempt without
// putting the value into ordinary chat, a retry queue, or the composer draft.
export async function submitSecretForm({ questionId, botId, sessionId, value, cancel = false }, { post, isCurrent, continueChat }) {
  let result
  try {
    result = await post('POST', `/api/chat/secrets/${encodeURIComponent(questionId)}`,
      cancel ? { botId, sessionId, cancel: true } : { botId, sessionId, value })
  } catch {
    return { status: 'failed' }
  }
  if (cancel) return { status: result?.status === 'cancelled' ? 'cancelled' : 'failed' }
  if (result?.status !== 'stored') return { status: 'failed' }
  if (!isCurrent() || typeof result.userText !== 'string' || !result.userText.trim()) {
    return { status: 'stored', continued: false }
  }
  try {
    const delivery = continueChat(result.userText)
    return { status: 'stored', continued: delivery?.accepted === true, completion: delivery?.completion }
  } catch {
    return { status: 'stored', continued: false }
  }
}
