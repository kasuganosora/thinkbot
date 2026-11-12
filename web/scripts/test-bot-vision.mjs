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
  assert.match(form, /正在运行时，保存成功会切换到新的模型/)
})

test('vision is sent with the bot update and still loaded from its config key', () => {
  assert.match(settings, /visionKey\(botId\)/)
  assert.match(settings, /`bot\.\$\{botId\}\.vision`/)
  assert.match(settings, /configApi\.get\(visionKey\(id\)\)/)
  assert.match(settings, /llmVision: f\.llmVision \|\| ''/)
  assert.doesNotMatch(settings, /configApi\.batchSet/)
})

test('clearing the selector stays writable', () => {
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

test('a saved model reports whether the running bot actually switched', () => {
  assert.match(settings, /saved\.applied/)
  assert.match(settings, /saved\.pending/)
  assert.match(settings, /已切换到保存的模型/)
})
