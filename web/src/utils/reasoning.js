// 推理强度（reasoning_effort）选项与工具函数。
//
// 统一档位（超 / 高 / 中 / 低 / 极低 + 关闭），与后端 llm.CanonicalReasoningLevels
// 一一对应。配置里只出现这几档；各供应商的实际词表由后端 NormalizeReasoningEffort
// 按模型家族映射（如 GLM-5.3+ / DeepSeek V4 只接受 low/high/max，会把这几档
// 就近落到对应的三档上），不支持该参数的模型一律不发送。
//
// 取值语义：
//   ''        继承（主回复：模型默认，不发送参数；后台任务：继承全局设置/内置默认）
//   provider  模型默认：不发送参数，由模型/服务商自行决定

export const EFFORT_LEVELS = [
  { value: 'max', label: '超（max）' },
  { value: 'high', label: '高（high）' },
  { value: 'medium', label: '中（medium）' },
  { value: 'low', label: '低（low）' },
  { value: 'minimal', label: '极低（minimal）' },
  { value: 'none', label: '关闭（none）' }
]

export const REPLY_EFFORT_OPTIONS = [
  { label: '模型默认（不指定）', value: '' },
  ...EFFORT_LEVELS.map(l => ({ label: l.label, value: l.value }))
]

export const TASK_EFFORT_VALUES = ['', 'provider', ...EFFORT_LEVELS.map(l => l.value)]

export const REASONING_TASKS = [
  { key: 'memory_dedup', label: '记忆去重', tip: '长期记忆聚类合并。过低可能丢失细节（09-27 low 曾丢细节），默认 medium。' },
  { key: 'heartbeat', label: '心跳', tip: '心跳自主唤醒的决策轮。留空沿用主回复的推理强度。' },
  { key: 'dream', label: '梦境巩固', tip: '梦境各阶段（提取 / 主题聚类 / 评分）。' },
  { key: 'workflow_heal', label: '工作流自愈', tip: '工作流失败后的诊断与修复建议。' }
]

const VALUE_LABELS = {
  provider: '模型默认（不发送）',
  ...Object.fromEntries(EFFORT_LEVELS.map(l => [l.value, l.label]))
}

// taskEffortOptions 生成某个任务的下拉选项；首项「继承」标出实际生效的默认值。
export function taskEffortOptions(task, defaults = {}) {
  const d = defaults[task]
  let inherit = '继承默认'
  if (d === 'inherit_reply') inherit = '继承（沿用主回复）'
  else if (d) inherit = `继承默认（${d}）`
  return TASK_EFFORT_VALUES.map(v => ({ value: v, label: v === '' ? inherit : VALUE_LABELS[v] }))
}

// normalizeTasks 把后端返回补齐为全部任务键，未知值回落为 ''。
export function normalizeTasks(tasks = {}) {
  const out = {}
  for (const t of REASONING_TASKS) {
    const v = String(tasks?.[t.key] ?? '').trim().toLowerCase()
    out[t.key] = TASK_EFFORT_VALUES.includes(v) ? v : ''
  }
  return out
}
