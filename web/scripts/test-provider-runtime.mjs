import assert from 'node:assert/strict'
import { createSSRApp, h } from 'vue'
import { renderToString } from '@vue/server-renderer'
import { providerRuntimeStatus } from '../src/components/providerRuntimeStatus.js'

const failed = providerRuntimeStatus({
  runtimeApplied: false,
  runtimeState: 'failed',
  runtimeMessage: 'model is not configured'
})
assert.match(failed, /期望配置已保存/)
assert.match(failed, /仍是上一套/)
assert.match(failed, /not configured/)
assert.doesNotMatch(failed, /正在使用。$/)

const applied = providerRuntimeStatus({ runtimeApplied: true, runtimeState: 'applied' })
assert.match(applied, /正在使用/)

const html = await renderToString(createSSRApp({
  render: () => h('div', { 'data-testid': 'provider-runtime-status' }, failed)
}))
assert.match(html, /data-testid="provider-runtime-status"/)
assert.match(html, /仍是上一套/)
console.log('provider runtime status render ok')
