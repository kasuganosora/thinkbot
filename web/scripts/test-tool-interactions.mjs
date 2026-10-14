import fs from 'node:fs'
import assert from 'node:assert/strict'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { parse, compileScript } from '@vue/compiler-sfc'
import { ref, computed, watch, reactive, nextTick, effectScope } from 'vue'

// Exercise the actual SFC setup functions with real Vue reactivity. Only network,
// lifecycle/DOM animation, and timers are replaced, so no browser dependency is needed.
function component(name, props, extras = {}) {
  const filename = fileURLToPath(new URL(`../src/components/${name}.vue`, import.meta.url))
  const { descriptor } = parse(fs.readFileSync(filename, 'utf8'))
  const source = compileScript(descriptor, { id: `regression-${name}` }).content
    .replace(/^import .+ from .+\n/gm, '').replace('export default', 'const component =')
  const dependencies = { ref, computed, watch, nextTick, onMounted() {}, onBeforeUnmount() {}, ToolCallCard: {}, toolLabels: {}, ...extras }
  const definition = new Function(...Object.keys(dependencies), `${source}\nreturn component`)(...Object.values(dependencies))
  const scope = effectScope()
  const state = scope.run(() => definition.setup(props, { expose() {} }))
  return { state, stop: () => scope.stop() }
}

for (const [status, expected, label] of [['timeout', 'timeout', '超时'], ['cancelled', 'cancelled', '已停止'], ['killed', 'cancelled', '已停止'], ['superseded', 'superseded', '已取代']]) {
  test(`${status} tool results are not summarized as successful`, () => {
    const c = component('ToolCallGroup', reactive({ name: 'exec', calls: [{ id: 1, status }] }))
    try {
      assert.equal(c.state.aggState.value, expected)
      assert(c.state.aggMeta.value.includes(label))
      assert(!c.state.aggMeta.value.includes('完成'))
    } finally { c.stop() }
  })
}

test('running group keeps failure counts visible and respects a manual collapse', async () => {
  const props = reactive({ name: 'exec', calls: [{ id: 1, status: 'success' }, { id: 2, status: 'error' }, { id: 3, status: 'running' }] })
  const c = component('ToolCallGroup', props)
  try {
    assert.equal(c.state.aggState.value, 'running')
    assert(c.state.aggMeta.value.includes('1 失败'))
    assert(c.state.expanded.value)
    c.state.toggle()
    props.calls[2].status = 'success'
    await nextTick()
    assert.equal(c.state.expanded.value, false)
  } finally { c.stop() }
})

function choice(overrides = {}) {
  const props = reactive({ payload: { questionId: 'uc-test', question: 'choose', options: [{ id: 'a', label: 'A' }] }, status: 'running', submitted: false, ...overrides.props })
  const resolved = ref('')
  const answered = [], terminals = [], requests = [], unmount = []
  let fails = overrides.fails ?? 0
  const extras = {
    MessagePlugin: { success() {}, error() {}, warning() {}, info() {} },
    useBotStore: () => ({ choiceState: () => null, choiceIdByToolCallId: () => resolved.value, markChoiceSubmitted: (...args) => answered.push(args), markChoiceTerminal: (...args) => terminals.push(args) }),
    userChoiceApi: { answer: async (...args) => { requests.push(args); if (fails-- > 0) throw Object.assign(new Error('network'), { code: overrides.code ?? 500 }) } },
    animateSpring() {}, prefersReducedMotion: () => true,
    onBeforeUnmount: fn => unmount.push(fn),
    setTimeout: fn => { overrides.onWait?.(props, unmount); fn() },
    window: { removeEventListener() {} },
  }
  const instance = component('ChoiceCard', props, extras)
  instance.state.selectedIds.value = ['a']
  return { ...instance, props, resolved, answered, terminals, requests }
}
const placeholder = { payload: { questionId: 'call_tmp', question: 'choose', options: [{ id: 'a', label: 'A' }] } }

test('question synchronization timeout releases submitting and preserves an answer for retry', async () => {
  const c = choice({ props: structuredClone(placeholder) })
  try {
    await c.state.submit()
    assert.equal(c.state.submitting.value, false)
    assert.equal(c.state.phase.value, 'active')
    assert.equal(c.requests.length, 0)
    assert(c.state.submitError.value)
    assert.deepEqual(c.state.selectedIds.value, ['a'])
    c.resolved.value = 'uc-real'
    await c.state.submit()
    assert.equal(c.requests.length, 1)
    assert.equal(c.requests[0][0], 'uc-real')
    assert.equal(c.state.phase.value, 'answered')
    assert.equal(c.state.submitting.value, false)
  } finally { c.stop() }
})

test('network failure permits an explicit retry of the preserved selection', async () => {
  const c = choice({ fails: 1 })
  try {
    await c.state.submit()
    assert.equal(c.state.phase.value, 'active')
    assert.equal(c.state.submitting.value, false)
    assert(c.state.submitError.value)
    await c.state.submit()
    assert.equal(c.requests.length, 2)
    assert.deepEqual(c.requests[1][1].selectedIds, ['a'])
    assert.equal(c.state.phase.value, 'answered')
  } finally { c.stop() }
})

test('conflict response does not falsely record this answer as accepted', async () => {
  const c = choice({ fails: 1, code: 409 })
  try {
    await c.state.submit()
    assert.equal(c.state.phase.value, 'ended')
    assert.equal(c.answered.length, 0)
    assert.equal(c.state.submitting.value, false)
  } finally { c.stop() }
})

test('server timeout while waiting for a question prevents late submission', async () => {
  const c = choice({ props: structuredClone(placeholder), onWait: props => { props.status = 'timeout' } })
  try {
    await c.state.submit()
    assert.equal(c.state.phase.value, 'timeout')
    assert.equal(c.requests.length, 0)
    assert.equal(c.state.submitting.value, false)
  } finally { c.stop() }
})

test('unmounting while waiting does not send an answer after leaving the conversation', async () => {
  const c = choice({ props: structuredClone(placeholder), onWait: (_props, unmount) => unmount.forEach(fn => fn()) })
  try {
    await c.state.submit()
    assert.equal(c.requests.length, 0)
    assert.equal(c.state.submitting.value, false)
  } finally { c.stop() }
})
