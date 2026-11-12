import assert from 'node:assert/strict'
import test from 'node:test'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const src = join(dirname(fileURLToPath(import.meta.url)), '..', 'src', 'components', 'ProvidersManager.vue')
const text = readFileSync(src, 'utf8')

test('saving a provider omits an empty key and never copies the masked value', () => {
  assert.match(text, /if \(apiKeyInput\.value\) payload\.apiKey = apiKeyInput\.value/)
  assert.doesNotMatch(text, /payload\.apiKey = active\.value\.apiKey/)
  assert.doesNotMatch(text, /apiKeyInput\.value = active\.value\.apiKey/)
})

test('clearing a key is an explicit empty string', () => {
  assert.match(text, /data-testid="provider-clear-key"/)
  assert.match(text, /providerApi\.update\(active\.value\.id, \{ apiKey: '' \}\)/)
})

test('a runtime apply failure is not reported as a plain success', () => {
  assert.match(text, /runtimeApplied === false/)
  assert.match(text, /runtimeMessage/)
})
