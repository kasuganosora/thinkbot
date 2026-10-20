import assert from 'node:assert/strict'
import test from 'node:test'
import { REPLY_EFFORT_OPTIONS, REASONING_TASKS, TASK_EFFORT_VALUES, taskEffortOptions, normalizeTasks } from '../src/utils/reasoning.js'

test('reply options include model default and off', () => {
  const values = REPLY_EFFORT_OPTIONS.map(o => o.value)
  for (const v of ['', 'none', 'low', 'medium', 'high']) assert.ok(values.includes(v), v)
})

test('tasks cover reply-independent background jobs', () => {
  assert.deepEqual(REASONING_TASKS.map(t => t.key), ['memory_dedup', 'heartbeat', 'dream', 'workflow_heal'])
})

test('task options mark the effective default', () => {
  const opts = taskEffortOptions('memory_dedup', { memory_dedup: 'medium' })
  assert.equal(opts[0].value, '')
  assert.match(opts[0].label, /medium/)
  assert.deepEqual(opts.map(o => o.value), TASK_EFFORT_VALUES)
  assert.match(taskEffortOptions('heartbeat', { heartbeat: 'inherit_reply' })[0].label, /主回复/)
})

test('normalizeTasks fills every task and drops unknown values', () => {
  assert.deepEqual(normalizeTasks({ memory_dedup: 'LOW', dream: 'ultra' }),
    { memory_dedup: 'low', heartbeat: '', dream: '', workflow_heal: '' })
})
