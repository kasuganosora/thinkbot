import assert from 'node:assert/strict'
import test from 'node:test'
import { createServer } from 'vite'
import { createSSRApp } from 'vue'
import { createPinia } from 'pinia'
import { renderToString } from '@vue/server-renderer'
import { secretRequestFromOutput, secretRequestsForMessage, submitSecretForm } from '../src/utils/secretInput.js'

const payload = { status: 'requested', input_mode: 'web_form', question_id: 'request-1', name: 'demo', purpose: 'Fictional test credential', placeholder: '{{secret:demo}}', expires_at: Math.floor(Date.now() / 1000) + 600 }
const fictionalValue = 'fictional-secret-only-for-test'

test('secret form metadata is allowlisted and deduplicated across live and historical message shapes', () => {
  assert.deepEqual(secretRequestFromOutput({ ...payload, value: fictionalValue }), payload)
  assert.equal(secretRequestFromOutput({ ...payload, input_mode: 'chat_reply' }), null)
  assert.equal(secretRequestFromOutput({ ...payload, question_id: '' }), null)
  const message = { role: 'assistant', toolCalls: [{ name: 'secret_input', output: payload }], parts: [{ type: 'tool', name: 'secret_input', output: { secretRequest: payload } }] }
  assert.deepEqual(secretRequestsForMessage(message), [payload])
  assert.deepEqual(secretRequestsForMessage({ ...message, role: 'user' }), [])
  assert.deepEqual(secretRequestsForMessage({ role: 'assistant', toolCalls: [{ name: 'unrelated_tool', output: payload }] }), [])
})

test('only dedicated endpoint receives raw input; continuation receives safe acknowledgement', async () => {
  const requests = [], chats = []
  const completion = Promise.resolve({ status: 'completed' })
  const result = await submitSecretForm({ questionId: 'request/1', botId: 'b1', sessionId: 's1', value: fictionalValue }, {
    post: async (...args) => { requests.push(args); return { status: 'stored', userText: '[密钥已提供]' } },
    isCurrent: () => true,
    continueChat: text => { chats.push(text); return { accepted: true, completion } },
  })
  assert.deepEqual(requests, [['POST', '/api/chat/secrets/request%2F1', { botId: 'b1', sessionId: 's1', value: fictionalValue }]])
  assert.deepEqual(chats, ['[密钥已提供]'])
  assert.equal(result.status, 'stored')
  assert.equal(result.continued, true)
  assert.equal(result.completion, completion)
  assert(!JSON.stringify(result).includes(fictionalValue))
})

test('expired, restarted, unauthorized and disconnected requests never fallback to chat', async () => {
  for (const code of ['not_found', 'expired', 'unauthorized', 'network']) {
    const result = await submitSecretForm({ questionId: 'old-request', botId: 'b1', sessionId: 's1', value: fictionalValue }, {
      post: async () => { throw Object.assign(new Error(fictionalValue), { code }) },
      isCurrent: () => true,
      continueChat: () => assert.fail('secret must not be sent as normal chat'),
    })
    assert.deepEqual(result, { status: 'failed' })
  }
})

test('cancellation discards entered value and sends no continuation', async () => {
  const requests = []
  const result = await submitSecretForm({ questionId: 'request-1', botId: 'b1', sessionId: 's1', value: fictionalValue, cancel: true }, {
    post: async (...args) => { requests.push(args); return { status: 'cancelled' } },
    isCurrent: () => true,
    continueChat: () => assert.fail('cancellation must not become a chat message'),
  })
  assert.deepEqual(requests[0][2], { botId: 'b1', sessionId: 's1', cancel: true })
  assert.deepEqual(result, { status: 'cancelled' })
})

test('navigation and local delivery refusal report stored without posting to another conversation or retrying', async () => {
  for (const current of [false, true]) {
    const chats = []
    const result = await submitSecretForm({ questionId: 'request-1', botId: 'b1', sessionId: 's1', value: fictionalValue }, {
      post: async () => ({ status: 'stored', userText: '[密钥已提供]' }),
      isCurrent: () => current,
      continueChat: text => { chats.push(text); return { accepted: false } },
    })
    assert.equal(result.status, 'stored')
    assert.equal(result.continued, false)
    assert.equal(chats.length, current ? 1 : 0)
  }
})

test('password card renders protected input, purpose, cancellation and expired state', async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: 'custom' })
  try {
    const { default: Card } = await server.ssrLoadModule('/src/components/SecretInputCard.vue')
    const render = request => renderToString(createSSRApp(Card, { request, botId: 'b1', sessionId: 's1' }).use(createPinia()))
    const html = await render(payload)
    assert.match(html, /type="password"/)
    assert.match(html, /autocomplete="off"/)
    assert.match(html, /Fictional test credential/)
    assert.match(html, /取消请求/)
    const expired = await render({ ...payload, expires_at: 1 })
    assert.match(expired, /请求已过期/)
    assert(!expired.includes('type="password"'))
  } finally { await server.close() }
})
