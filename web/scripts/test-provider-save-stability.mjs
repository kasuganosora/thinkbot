import assert from 'node:assert/strict'
import test from 'node:test'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { createRequire } from 'node:module'
import { createServer } from 'vite'
import { createSSRApp, h } from 'vue'
import { renderToString } from '@vue/server-renderer'
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

function deferred() {
  let resolve
  let reject
  const promise = new Promise((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

test('ProvidersManager.load generation guard drops out-of-order stale responses across create, delete, and save', async (t) => {
  const virtualTdesignId = '\0virtual:tdesign-vue-next'
  const server = await createServer({
    server: { middlewareMode: true },
    appType: 'custom',
    plugins: [{
      name: 'stub-tdesign-for-ssr',
      enforce: 'pre',
      resolveId(id) {
        if (id === 'tdesign-vue-next') return virtualTdesignId
      },
      load(id) {
        if (id === virtualTdesignId) {
          return 'export const MessagePlugin = { success() {}, warning() {}, error() {} }; export const DialogPlugin = { confirm() { return { destroy() {} } } };'
        }
      }
    }]
  })
  try {
    const req = createRequire(import.meta.url)
    const tdesign = req('tdesign-vue-next')
    const { providerApi } = await server.ssrLoadModule('/src/api/services.js')
    const { default: ProvidersManager } = await server.ssrLoadModule('/src/components/ProvidersManager.vue')

    const origMsg = {
      success: tdesign.MessagePlugin.success,
      warning: tdesign.MessagePlugin.warning,
      error: tdesign.MessagePlugin.error
    }
    const origConfirm = tdesign.DialogPlugin.confirm
    const origApi = { ...providerApi }

    let lastConfirm = null
    tdesign.MessagePlugin.success = () => {}
    tdesign.MessagePlugin.warning = () => {}
    tdesign.MessagePlugin.error = () => {}
    tdesign.DialogPlugin.confirm = (opts) => {
      const handle = { destroy() {} }
      lastConfirm = () => opts.onConfirm()
      return handle
    }

    async function mountExposed() {
      let exposed = null
      const origSetup = ProvidersManager.setup
      ProvidersManager.setup = (props, ctx) => origSetup(props, {
        ...ctx,
        expose: (obj) => {
          exposed = obj
          if (ctx.expose) ctx.expose(obj)
        }
      })
      try {
        const app = createSSRApp(ProvidersManager)
        for (const tag of ['t-icon', 't-switch', 't-input', 't-select', 't-button', 't-dialog', 't-form', 't-form-item', 't-checkbox-group', 't-checkbox', 't-input-number', 't-divider', 't-link']) {
          app.component(tag, { render: () => null })
        }
        await renderToString(app)
      } finally {
        ProvidersManager.setup = origSetup
      }
      assert.ok(exposed && typeof exposed.load === 'function', 'ProvidersManager must expose load/state for async race testing')
      return {
        get providers() { return exposed.providers.value },
        get activeId() { return exposed.activeId.value },
        set activeId(v) { exposed.activeId.value = v },
        load: exposed.load,
        addProvider: exposed.addProvider,
        saveProvider: exposed.saveProvider,
        removeProvider: exposed.removeProvider
      }
    }

    try {
      await t.test('out-of-order create: stale list resolving after create must not remove new provider or jump selection', async () => {
        const listQueue = []
        providerApi.list = () => {
          const d = deferred()
          listQueue.push(d)
          return d.promise
        }
        providerApi.create = async () => ({
          id: 'prov-new',
          name: '新服务商',
          clientType: 'OpenAI Compatible',
          baseUrl: '',
          apiKey: '',
          enabled: false,
          models: []
        })

        const vm = await mountExposed()
        // Initial load seeds prov-a
        const initLoad = vm.load()
        listQueue[0].resolve([{ id: 'prov-a', name: 'Alpha', clientType: 'OpenAI Compatible', baseUrl: '', apiKey: '', enabled: true, models: [] }])
        await initLoad
        assert.deepEqual(vm.providers.map(p => p.id), ['prov-a'])
        assert.equal(vm.activeId, 'prov-a')

        // Overlapping slow background load #1 starts BEFORE create
        const staleLoad = vm.load('prov-a')
        assert.equal(listQueue.length, 2)

        // User creates prov-new; addProvider triggers load('prov-new') (#2) which resolves FIRST
        const createPromise = vm.addProvider()
        // Wait a microtask for create() -> load('prov-new') to enqueue listQueue[2]
        await Promise.resolve()
        await Promise.resolve()
        assert.equal(listQueue.length, 3)

        listQueue[2].resolve([
          { id: 'prov-a', name: 'Alpha', clientType: 'OpenAI Compatible', baseUrl: '', apiKey: '', enabled: true, models: [] },
          { id: 'prov-new', name: '新服务商', clientType: 'OpenAI Compatible', baseUrl: '', apiKey: '', enabled: false, models: [] }
        ])
        await createPromise
        assert.deepEqual(vm.providers.map(p => p.id), ['prov-a', 'prov-new'])
        assert.equal(vm.activeId, 'prov-new')

        // Now stale load #1 resolves late WITHOUT prov-new
        listQueue[1].resolve([
          { id: 'prov-a', name: 'Alpha', clientType: 'OpenAI Compatible', baseUrl: '', apiKey: '', enabled: true, models: [] }
        ])
        await staleLoad

        assert.deepEqual(vm.providers.map(p => p.id), ['prov-a', 'prov-new'], 'stale list removed newly created provider')
        assert.equal(vm.activeId, 'prov-new', 'stale list jumped selection away from created provider')
      })

      await t.test('out-of-order delete: stale list resolving after delete must not resurrect deleted provider', async () => {
        const listQueue = []
        providerApi.list = () => {
          const d = deferred()
          listQueue.push(d)
          return d.promise
        }
        providerApi.remove = async () => null

        const vm = await mountExposed()
        const initLoad = vm.load()
        listQueue[0].resolve([
          { id: 'prov-a', name: 'Alpha', clientType: 'OpenAI Compatible', baseUrl: '', apiKey: '', enabled: true, models: [] },
          { id: 'prov-b', name: 'Beta', clientType: 'OpenAI Compatible', baseUrl: '', apiKey: '', enabled: false, models: [] }
        ])
        await initLoad
        vm.activeId = 'prov-b'

        // Stale load #1 starts while prov-b still existed
        const staleLoad = vm.load('prov-b')
        assert.equal(listQueue.length, 2)

        // User deletes prov-b; confirm triggers remove('prov-b') + load('') (#2) which resolves FIRST
        vm.removeProvider()
        const deletePromise = lastConfirm()
        await Promise.resolve()
        await Promise.resolve()
        assert.equal(listQueue.length, 3)

        listQueue[2].resolve([
          { id: 'prov-a', name: 'Alpha', clientType: 'OpenAI Compatible', baseUrl: '', apiKey: '', enabled: true, models: [] }
        ])
        await deletePromise
        assert.deepEqual(vm.providers.map(p => p.id), ['prov-a'])
        assert.equal(vm.activeId, 'prov-a')

        // Now stale load #1 resolves late WITH deleted prov-b
        listQueue[1].resolve([
          { id: 'prov-a', name: 'Alpha', clientType: 'OpenAI Compatible', baseUrl: '', apiKey: '', enabled: true, models: [] },
          { id: 'prov-b', name: 'Beta', clientType: 'OpenAI Compatible', baseUrl: '', apiKey: '', enabled: false, models: [] }
        ])
        await staleLoad

        assert.deepEqual(vm.providers.map(p => p.id), ['prov-a'], 'stale list resurrected deleted provider')
        assert.equal(vm.activeId, 'prov-a', 'stale list jumped selection to deleted provider')
      })

      await t.test('out-of-order save: stale list resolving after save must not overwrite saved fields or jump selection', async () => {
        const listQueue = []
        providerApi.list = () => {
          const d = deferred()
          listQueue.push(d)
          return d.promise
        }
        providerApi.update = async (id, payload) => ({
          id,
          ...payload,
          apiKey: 'masked',
          runtimeApplied: true,
          runtimeState: 'applied'
        })

        const vm = await mountExposed()
        const initLoad = vm.load()
        listQueue[0].resolve([
          { id: 'prov-a', name: 'Alpha', clientType: 'OpenAI Compatible', baseUrl: 'https://a.example/v1', apiKey: 'masked', enabled: true, models: [] },
          { id: 'prov-b', name: 'Beta Old', clientType: 'OpenAI Compatible', baseUrl: 'https://b.example/v1', apiKey: 'masked', enabled: false, models: [] }
        ])
        await initLoad

        // Stale load #1 starts while activeId is prov-a and prov-b still has 'Beta Old'
        const staleLoad = vm.load('prov-a')
        assert.equal(listQueue.length, 2)

        // User selects prov-b, renames it to 'Beta Saved', and saves; load #2 resolves FIRST
        vm.activeId = 'prov-b'
        vm.providers[1].name = 'Beta Saved'
        const savePromise = vm.saveProvider()
        await Promise.resolve()
        await Promise.resolve()
        assert.equal(listQueue.length, 3)

        listQueue[2].resolve([
          { id: 'prov-a', name: 'Alpha', clientType: 'OpenAI Compatible', baseUrl: 'https://a.example/v1', apiKey: 'masked', enabled: true, models: [] },
          { id: 'prov-b', name: 'Beta Saved', clientType: 'OpenAI Compatible', baseUrl: 'https://b.example/v1', apiKey: 'masked', enabled: false, models: [] }
        ])
        await savePromise
        assert.equal(vm.activeId, 'prov-b')
        assert.equal(vm.providers[1].name, 'Beta Saved')

        // Stale load #1 resolves late with old 'Beta Old' and keepId='prov-a'
        listQueue[1].resolve([
          { id: 'prov-a', name: 'Alpha', clientType: 'OpenAI Compatible', baseUrl: 'https://a.example/v1', apiKey: 'masked', enabled: true, models: [] },
          { id: 'prov-b', name: 'Beta Old', clientType: 'OpenAI Compatible', baseUrl: 'https://b.example/v1', apiKey: 'masked', enabled: false, models: [] }
        ])
        await staleLoad

        assert.equal(vm.activeId, 'prov-b', 'stale list jumped selection back to prov-a')
        assert.equal(vm.providers[1].name, 'Beta Saved', 'stale list overwrote newly saved provider name')
      })
    } finally {
      Object.assign(providerApi, origApi)
      tdesign.MessagePlugin.success = origMsg.success
      tdesign.MessagePlugin.warning = origMsg.warning
      tdesign.MessagePlugin.error = origMsg.error
      tdesign.DialogPlugin.confirm = origConfirm
    }
  } finally {
    await server.close()
  }
})
