import test from 'node:test'
import assert from 'node:assert/strict'
import { readChatStream } from '../src/api/chatStream.js'

const frame = (type, data, id) => `${id ? `id: ${id}\n` : ''}event: ${type}\ndata: ${JSON.stringify(data)}\n\n`
function response(text, chunkSize = 7) {
  const bytes = new TextEncoder().encode(text)
  return new Response(new ReadableStream({ start(controller) { for (let i = 0; i < bytes.length; i += chunkSize) controller.enqueue(bytes.slice(i, i + chunkSize)); controller.close() } }), { headers: { 'Content-Type': 'text/event-stream' } })
}

test('SSE decodes split UTF-8 and CRLF frames and requires explicit done', async () => {
  const chunks = []
  const result = await readChatStream(response((frame('start', { traceId: 't', sessionId: 's' }) + frame('text_delta', { text: '中文回复' }, 1) + frame('done', { text: '中文回复', traceId: 't', sessionId: 's' }, 2)).replaceAll('\n', '\r\n'), 1), { onTextDelta: text => chunks.push(text) }, { sessionId: 's' })
  assert.equal(result.text, '中文回复')
  assert.equal(result.traceId, 't')
  assert.deepEqual(chunks, ['中文回复'])
  assert.equal(result.streamSeq, 2)
})

test('snapshot replaces previous output and replayed sequence IDs are ignored', async () => {
  let rendered = 'stale duplicate'
  const snapshots = []
  const result = await readChatStream(response(
    frame('snapshot', { message: { id: 'm', role: 'assistant', traceId: 't', sessionId: 's', content: 'prefix', parts: [{ type: 'text', content: 'prefix' }], toolCalls: [], streamSeq: 5, streaming: true } })
    + frame('text_delta', { text: 'ignored' }, 5)
    + frame('text_delta', { text: ' suffix' }, 6)
    + frame('text_delta', { text: ' duplicate' }, 6)
    + frame('done', { text: 'prefix suffix', parts: [{ type: 'text', content: 'prefix suffix' }] }, 7)
  ), { onSnapshot: snapshot => { snapshots.push(snapshot); rendered = snapshot.content }, onTextDelta: text => { rendered += text } }, { traceId: 't', sessionId: 's', sinceSeq: 3 })
  assert.equal(rendered, 'prefix suffix')
  assert.equal(snapshots.length, 1)
  assert.equal(result.parts[0].content, 'prefix suffix')
})

test('tool callback mutation does not double-append internal stdout', async () => {
  let live
  const result = await readChatStream(response(frame('tool_call', { toolCallId: 'call', tool: 'exec', input: {} }, 1) + frame('tool_progress', { toolCallId: 'call', chunk: 'one', stream: 'stdout' }, 2) + frame('done', {})), {
    onToolCall: call => { live = call },
    onToolProgress: (_id, payload) => { live.output.stdout += payload.chunk },
  })
  assert.equal(live.output.stdout, 'one')
  assert.equal(result.toolCalls[0].output.stdout, 'one')
})

test('EOF during a task reports a recoverable disconnect instead of completion', async () => {
  await assert.rejects(readChatStream(response(frame('start', { traceId: 'unfinished' }) + frame('text_delta', { text: 'partial' }, 10))), error => error.recoverable === true && error.traceId === 'unfinished' && error.streamSeq === 10)
})

test('terminal errors preserve recoverable semantics', async () => {
  for (const recoverable of [false, true]) {
    await assert.rejects(readChatStream(response(frame('error', { message: 'task failed', recoverable, traceId: 't' }))), error => error.message === 'task failed' && error.recoverable === recoverable)
  }
})

test('a snapshot from another session is never delivered to the renderer', async () => {
  let delivered = false
  await assert.rejects(readChatStream(response(frame('snapshot', { message: { role: 'assistant', sessionId: 'wrong', traceId: 't', content: 'private' } })), { onSnapshot: () => { delivered = true } }, { traceId: 't', sessionId: 'right' }), error => error.recoverable === false)
  assert.equal(delivered, false)
})

test('done preserves an explicit empty final answer and command metadata', async () => {
  const result = await readChatStream(response(frame('text_delta', { text: 'previous text' }, 1) + frame('done', { text: '', command: 'clear', cleared: true, status: 'cancelled', toolCalls: [], parts: [] }, 2)))
  assert.equal(result.text, '')
  assert.equal(result.command, 'clear')
  assert.equal(result.cleared, true)
  assert.equal(result.status, 'cancelled')
  assert.deepEqual(result.parts, [])
})

test('malformed payload and unterminated final frame remain recoverable failures', async () => {
  await assert.rejects(readChatStream(response('event: text_delta\ndata: invalid\n\n')), error => error.recoverable === true)
  await assert.rejects(readChatStream(response('event: done\ndata: {"text":"incomplete"}')), error => error.recoverable === true)
})


test('concurrent history cursor prevents replaying buffered text and tool events', async () => {
  let current = { role: 'assistant', traceId: 't', sessionId: 's', content: '', streamSeq: 0, toolCalls: [] }
  const deltas = []
  const result = await readChatStream(response(frame('text_delta', { text: 'A' }, 1) + frame('text_delta', { text: 'B' }, 2) + frame('text_delta', { text: 'C' }, 3) + frame('done', { text: 'ABC', streamSeq: 3 })), {
    currentSnapshot: () => current,
    onTextDelta: text => {
      deltas.push(text)
      // A history response arrives after A, already containing event B.
      current = text === 'A' ? { ...current, content: 'AB', streamSeq: 2 } : { ...current, content: current.content + text, streamSeq: 3 }
    },
  }, { traceId: 't', sessionId: 's' })
  assert.deepEqual(deltas, ['A', 'C'])
  assert.equal(current.content, 'ABC')
  assert.equal(result.text, 'ABC')
})
