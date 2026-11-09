import assert from 'node:assert/strict'
import test from 'node:test'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

// The vision slot is a per-bot config key, not a bot_definitions column, so the
// settings UI owns both halves of the round trip. These checks keep the wiring
// from silently regressing (especially the clear/disable path).
const src = join(dirname(fileURLToPath(import.meta.url)), '..', 'src')
const form = readFileSync(join(src, 'components/bot/BotBasicForm.vue'), 'utf8')
const settings = readFileSync(join(src, 'views/BotSettings.vue'), 'utf8')

test('bot form exposes a vision model selector', () => {
  assert.match(form, /v-model="form\.llmVision"/)
  assert.match(form, /data-testid="bot-llm-vision"/)
  assert.match(form, /留空 = 不启用图片转写/)
  assert.match(form, /停用并重新启用 Bot 才生效/)
})

test('vision is read from and written to the per-bot config key', () => {
  assert.match(settings, /visionKey\(botId\)/)
  assert.match(settings, /`bot\.\$\{botId\}\.vision`/)
  assert.match(settings, /configApi\.get\(visionKey\(id\)\)/)
  assert.match(settings, /configApi\.batchSet\(\{ \[visionKey\(f\.id\)\]/)
})

test('clearing the selector stays writable', () => {
  // PUT /api/config/:key binds Value with `required` and rejects an empty
  // string; the batch endpoint accepts it. Clearing = disabling transcription.
  assert.doesNotMatch(settings, /configApi\.set\(visionKey/)
  assert.match(settings, /f\.llmVision \|\| ''/)
})

test('an unset key degrades to disabled instead of surfacing a 404', () => {
  assert.match(settings, /从未配置过时后端返回 404/)
  assert.match(settings, /value = ''/)
})

test('switching bots cannot let a stale config response overwrite the form', () => {
  assert.match(settings, /if \(form\.value\.id !== id\) return/)
  assert.match(settings, /if \(!form\.value\.llmVision\) form\.value\.llmVision = value/)
})

test('a changed vision model warns that a bot restart is required', () => {
  assert.match(settings, /visionChanged/)
  assert.match(settings, /停用并重新启用 Bot 后生效/)
})
