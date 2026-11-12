import assert from 'node:assert/strict'
import { createSSRApp, h } from 'vue'
import { renderToString } from '@vue/server-renderer'
import { llmStatusText } from '../src/components/bot/llmStatus.js'

const text = llmStatusText({
  desiredMain: 'model-sol-main',
  desiredLight: 'model-terra-light',
  desiredVision: 'model-vision',
  effectiveMain: 'model-old',
  effectiveLight: '',
  effectiveVision: '',
  error: 'model "model-sol-main" is not configured on an enabled provider'
})
assert.match(text, /期望主模型 model-sol-main/)
assert.match(text, /正在使用 model-old/)
assert.doesNotMatch(text, /正在使用 model-sol-main/)
assert.match(text, /未生效/)
assert.match(text, /not configured/)

const app = createSSRApp({
  render() {
    return h('div', { 'data-testid': 'bot-llm-status' }, text)
  }
})
const html = await renderToString(app)
assert.match(html, /data-testid="bot-llm-status"/)
assert.match(html, /正在使用 model-old/)
assert.doesNotMatch(html, /正在使用 model-sol-main/)
console.log('llm status render ok')
