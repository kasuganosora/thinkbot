import type { SSEEvent } from '@/types/api'

/**
 * 发送 SSE 流式聊天消息。
 *
 * 通过 fetch POST 获取 ReadableStream，手动解析 SSE 格式。
 * 不使用 EventSource 是因为 EventSource 只支持 GET。
 */
export async function streamChat(
  botId: string,
  text: string,
  onEvent: (event: SSEEvent) => void,
  signal?: AbortSignal,
): Promise<void> {
  const res = await fetch('/api/chat/send', {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ botId, text }),
    signal,
  })

  if (!res.ok || !res.body) {
    const msg = `HTTP ${res.status}`
    onEvent({ type: 'error', data: { message: msg } })
    return
  }

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  while (true) {
    const { done, value } = await reader.read()
    if (done) break

    buffer += decoder.decode(value, { stream: true })

    // SSE 以双换行分隔事件
    const parts = buffer.split('\n\n')
    buffer = parts.pop() ?? ''

    for (const part of parts) {
      const event = parseSSEBlock(part)
      if (event) onEvent(event)
    }
  }

  // 处理缓冲区中剩余的数据
  if (buffer.trim()) {
    const event = parseSSEBlock(buffer)
    if (event) onEvent(event)
  }
}

function parseSSEBlock(block: string): SSEEvent | null {
  let type = ''
  let dataStr = ''

  for (const line of block.split('\n')) {
    if (line.startsWith('event: ')) {
      type = line.slice(7).trim()
    } else if (line.startsWith('data: ')) {
      dataStr += line.slice(6)
    }
  }

  if (!type) return null

  let data: Record<string, unknown> = {}
  if (dataStr) {
    try {
      data = JSON.parse(dataStr)
    } catch {
      data = { raw: dataStr }
    }
  }

  return { type: type as SSEEvent['type'], data }
}
