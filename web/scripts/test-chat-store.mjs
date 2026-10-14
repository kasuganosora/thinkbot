import assert from 'node:assert/strict'
import test from 'node:test'
import { createServer } from 'vite'
import { createPinia } from 'pinia'
import { nextTick } from 'vue'

// Load the real store through the app's Vite aliases; mock only HTTP boundaries.
await test('chat conversation and delivery regressions', async (t) => {
  const server = await createServer({ server: { middlewareMode: true }, appType: 'custom' })
  const intervals = new Set()
  const originalInterval = globalThis.setInterval
  globalThis.setInterval = (...args) => { const timer = originalInterval(...args); timer.unref(); intervals.add(timer); return timer }
  const { useBotStore } = await server.ssrLoadModule('/src/stores/bot.js')
  const { chatApi, sessionApi, workflowApi } = await server.ssrLoadModule('/src/api/services.js')
  const originals = new Map([chatApi, sessionApi, workflowApi].map(api => [api, { ...api }]))
  const stores = []
  const settle = async () => { for (let i = 0; i < 20; i++) await nextTick() }
  const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no }); return { promise, resolve, reject } }
  async function makeStore() {
    chatApi.abort = async () => ({ aborted: true })
    chatApi.abortSession = async () => ({ aborted: true })
    chatApi.resume = async () => ({ text: '' })
    chatApi.activeTasks = async () => []
    chatApi.activeTasksWithMeta = async () => ({ tasks: [], unattributed: 0 })
    chatApi.history = async (_bot, _cursor, _limit, session) => ({ messages: [{ id: `m-${session}`, role: 'user', content: `history ${session}` }], hasMore: false })
    sessionApi.list = async () => ({ sessions: [{ id: 's1', title: 'First' }, { id: 's2', title: 'Second' }] })
    workflowApi.sessionWorkflow = async () => ({})
    const store = useBotStore(createPinia())
    stores.push(store)
    store.bots = [{ id: 'b1', name: 'One' }, { id: 'b2', name: 'Two' }]
    store.activeBotId = 'b1'
    await settle()
    return store
  }
  try {
    await t.test('draft text and attachments stay scoped to Bot and session', async () => {
      const store = await makeStore()
      const attachment = { name: 'note.txt', dataUrl: 'data:text/plain;base64,YQ==' }
      store.composerDraft.text = 'first draft'
      store.composerDraft.attachments = [attachment]
      store.selectSession('s2')
      await settle()
      assert.equal(store.composerDraft.text, '')
      store.composerDraft.text = 'second draft'
      store.selectSession('s1')
      await settle()
      assert.equal(store.composerDraft.text, 'first draft')
      assert.equal(store.composerDraft.attachments[0].dataUrl, attachment.dataUrl)
      store.activeBotId = 'b2'
      await settle()
      assert.equal(store.composerDraft.text, '')
      store.activeBotId = 'b1'
      await settle()
      assert.equal(store.composerDraft.text, 'first draft')
    })
    await t.test('history refresh failure retains existing messages and exposes retry state', async () => {
      const store = await makeStore()
      const before = store.messages.map(m => m.id)
      chatApi.history = async () => { throw new Error('offline') }
      await store.loadMessages()
      assert.deepEqual(store.messages.map(m => m.id), before)
      assert.equal(store.messagesError, 'offline')
      assert.equal(store.messagesLoading, false)
      store.selectSession('s2')
      await settle()
      assert.equal(store.messages.length, 0, 'old conversation must not be displayed under the new title')
    })
    await t.test('late history response cannot overwrite a newer conversation', async () => {
      const store = await makeStore()
      const old = deferred()
      chatApi.history = async (_bot, _cursor, _limit, session) => session === 's1' ? old.promise : { messages: [{ id: 'new', role: 'user', content: 'new session' }] }
      const pending = store.loadMessages()
      store.selectSession('s2')
      await settle()
      old.resolve({ messages: [{ id: 'stale', role: 'user', content: 'wrong session' }] })
      await pending
      assert.equal(store.messages[0].id, 'new')
    })
    await t.test('late Bot session response cannot select a conversation from another Bot', async () => {
      const store = await makeStore()
      const old = deferred()
      sessionApi.list = async bot => bot === 'b1' ? old.promise : { sessions: [{ id: 'b2-session', title: 'Bot Two' }] }
      const pending = store.loadSessions('b1')
      store.activeBotId = 'b2'
      await settle()
      old.resolve({ sessions: [{ id: 'wrong-session' }] })
      await pending
      assert.equal(store.activeSessionId, 'b2-session')
      assert.equal(store.sessions[0].id, 'b2-session')
      assert.equal(store.sessionsLoading, false)
    })
    await t.test('busy sends are refused without mutating history', async () => {
      const store = await makeStore()
      store.replying = true
      const count = store.messages.length
      assert.equal(store.sendMessage('not allowed', [{ name: 'a.txt' }]).accepted, false)
      assert.equal(store.messages.length, count)
    })
    await t.test('connection failure restores payload only in its original draft', async () => {
      const store = await makeStore()
      const response = deferred()
      chatApi.send = () => response.promise
      const key = store.draftKey
      const result = store.sendMessage('recover this', [{ name: 'a.txt', dataUrl: 'data:text/plain;base64,YQ==' }])
      assert.equal(result.accepted, true)
      store.selectSession('s2')
      await settle()
      store.composerDraft.text = 'new conversation draft'
      response.reject(new Error('offline'))
      assert.equal((await result.completion).status, 'unknown')
      assert.equal(store.composerDraft.text, 'new conversation draft')
      assert.equal(store.getDraft(key).text, 'recover this')
      assert.equal(store.getDraft(key).attachments.length, 1)
    })
    await t.test('failed send preserves a newer draft and provides explicit recovery', async () => {
      const store = await makeStore()
      chatApi.send = async () => { throw new Error('offline') }
      const result = store.sendMessage('old draft')
      store.composerDraft.text = 'new draft'
      await result.completion
      assert.equal(store.composerDraft.text, 'new draft')
      assert.equal(store.composerDraft.recovery.text, 'old draft')
      store.restoreDraft()
      assert.equal(store.composerDraft.text, 'old draft\nnew draft')
    })
    await t.test('rejected append is recoverable and never automatically starts a new turn', async () => {
      const store = await makeStore()
      const run = deferred()
      let sends = 0
      chatApi.send = (_bot, _text, handlers) => { sends++; handlers.onStart('trace-1'); return run.promise }
      chatApi.append = async () => ({ accepted: false })
      const turn = store.sendMessage('initial')
      const append = store.appendToCurrentReply('supplement')
      assert.equal(append.accepted, true)
      assert.equal((await append.completion).status, 'rejected')
      assert.equal(store.composerDraft.text, 'supplement')
      run.resolve({ text: 'done' })
      await turn.completion
      await settle()
      assert.equal(sends, 1)
      assert.equal(store.messages.some(m => m._appended), false)
    })
    await t.test('missing append acknowledgement is unknown and marked without resend', async () => {
      const store = await makeStore()
      const run = deferred()
      chatApi.send = (_bot, _text, handlers) => { handlers.onStart('trace-2'); return run.promise }
      chatApi.append = async () => ({})
      const turn = store.sendMessage('initial')
      const append = store.appendToCurrentReply('maybe delivered')
      assert.equal((await append.completion).status, 'unknown')
      assert.equal(store.messages.at(-1)._appendUnconfirmed, true)
      assert.match(store.composerDraft.notice, /未确认/)
      run.resolve({ text: 'done' })
      await turn.completion
    })
    await t.test('append during initial connection is refused without losing input', async () => {
      const store = await makeStore()
      const run = deferred()
      chatApi.send = () => run.promise
      const turn = store.sendMessage('initial')
      assert.equal(store.appendToCurrentReply('wait for trace').accepted, false)
      run.resolve({ text: 'done' })
      await turn.completion
    })
    await t.test('removing the last conversation removes its stale transcript', async () => {
      const store = await makeStore()
      store.sessions = [{ id: 's1' }]
      sessionApi.remove = async () => ({})
      await store.deleteSession('s1')
      assert.equal(store.activeSessionId, null)
      assert.deepEqual(store.messages, [])
      assert.equal(store.hasMore, false)
    })
    await t.test('history plus resume snapshot renders one assistant per trace', async () => {
      const store = await makeStore()
      const history = { id: 'persisted', role: 'assistant', traceId: 'live-trace', sessionId: 's1', content: 'prefix', parts: [{ type: 'text', content: 'prefix' }], streaming: true, streamSeq: 5 }
      chatApi.history = async () => ({ messages: [history] })
      chatApi.activeTasks = async () => [{ traceId: 'live-trace', sessionId: 's1' }]
      chatApi.resume = async (_bot, _trace, handlers) => {
        assert.equal(handlers.sinceSeq, 5)
        assert.equal(handlers.sessionId, 's1')
        handlers.onSnapshot(history)
        handlers.onTextDelta(' suffix')
        handlers.onSequence(6)
        return { traceId: 'live-trace', text: 'prefix suffix', parts: [{ type: 'text', content: 'prefix suffix' }], streamSeq: 7 }
      }
      await store.loadMessages()
      await settle()
      const rows = store.messages.filter(message => message.role === 'assistant' && message.traceId === 'live-trace')
      assert.equal(rows.length, 1)
      assert.equal(rows[0].content, 'prefix suffix')
      assert.equal(rows[0].streaming, false)
      assert.equal(rows[0].streamSeq, 7)
    })
    await t.test('done payload keeps authoritative interleaved parts', async () => {
      const store = await makeStore()
      const parts = [{ type: 'text', content: 'before' }, { type: 'tool', id: 'tool', name: 'exec', status: 'success' }, { type: 'text', content: 'after' }]
      chatApi.send = async (_bot, _text, handlers) => { handlers.onStart('done-trace'); handlers.onTextDelta('partial'); return { traceId: 'done-trace', text: 'beforeafter', parts, toolCalls: [parts[1]] } }
      await store.sendMessage('run it').completion
      const message = store.messages.find(message => message.role === 'assistant' && message.traceId === 'done-trace')
      assert.equal(message.parts.length, 3)
      assert.equal(message.parts[2].content, 'after')
      assert.equal(message.content, 'beforeafter')
    })
    await t.test('persisted history replaces optimistic rows without duplicates', async () => {
      const store = await makeStore()
      chatApi.send = async (_bot, _text, handlers) => { handlers.onStart('persisted-trace'); return { traceId: 'persisted-trace', text: 'answer' } }
      await store.sendMessage('unique prompt').completion
      chatApi.history = async () => ({ messages: [{ id: 'server-answer', role: 'assistant', traceId: 'persisted-trace', sessionId: 's1', content: 'answer', streaming: false }, { id: 'server-prompt', role: 'user', traceId: 'persisted-trace', sessionId: 's1', content: 'unique prompt' }] })
      await store.loadMessages()
      assert.equal(store.messages.filter(message => message.traceId === 'persisted-trace' && message.role === 'assistant').length, 1)
      assert.equal(store.messages.filter(message => message.content === 'unique prompt').length, 1)
    })
    await t.test('switching Bot disconnects locally without sending abort', async () => {
      const store = await makeStore()
      const run = deferred()
      let aborts = 0
      chatApi.abort = async () => { aborts++; return {} }
      let signal
      chatApi.send = (_bot, _text, handlers) => { signal = handlers.signal; handlers.onStart('background-trace'); return run.promise }
      const turn = store.sendMessage('continue in background')
      store.selectBot('b2')
      await settle()
      assert.equal(signal.aborted, true)
      assert.equal(aborts, 0)
      run.resolve({ text: 'old output' })
      await turn.completion
      assert.equal(store.messages.some(message => message.content === 'old output'), false)
    })
    await t.test('failed explicit stop keeps the current stream connected', async () => {
      const store = await makeStore()
      const run = deferred()
      let signal
      chatApi.send = (_bot, _text, handlers) => { signal = handlers.signal; handlers.onStart('stop-trace'); return run.promise }
      const turn = store.sendMessage('run')
      chatApi.abortSession = async () => { throw new Error('offline') }
      assert.equal(await store.stopReply(), false)
      assert.equal(signal.aborted, false)
      assert.equal(store.replying, true)
      assert.match(store.composerDraft.notice, /停止失败/)
      run.resolve({ text: 'done' })
      await turn.completion
    })
    await t.test('stale resume completion cannot clear a newer subscription for the same trace', async () => {
      const store = await makeStore()
      const old = deferred(), latest = deferred()
      let subscriptions = 0
      chatApi.activeTasks = async (_bot, sessionId) => sessionId === 's1' ? [{ traceId: 'same-trace', sessionId }] : []
      chatApi.resume = () => ++subscriptions === 1 ? old.promise : latest.promise
      await store.resumeInFlightTasks()
      store.selectSession('s2')
      await settle()
      store.selectSession('s1')
      await settle()
      assert.equal(subscriptions, 2)
      old.resolve({ text: 'outdated' })
      await settle()
      assert.equal(store.replying, true)
      assert.equal(store.messages.some(message => message.content === 'outdated'), false)
      latest.resolve({ text: 'latest', traceId: 'same-trace' })
      await settle()
      assert.equal(store.replying, false)
      assert.equal(store.messages.filter(message => message.content === 'latest').length, 1)
    })
    await t.test('continuation discovers its own trace instead of using the session id', async () => {
      const store = await makeStore()
      const traces = []
      chatApi.activeTasks = async () => [{ traceId: 'continuation-unique', sessionId: 's1' }]
      chatApi.resume = async (_bot, trace) => { traces.push(trace); return { text: 'continued' } }
      await store.resumeContinuation('s1')
      await settle()
      assert.deepEqual(traces, ['continuation-unique'])
    })
    await t.test('stale history cannot revive a completed trace and duplicate traces are collapsed', async () => {
      const store = await makeStore()
      store.messages = [{ id: 'local-final', traceId: 'finished', role: 'assistant', content: 'final', streaming: false, streamSeq: 100 }, { id: 'old-duplicate', traceId: 'finished', role: 'assistant', content: 'old', streaming: true, streamSeq: 90 }]
      chatApi.history = async () => ({ messages: [{ id: 'stale-1', traceId: 'finished', role: 'assistant', content: 'stale', streaming: true, streamSeq: 90 }, { id: 'stale-2', traceId: 'finished', role: 'assistant', content: 'stale duplicate', streaming: true, streamSeq: 80 }] })
      await store.loadMessages()
      assert.equal(store.messages.length, 1)
      assert.equal(store.messages[0].content, 'final')
      assert.equal(store.messages[0].streaming, false)
    })
    await t.test('switching conversations while reconnecting clears the pending indicator', async () => {
      const store = await makeStore()
      const old = deferred()
      chatApi.history = async (_bot, _cursor, _limit, session) => session === 's1' ? old.promise : { messages: [] }
      const reconnect = store.reconnectStream()
      assert.equal(store.reconnecting, true)
      store.selectSession('s2')
      await settle()
      assert.equal(store.reconnecting, false)
      old.resolve({ messages: [] })
      await reconnect
      assert.equal(store.reconnecting, false)
    })
    await t.test('terminal error preserves canonical tool results even without text', async () => {
      const store = await makeStore()
      const tool = { type: 'tool', id: 'failed-tool', name: 'exec', status: 'error', output: { stderr: 'command failed' } }
      chatApi.send = async (_bot, _text, handlers) => {
        handlers.onStart('error-trace')
        throw Object.assign(new Error('task failed'), { recoverable: false, traceId: 'error-trace', text: '', parts: [tool], toolCalls: [tool], streamSeq: 8, status: 'failed' })
      }
      await store.sendMessage('failing task').completion
      const message = store.messages.find(message => message.traceId === 'error-trace' && message.role === 'assistant')
      assert.equal(message.parts[0].id, 'failed-tool')
      assert.equal(message.toolCalls[0].output.stderr, 'command failed')
      assert.equal(message.streaming, false)
      assert.equal(message.streamSeq, 8)
      assert.equal(store.streamError, 'task failed')
    })
    await t.test('persistence warnings remain accepted without triggering a resend', async () => {
      const store = await makeStore()
      chatApi.send = async (_bot, _text, handlers) => { handlers.onStart('save-warning'); return { traceId: 'save-warning', text: 'finished', persistenceError: 'save failed', status: 'completed' } }
      const result = store.sendMessage('save warning')
      assert.equal((await result.completion).status, 'completed')
      assert.equal(store.messages.find(message => message.role === 'assistant' && message.traceId === 'save-warning').persistenceError, 'save failed')
      const run = deferred()
      chatApi.send = (_bot, _text, handlers) => { handlers.onStart('append-warning'); return run.promise }
      const next = store.sendMessage('next')
      chatApi.append = async () => ({ accepted: true, persistenceError: 'append save failed' })
      const append = store.appendToCurrentReply('accepted supplement')
      assert.equal((await append.completion).status, 'accepted')
      assert.match(store.composerDraft.notice, /已接收.*保存失败/)
      assert.equal(store.composerDraft.recovery, null)
      run.resolve({ text: 'done' })
      await next.completion
    })
    await t.test('failed stop request propagates failure and leaves stream state intact', async () => {
      const store = await makeStore()
      store.replying = true
      chatApi.abortSession = async () => { throw new Error('stop unavailable') }
      await assert.rejects(store.abortSession('s1'), /stop unavailable/)
      assert.equal(store.replying, true)
    })
  } finally {
    for (const store of stores) store.$dispose()
    for (const timer of intervals) clearInterval(timer)
    globalThis.setInterval = originalInterval
    for (const [api, methods] of originals) Object.assign(api, methods)
    await server.close()
  }
})
