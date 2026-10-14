import assert from 'node:assert/strict'
import test from 'node:test'
import { extractMessageArtifacts, normalizeArtifactPath } from '../src/utils/messageArtifacts.js'

const write = (path, overrides = {}) => ({ id: 'write-1', type: 'tool', name: 'sandbox_write_file', status: 'success', output: { success: true, path }, ...overrides })
const message = (calls, fields = {}) => ({ role: 'assistant', sessionId: 's1', parts: calls, ...fields })
const scope = { botId: 'b1', sessionId: 's1' }

test('extracts only writes confirmed by this assistant message', () => {
  const files = extractMessageArtifacts(message([write('/data/reports/结果.md')]), scope)
  assert.equal(files.length, 1)
  assert.equal(files[0].path, '/data/reports/结果.md')
  assert.equal(files[0].name, '结果.md')
  assert.equal(files[0].toolCallId, 'write-1')
  assert.equal(files[0].toolName, 'write_file')
})

test('recognizes both supported write tools with and without the sandbox prefix', () => {
  const names = ['write_file', 'sandbox_write_file', 'replace_in_file', 'sandbox_replace_in_file']
  assert.equal(extractMessageArtifacts(message(names.map((name, i) => write(`result-${i}.md`, { name }))), scope).length, 4)
})

test('does not infer artifacts from reading, listing, commands, prose, inputs, or unrelated tools', () => {
  const calls = ['read_file', 'list_dir', 'exec', 'sandbox_exec', 'delete_file', 'sandbox_edit_file', 'sandbox_sandbox_write_file'].map(name => write('secret.md', { name }))
  calls.push(write('unused', { output: { success: true }, input: { path: '/data/input-only.md' } }))
  calls.push({ type: 'text', name: 'write_file', status: 'success', output: { success: true, path: '/data/fake.md' } })
  const msg = message(calls, { content: 'Result: /data/not-a-confirmed-artifact.md', files: [{ path: '/data/another.md' }] })
  assert.deepEqual(extractMessageArtifacts(msg, scope), [])
})

test('requires success status and an explicit boolean success from tool output', () => {
  const calls = ['running', 'error', 'timeout', 'cancelled', 'killed', 'superseded', undefined].map(status => write('wrong.md', { status }))
  for (const success of [false, 'true', 1, null, undefined]) calls.push(write('wrong.md', { output: { success, path: '/data/wrong.md' } }))
  assert.deepEqual(extractMessageArtifacts(message(calls), scope), [])
})

test('deduplicates mirrored parts/toolCalls and equivalent workspace paths', () => {
  const msg = message([write('reports/result.md'), write('./reports/result.md')], { toolCalls: [write('/data/reports/result.md'), write('/data/second.md', { id: 'write-2' })] })
  assert.deepEqual(extractMessageArtifacts(msg, scope).map(file => file.path), ['/data/reports/result.md', '/data/second.md'])
})

test('supports persisted JSON payloads while rejecting malformed structures', () => {
  const valid = write('/data/history.json', { output: JSON.stringify({ success: true, path: '/data/history.json' }) })
  const msg = message(JSON.stringify([valid]), { toolCalls: JSON.stringify([valid]) })
  assert.equal(extractMessageArtifacts(msg, scope).length, 1)
  for (const value of ['{broken', 'null', '{}', 1, null]) {
    assert.deepEqual(extractMessageArtifacts(message(value, { toolCalls: value }), scope), [])
    assert.deepEqual(extractMessageArtifacts(message([write('/data/no.md', { output: value })]), scope), [])
  }
})

test('rejects messages from another Bot/session and never scans user messages', () => {
  assert.deepEqual(extractMessageArtifacts(message([write('one.md')], { sessionId: 's2' }), scope), [])
  assert.deepEqual(extractMessageArtifacts(message([write('one.md')], { botId: 'b2' }), scope), [])
  assert.deepEqual(extractMessageArtifacts(message([write('one.md')], { role: 'user' }), scope), [])
  assert.deepEqual(extractMessageArtifacts(message([write('one.md')]), { botId: 'b1' }), [])
  assert.equal(extractMessageArtifacts(message([write('one.md')], { sessionId: 7 }), { ...scope, sessionId: '7' }).length, 1)
})

test('legacy untagged messages remain scoped to the supplied single message', () => {
  const first = message([write('first.md')], { sessionId: undefined })
  const second = message([write('second.md')], { sessionId: undefined })
  assert.deepEqual(extractMessageArtifacts(first, scope).map(file => file.name), ['first.md'])
  assert.deepEqual(extractMessageArtifacts(second, scope).map(file => file.name), ['second.md'])
})

test('normalizes workspace-relative paths without changing filename characters', () => {
  assert.equal(normalizeArtifactPath('reports/a b.md'), '/data/reports/a b.md')
  assert.equal(normalizeArtifactPath('./reports//a.md'), '/data/reports/a.md')
  assert.equal(normalizeArtifactPath('/data/reports/a.md'), '/data/reports/a.md')
  assert.equal(normalizeArtifactPath('100%.txt'), '/data/100%.txt')
  assert.equal(normalizeArtifactPath('literal%20name.txt'), '/data/literal%20name.txt')
})

test('rejects traversal, encoded escapes, controls, URLs, and unknown absolute roots', () => {
  const invalid = [
    '../secret', 'a/../secret', '/data/../secret', '/data/./../secret',
    '/data/%2e%2e/secret', '/data/%252e%252e/secret', '/data/a%2f..%2fsecret',
    'https://example.com/a', '//example.com/a', 'file:///etc/passwd', 'javascript:alert(1)',
    '/etc/passwd', '/workspace/secret', '/database/file', 'C:\\secret', '/data/a\\..\\secret',
    '/data/bad\nname', '/data/bad\0name', '/data/%00name', '/data/bad\u007fname',
    '', '/', '/data', '/data/', '.', 'reports/', ' padded.txt ', null, {},
  ]
  for (const path of invalid) assert.equal(normalizeArtifactPath(path), '', String(path))
  assert.deepEqual(extractMessageArtifacts(message(invalid.map(path => write(path))), scope), [])
})

test('does not mutate message content or tool results', () => {
  const call = write('/data/result.md')
  Object.freeze(call.output); Object.freeze(call)
  const msg = Object.freeze(message(Object.freeze([call])))
  assert.equal(extractMessageArtifacts(msg, scope).length, 1)
  assert.equal(call.output.path, '/data/result.md')
})
