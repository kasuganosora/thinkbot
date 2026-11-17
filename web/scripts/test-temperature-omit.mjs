import assert from 'node:assert/strict'
import test from 'node:test'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import {
  OMIT_SAMPLING_PARAM,
  samplingAllowed,
  normalizeBotTemperature,
  normalizeModelSampling,
  formatSamplingBadge
} from '../src/utils/sampling.js'

const srcDir = join(dirname(fileURLToPath(import.meta.url)), '..', 'src')
const botFormSrc = readFileSync(join(srcDir, 'components/bot/BotBasicForm.vue'), 'utf8')
const botSettingsSrc = readFileSync(join(srcDir, 'views/BotSettings.vue'), 'utf8')
const providersSrc = readFileSync(join(srcDir, 'components/ProvidersManager.vue'), 'utf8')

test('sampling helpers preserve -1 omit and 0 unset while rejecting invalid negatives', () => {
  assert.equal(OMIT_SAMPLING_PARAM, -1)

  for (const ok of [-1, 0, 0.1, 0.7, 1, 2]) {
    assert.equal(samplingAllowed(ok), true, `expected ${ok} to be allowed`)
  }
  for (const bad of [-0.5, -2, -0.01, NaN, Infinity, null, undefined, '-1']) {
    assert.equal(samplingAllowed(bad), false, `expected ${String(bad)} to be rejected`)
  }

  assert.equal(normalizeBotTemperature(-1), -1)
  assert.equal(normalizeBotTemperature(0), 0)
  assert.equal(normalizeBotTemperature(0.7), 0.7)
  assert.equal(normalizeBotTemperature(undefined), 0.7)
  assert.equal(normalizeBotTemperature(null), 0.7)

  assert.equal(normalizeModelSampling(-1), -1)
  assert.equal(normalizeModelSampling(0), 0)
  assert.equal(normalizeModelSampling(0.95), 0.95)
  assert.equal(normalizeModelSampling(undefined), 0)
  assert.equal(normalizeModelSampling(null), 0)

  assert.equal(formatSamplingBadge('T', -1), 'T omit')
  assert.equal(formatSamplingBadge('P', -1), 'P omit')
  assert.equal(formatSamplingBadge('T', 0.7), 'T 0.7')
  assert.equal(formatSamplingBadge('P', 0.95), 'P 0.95')
  assert.equal(formatSamplingBadge('T', 0), '')
  assert.equal(formatSamplingBadge('P', 0), '')
})

test('BotBasicForm allows -1 omit without a 0-min slider snapping values away', () => {
  assert.doesNotMatch(botFormSrc, /<t-slider[^>]*v-model="form\.temperature"/)
  assert.match(botFormSrc, /<t-input-number[^>]*v-model="form\.temperature"[^>]*:min="-1"[^>]*:max="2"/)
  assert.match(botFormSrc, /data-testid="bot-temperature"/)
  assert.match(botFormSrc, /omit/)
  assert.match(botFormSrc, /-1 表示请求中不发送 temperature/)
})

test('BotSettings preserves -1 and 0 across load, validation, save, and echo', () => {
  assert.match(botSettingsSrc, /normalizeBotTemperature\(b\.temperature\)/)
  assert.match(botSettingsSrc, /samplingAllowed\(f\.temperature\)/)
  assert.doesNotMatch(botSettingsSrc, /temperature:\s*f\.temperature\s*\|\|/)
})

test('ProvidersManager avoids || 0 fallbacks and renders omit for both temperature and topP', () => {
  assert.match(providersSrc, /m\.topP === -1 \? 'omit' : m\.topP/)
  assert.doesNotMatch(providersSrc, /temperature:\s*f\.temperature\s*\|\|\s*0/)
  assert.doesNotMatch(providersSrc, /topP:\s*f\.topP\s*\|\|\s*0/)
  assert.match(providersSrc, /normalizeModelSampling\(f\.temperature\)/)
  assert.match(providersSrc, /normalizeModelSampling\(f\.topP\)/)
})
