import assert from 'node:assert/strict'
import test from 'node:test'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import {
  mergeProviderList,
  applySavedProvider,
  resolveActiveProviderId,
  captureScrollState,
  restoreScrollState,
  withPreservedScroll
} from '../src/components/providerListState.js'

const srcDir = join(dirname(fileURLToPath(import.meta.url)), '..', 'src')
const providersSrc = readFileSync(join(srcDir, 'components/ProvidersManager.vue'), 'utf8')

test('mergeProviderList keeps current provider and model order while surfacing real updates', () => {
  const pA = {
    id: 'prov-a',
    name: 'Alpha',
    clientType: 'OpenAI Compatible',
    baseUrl: 'https://a.example/v1',
    apiKey: 'aaa...1111',
    enabled: true,
    models: [
      { id: 'm-1', name: 'M1', temperature: 0.7, topP: 0 },
      { id: 'm-2', name: 'M2', temperature: 0.3, topP: 0.9 }
    ]
  }
  const pB = {
    id: 'prov-b',
    name: 'Beta',
    clientType: 'OpenAI Compatible',
    baseUrl: 'https://b.example/v1',
    apiKey: 'bbb...2222',
    enabled: false,
    models: [{ id: 'm-b1', name: 'MB1', temperature: 0, topP: 0 }]
  }
  const pC = {
    id: 'prov-c',
    name: 'Gamma',
    clientType: 'Anthropic',
    baseUrl: 'https://c.example',
    apiKey: '',
    enabled: false,
    models: []
  }

  const current = [pA, pB, pC]
  // Server returns shuffled order + updated Beta + edited m-2 + new m-3 + brand-new prov-d
  const incoming = [
    {
      id: 'prov-c',
      name: 'Gamma',
      clientType: 'Anthropic',
      baseUrl: 'https://c.example',
      apiKey: '',
      enabled: false,
      models: []
    },
    {
      id: 'prov-b',
      name: 'Zeta Renamed',
      clientType: 'OpenAI Responses',
      baseUrl: 'https://b-new.example/v1',
      apiKey: 'new...9999',
      enabled: true,
      tokenParamStyle: 'max_completion_tokens',
      models: [{ id: 'm-b1', name: 'MB1 Updated', temperature: -1, topP: -1 }]
    },
    {
      id: 'prov-d',
      name: 'Delta Added',
      clientType: 'Gemini',
      baseUrl: '',
      apiKey: '',
      enabled: false,
      models: []
    },
    {
      id: 'prov-a',
      name: 'Alpha',
      clientType: 'OpenAI Compatible',
      baseUrl: 'https://a.example/v1',
      apiKey: 'aaa...1111',
      enabled: true,
      models: [
        { id: 'm-2', name: 'M2 Edited', temperature: -1, topP: 0.95 },
        { id: 'm-1', name: 'M1', temperature: 0.7, topP: 0 },
        { id: 'm-3', name: 'M3 New', temperature: 0.5, topP: 0 }
      ]
    }
  ]

  const merged = mergeProviderList(current, incoming)
  assert.deepEqual(merged.map(p => p.id), ['prov-a', 'prov-b', 'prov-c', 'prov-d'])

  // Object identity for existing entries is preserved so Vue does not tear down detail DOM
  assert.equal(merged[0], pA)
  assert.equal(merged[1], pB)
  assert.equal(merged[2], pC)

  // Real updates on prov-b are surfaced, never masked
  assert.equal(merged[1].name, 'Zeta Renamed')
  assert.equal(merged[1].clientType, 'OpenAI Responses')
  assert.equal(merged[1].baseUrl, 'https://b-new.example/v1')
  assert.equal(merged[1].apiKey, 'new...9999')
  assert.equal(merged[1].enabled, true)
  assert.equal(merged[1].tokenParamStyle, 'max_completion_tokens')
  assert.equal(merged[1].models[0].name, 'MB1 Updated')
  assert.equal(merged[1].models[0].temperature, -1)
  assert.equal(merged[1].models[0].topP, -1)

  // Model order inside prov-a stays stable for existing models and appends m-3
  assert.deepEqual(merged[0].models.map(m => m.id), ['m-1', 'm-2', 'm-3'])
  assert.equal(merged[0].models[1].name, 'M2 Edited')
  assert.equal(merged[0].models[1].temperature, -1)
})

test('applySavedProvider updates in place without dropping models or reordering', () => {
  const list = [
    { id: 'prov-1', name: 'One', baseUrl: 'https://1.example/v1', enabled: false, models: [{ id: 'm1', name: 'M1' }] },
    { id: 'prov-2', name: 'Two', baseUrl: 'https://2.example/v1', enabled: false, models: [{ id: 'm2', name: 'M2' }] }
  ]
  const out = applySavedProvider(list, {
    id: 'prov-2',
    name: 'Two Edited',
    baseUrl: 'https://2-edited.example/v1',
    enabled: true,
    apiKey: 'abc...defg',
    runtimeApplied: true
  })
  assert.deepEqual(out.map(p => p.id), ['prov-1', 'prov-2'])
  assert.equal(out[1].name, 'Two Edited')
  assert.equal(out[1].baseUrl, 'https://2-edited.example/v1')
  assert.equal(out[1].enabled, true)
  assert.equal(out[1].apiKey, 'abc...defg')
  assert.deepEqual(out[1].models, [{ id: 'm2', name: 'M2' }])
})

test('resolveActiveProviderId preserves selected provider and only falls back when missing', () => {
  const list = [{ id: 'prov-1' }, { id: 'prov-2' }, { id: 'prov-3' }]
  assert.equal(resolveActiveProviderId(list, 'prov-2'), 'prov-2')
  assert.equal(resolveActiveProviderId(list, 'prov-missing'), 'prov-1')
  assert.equal(resolveActiveProviderId(list, ''), 'prov-1')
  assert.equal(resolveActiveProviderId([], 'prov-2'), '')
})

test('withPreservedScroll restores list, detail, ancestor, and window scroll after async save + tick', async () => {
  const listEl = { scrollTop: 140, scrollLeft: 0, parentElement: null }
  const outerScrollEl = { scrollTop: 320, scrollLeft: 0, parentElement: null }
  const detailEl = { scrollTop: 480, scrollLeft: 0, parentElement: outerScrollEl }
  let winX = 0
  let winY = 260
  const fakeWin = {
    get scrollX() { return winX },
    get scrollY() { return winY },
    scrollTo(x, y) { winX = x; winY = y }
  }

  let ticked = false
  const fakeNextTick = async () => {
    ticked = true
  }

  const snap = captureScrollState([listEl, detailEl], fakeWin)
  listEl.scrollTop = 0
  detailEl.scrollTop = 0
  outerScrollEl.scrollTop = 0
  winY = 0
  restoreScrollState(snap)
  assert.equal(listEl.scrollTop, 140)
  assert.equal(detailEl.scrollTop, 480)
  assert.equal(outerScrollEl.scrollTop, 320)
  assert.equal(winY, 260)

  await withPreservedScroll(
    [listEl, detailEl],
    async () => {
      // Simulate DOM refresh resetting scroll positions during save reload
      listEl.scrollTop = 0
      detailEl.scrollTop = 0
      outerScrollEl.scrollTop = 0
      winY = 0
    },
    { nextTickFn: fakeNextTick, win: fakeWin }
  )
  assert.equal(ticked, true)
  assert.equal(listEl.scrollTop, 140)
  assert.equal(detailEl.scrollTop, 480)
  assert.equal(outerScrollEl.scrollTop, 320)
  assert.equal(winY, 260)
})

test('ProvidersManager wires scroll/selection/order preservation across provider and model saves', () => {
  assert.match(providersSrc, /ref="provListEl"/)
  assert.match(providersSrc, /ref="provDetailEl"/)
  assert.match(providersSrc, /mergeProviderList/)
  assert.match(providersSrc, /resolveActiveProviderId/)
  assert.match(providersSrc, /withPreservedScroll/)
})
