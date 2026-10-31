import assert from 'node:assert/strict'
import test from 'node:test'
import { configApplyFeedback } from '../src/utils/configApplyFeedback.js'
import { MCP_SECRET_MASK, mcpRows, mcpRowsToObject, validateMcpImport } from '../src/utils/mcpConfigForm.js'

test('saving and applying container limits remain distinct outcomes', () => {
  assert.equal(configApplyFeedback(null).message, '配置已保存')
  assert.equal(configApplyFeedback({ applied: true }).message, '配置已保存并生效')
  for (const result of [{ needsRecreate: true }, { applied: false }, { failed: true }]) {
    const feedback = configApplyFeedback(result)
    assert.equal(feedback.theme, 'warning')
    assert.doesNotMatch(feedback.message, /已保存并生效/)
  }
})

test('mixed CPU application results disclose failures and recreation', () => {
  const feedback = configApplyFeedback({ applied: false, failed: true, needsRecreate: true, failedBots: ['bot-a'] })
  assert.match(feedback.message, /应用失败/)
  assert.match(feedback.message, /需要重建/)
})

test('MCP mask survives an unchanged save but cannot be moved to a different key', () => {
  const rows = mcpRows({ API_TOKEN: MCP_SECRET_MASK, MODE: 'safe' })
  assert.deepEqual(mcpRowsToObject(rows), { API_TOKEN: MCP_SECRET_MASK, MODE: 'safe' })
  rows[0].key = 'MY_VALUE' // A non-sensitive name must not turn the mask into a value.
  assert.throws(() => mcpRowsToObject(rows), /重新填写真实值/)
  rows[0].value = 'fictional-replacement-token'
  assert.equal(mcpRowsToObject(rows).MY_VALUE, 'fictional-replacement-token')
})

test('MCP users can clear a credential or replace it without exposing its previous value', () => {
  const rows = mcpRows({ Authorization: MCP_SECRET_MASK })
  rows[0].value = ''
  assert.deepEqual(mcpRowsToObject(rows), { Authorization: '' })
  assert.throws(() => mcpRowsToObject([{ key: 'TOKEN', value: MCP_SECRET_MASK }]), /重新填写/)
})

test('duplicate MCP keys cannot silently discard an edited credential', () => {
  assert.throws(() => mcpRowsToObject([{ key: 'TOKEN', value: 'one' }, { key: ' TOKEN ', value: 'two' }]), /重复/)
})

test('MCP import rejects masked snapshots and malformed top-level shapes', () => {
  for (const config of [null, {}, { mcpServers: [] }, { mcpServers: {} }, { mcpServers: { x: null } }]) {
    assert.throws(() => validateMcpImport(config))
  }
  for (const field of ['env', 'headers']) {
    assert.throws(() => validateMcpImport({ mcpServers: { x: { [field]: { TOKEN: MCP_SECRET_MASK } } } }), /无法用于导入/)
  }
  assert.doesNotThrow(() => validateMcpImport({ mcpServers: { x: { command: 'example', env: { TOKEN: 'fictional-token' } } } }))
})
