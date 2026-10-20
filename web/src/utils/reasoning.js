// 推理强度（reasoning_effort）选项与工具函数。
//
// 取值语义（与后端 config.ValidBotReasoning 一致）：
//   ''        继承（主回复：模型默认，不发送参数；后台任务：继承全局设置/内置默认）
//   provider  模型默认：不发送参数，由模型/服务商自行决定
//   none      关闭推理
//   low / medium / high  指定强度
// 后端会按模型词表映射（如 GLM-5.3 只接受 low/high/max：none→low、medium→high），
// 不支持该参数的模型一律不发送。

export const REPLY_EFFORT_OPTIONS = [
  { label: '模型默认（不指定）', value: '' },
  { label: '关闭（none）', value: 'none' },
  { label: 'minimal', value: 'minimal' },
  { label: 'low', value: 'low' },
  { label: 'medium', value: 'medium' },
  { label: 'high', value: 'high' }
]

export const TASK_EFFORT_VALUES = ['', 'provider', 'none', 'low', 'medium', 'high']

export const REASONING_TASKS = [
  { key: 'memory_dedup', label: '记忆去重', tip: '长期记忆聚类合并。过低可能丢失细节（09-27 low 曾丢细节），默认 medium。' },
  { key: 'heartbeat', label: '心跳', tip: '心跳自主唤醒的决策轮。留空沿用主回复的推理强度。' },
  { key: 'dream', label: '梦境巩固', tip: '梦境各阶段（提取 / 主题聚类 / 评分）。' },
  { key: 'workflow_heal', label: '工作流自愈', tip: '工作流失败后的诊断与修复建议。' }
]

const VALUE_LABELS = { provider: '模型默认（不发送）', none: '关闭（none）', low: 'low', medium: 'medium', high: 'high' }

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
