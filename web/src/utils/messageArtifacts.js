const WRITE_TOOLS = new Set(['write_file', 'replace_in_file'])
const CONTROL_CHARACTERS = /[\u0000-\u001f\u007f-\u009f]/
const URI_SCHEME = /^[a-z][a-z\d+.-]*:/i

function unsafePath(path) {
  return CONTROL_CHARACTERS.test(path)
    || path.includes('\\')
    || URI_SCHEME.test(path)
    || (path.startsWith('/') && !path.startsWith('/data/'))
    || path.split('/').includes('..')
}

/** Map explicit workspace paths to /data without accepting another filesystem root. */
export function normalizeArtifactPath(value) {
  if (typeof value !== 'string' || !value || value.length > 4096 || value !== value.trim()) return ''
  // Also reject encoded traversal/URLs. Keep the original spelling: filenames may
  // contain percent signs, and the download URL will encode them exactly once.
  let inspected = value
  for (let i = 0; i < 4; i++) {
    if (unsafePath(inspected)) return ''
    try {
      const decoded = decodeURIComponent(inspected)
      if (decoded === inspected) break
      inspected = decoded
    } catch { break }
  }
  if (unsafePath(inspected)) return ''
  const relative = value.startsWith('/data/') ? value.slice(6) : value
  const segments = relative.split('/').filter(segment => segment && segment !== '.')
  if (!segments.length || value.endsWith('/')) return ''
  return `/data/${segments.join('/')}`
}

function asObject(value) {
  if (value && typeof value === 'object' && !Array.isArray(value)) return value
  if (typeof value !== 'string') return null
  try {
    const parsed = JSON.parse(value)
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed : null
  } catch { return null }
}

function asArray(value) {
  if (Array.isArray(value)) return value
  if (typeof value !== 'string') return []
  try { const parsed = JSON.parse(value); return Array.isArray(parsed) ? parsed : [] } catch { return [] }
}

/** Extract only confirmed writes from one assistant message, never from prose or a Bot listing. */
export function extractMessageArtifacts(message, { botId, sessionId } = {}) {
  if (!message || message.role !== 'assistant') return []
  const sourceSession = message.sessionId ?? message.session_id
  const sourceBot = message.botId ?? message.bot_id
  if (sourceSession != null && sourceSession !== '' && String(sourceSession) !== String(sessionId)) return []
  if (sourceBot != null && sourceBot !== '' && String(sourceBot) !== String(botId)) return []

  const calls = [
    ...asArray(message.parts).filter(part => part?.type === 'tool'),
    ...asArray(message.toolCalls),
  ]
  const files = new Map()
  for (const call of calls) {
    if (!call || call.status !== 'success') continue
    const name = String(call.name || '').replace(/^sandbox_/, '')
    if (!WRITE_TOOLS.has(name)) continue
    const output = asObject(call.output)
    if (output?.success !== true) continue
    const path = normalizeArtifactPath(output.path)
    if (!path || files.has(path)) continue
    files.set(path, {
      path,
      name: path.slice(path.lastIndexOf('/') + 1),
      toolCallId: String(call.id ?? ''),
      toolName: name,
    })
  }
  return [...files.values()]
}
