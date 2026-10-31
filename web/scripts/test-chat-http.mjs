import assert from 'node:assert/strict'
import test from 'node:test'
import { createServer } from 'vite'

test('JSON chat errors retain canonical safe text without copying the request payload', async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: 'custom' })
  const originalFetch = globalThis.fetch
  try {
    const { request } = await server.ssrLoadModule('/src/api/http.js')
    globalThis.fetch = async () => new Response(JSON.stringify({
      code: 500, message: 'history unavailable', data: { userText: '[密钥已隐藏]', other: 'not copied' },
    }), { status: 500 })
    await assert.rejects(request('POST', '/api/chat/append', { text: 'fictional-private-credential' }), err => {
      assert.equal(err.userText, '[密钥已隐藏]')
      assert.equal(err.code, 500)
      assert(!JSON.stringify(err).includes('fictional-private-credential'))
      assert(!JSON.stringify(err).includes('not copied'))
      return true
    })
    globalThis.fetch = async () => new Response(JSON.stringify({ code: 500, message: 'offline', data: null }), { status: 500 })
    await assert.rejects(request('POST', '/api/chat/append', {}), err => !('userText' in err))
  } finally {
    globalThis.fetch = originalFetch
    await server.close()
  }
})
