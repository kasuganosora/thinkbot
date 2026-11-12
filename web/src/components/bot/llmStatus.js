export function llmStatusText({ desiredMain, desiredLight, desiredVision, effectiveMain, effectiveLight, effectiveVision, error }) {
  const parts = [
    `期望主模型 ${desiredMain || '未设置'}，正在使用 ${effectiveMain || '未生效'}`,
    `期望轻量模型 ${desiredLight || '未设置'}，正在使用 ${effectiveLight || '未生效'}`,
    `期望视觉模型 ${desiredVision || '未启用'}，正在使用 ${effectiveVision || '未启用'}`
  ]
  if (error) parts.push(error)
  return parts.join('。')
}
