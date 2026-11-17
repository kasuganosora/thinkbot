import assert from 'node:assert/strict'
import test from 'node:test'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { createServer } from 'vite'
import { createSSRApp, h } from 'vue'
import { renderToString } from 'vue/server-renderer'
import { createPinia, setActivePinia } from 'pinia'
import {
  OMIT_SAMPLING_PARAM,
  samplingAllowed,
  normalizeBotTemperature,
  normalizeModelSampling,
  formatSamplingBadge
} from '../src/utils/sampling.js'

const webRoot = join(dirname(fileURLToPath(import.meta.url)), '..')
const srcDir = join(webRoot, 'src')

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

test('BotBasicForm removes ineffective temperature editor and shows exact model-follow guidance', () => {
  const botFormSrc = readFileSync(join(srcDir, 'components/bot/BotBasicForm.vue'), 'utf8')
  assert.doesNotMatch(botFormSrc, /v-model="form\.temperature"/)
  assert.doesNotMatch(botFormSrc, /data-testid="bot-temperature"/)
  assert.doesNotMatch(botFormSrc, /data-testid="bot-temperature-omit"/)
  assert.doesNotMatch(botFormSrc, /-1 表示请求中不发送 temperature/)
  assert.ok(
    botFormSrc.includes('生成参数跟随模型，请在模型服务配置'),
    'BotBasicForm must display exact guidance: 生成参数跟随模型，请在模型服务配置'
  )
})

test('ProvidersManager keeps the real -1 editor, save normalization, and T omit / P omit echo', () => {
  const providersSrc = readFileSync(join(srcDir, 'components/ProvidersManager.vue'), 'utf8')
  assert.match(providersSrc, /v-model="modelDialog\.form\.temperature"[^>]*:min="-1"[^>]*:max="2"/)
  assert.match(providersSrc, /v-model="modelDialog\.form\.topP"[^>]*:min="-1"[^>]*:max="1"/)
  assert.match(providersSrc, /T \{\{ m\.temperature === -1 \? 'omit' : m\.temperature \}\}/)
  assert.match(providersSrc, /P \{\{ m\.topP === -1 \? 'omit' : m\.topP \}\}/)
  assert.doesNotMatch(providersSrc, /temperature:\s*f\.temperature\s*\|\|\s*0/)
  assert.doesNotMatch(providersSrc, /topP:\s*f\.topP\s*\|\|\s*0/)
  assert.match(providersSrc, /normalizeModelSampling\(f\.temperature\)/)
  assert.match(providersSrc, /normalizeModelSampling\(f\.topP\)/)
})

test('BotSettings saveBasic never sends or overwrites legacy bot temperature', async () => {
  const botSettingsSrc = readFileSync(join(srcDir, 'views/BotSettings.vue'), 'utf8')
  assert.doesNotMatch(botSettingsSrc, /temperature:\s*f\.temperature/)
  assert.doesNotMatch(botSettingsSrc, /temperature:\s*echoedTemp/)

  const require = createRequire(import.meta.url)
  const tdesignCjs = require('tdesign-vue-next')
  const origSuccess = tdesignCjs.MessagePlugin.success
  const origWarning = tdesignCjs.MessagePlugin.warning
  const origError = tdesignCjs.MessagePlugin.error
  tdesignCjs.MessagePlugin.success = () => {}
  tdesignCjs.MessagePlugin.warning = () => {}
  tdesignCjs.MessagePlugin.error = () => {}

  const vite = await createServer({
    root: webRoot,
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
    ssr: {
      noExternal: ['@xterm/xterm', '@xterm/addon-fit', 'vue-router', 'tdesign-vue-next']
    },
    plugins: [
      {
        name: 'virtual-router-and-tdesign',
        enforce: 'pre',
        resolveId(id) {
          if (id === 'tdesign-vue-next') return '\0virtual:tdesign-vue-next'
          if (id === 'vue-router') return '\0virtual:vue-router'
          if (id === '@xterm/xterm' || id === '@xterm/addon-fit') return '\0virtual:xterm'
          return null
        },
        load(id) {
          if (id === '\0virtual:tdesign-vue-next') {
            return `
              export const MessagePlugin = { success() {}, warning() {}, error() {}, info() {} }
              export const DialogPlugin = { confirm() { return { destroy() {} } } }
            `
          }
          if (id === '\0virtual:vue-router') {
            return `
              export function useRoute() { return { params: { id: 'bot-legacy-temp' }, query: {} } }
              export function useRouter() { return { push() {} } }
            `
          }
          if (id === '\0virtual:xterm') {
            return `export class Terminal {} export class FitAddon {}`
          }
          return null
        }
      }
    ]
  })

  function stubTdesignComponents(app) {
    const passSlot = {
      inheritAttrs: false,
      setup(_, { slots }) {
        return () => h('div', null, slots.default ? slots.default() : [])
      }
    }
    for (const tag of ['t-card', 't-form', 't-form-item', 't-button', 't-tag']) {
      app.component(tag, passSlot)
    }
    for (const tag of ['t-input', 't-select', 't-input-number', 't-icon', 't-loading', 't-empty']) {
      app.component(tag, { inheritAttrs: false, render: () => null })
    }
  }

  try {
    const pinia = createPinia()
    setActivePinia(pinia)

    const services = await vite.ssrLoadModule('/src/api/services.js')
    const { useBotStore } = await vite.ssrLoadModule('/src/stores/bot.js')
    const sfcMod = await vite.ssrLoadModule('/src/views/BotSettings.vue')
    const BotSettings = sfcMod.default
    const BotBasicFormMod = await vite.ssrLoadModule('/src/components/bot/BotBasicForm.vue')
    const BotBasicForm = BotBasicFormMod.default

    const store = useBotStore()
    store.bots = [
      {
        id: 'bot-legacy-temp',
        name: 'Legacy Bot',
        avatar: '🤖',
        description: 'old desc',
        llmMain: 'model-sol-main',
        llmLight: '',
        temperature: 0.35,
        maxTokens: 4096,
        workers: 4,
        reasoningEffort: '',
        hardMaxSteps: 0,
        running: false
      }
    ]

    const updatePayloads = []
    services.providerApi.list = async () => []
    services.configApi.get = async () => ({ value: '' })
    services.botApi.update = async (id, body) => {
      updatePayloads.push({ id, body: structuredClone(body) })
      return {
        id,
        ...body,
        temperature: 0.35,
        applied: true
      }
    }

    // Render BotBasicForm to verify guidance text in SSR HTML
    const formApp = createSSRApp({
      render: () => h(BotBasicForm, { form: store.bots[0] })
    })
    stubTdesignComponents(formApp)
    const formHtml = await renderToString(formApp)
    assert.ok(formHtml.includes('生成参数跟随模型，请在模型服务配置'))
    assert.ok(!formHtml.includes('data-testid="bot-temperature"'))

    // Mount BotSettings to capture exposed saveBasic and exercise a real save
    let exposed = null
    const origSettingsSetup = BotSettings.setup
    BotSettings.setup = (props, ctx) => origSettingsSetup(props, {
      ...ctx,
      expose(obj) {
        exposed = obj
        if (ctx.expose) ctx.expose(obj)
      }
    })
    try {
      const app = createSSRApp(BotSettings)
      app.use(pinia)
      stubTdesignComponents(app)
      await renderToString(app)
    } finally {
      BotSettings.setup = origSettingsSetup
    }

    assert.ok(exposed && typeof exposed.saveBasic === 'function', 'BotSettings should expose saveBasic for testability')
    exposed.form.value.name = 'Updated Legacy Bot'
    await exposed.saveBasic()

    assert.ok(updatePayloads.length >= 1, 'expected botApi.update to be called')
    for (const call of updatePayloads) {
      assert.equal(call.id, 'bot-legacy-temp')
      assert.equal(
        Object.prototype.hasOwnProperty.call(call.body, 'temperature'),
        false,
        `botApi.update payload must not contain temperature: ${JSON.stringify(call.body)}`
      )
    }
    assert.equal(store.bots[0].name, 'Updated Legacy Bot')
    assert.equal(store.bots[0].temperature, 0.35, 'legacy temperature in store must remain untouched')
  } finally {
    tdesignCjs.MessagePlugin.success = origSuccess
    tdesignCjs.MessagePlugin.warning = origWarning
    tdesignCjs.MessagePlugin.error = origError
    await vite.close()
  }
})
