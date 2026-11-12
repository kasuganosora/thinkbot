export function providerRuntimeStatus(saved) {
  if (!saved) return ''
  if (saved.runtimeApplied === false || saved.runtimeState === 'failed') {
    const detail = saved.runtimeMessage ? ` ${saved.runtimeMessage}` : ''
    return `期望配置已保存。正在使用的客户端仍是上一套。${detail}`.trim()
  }
  if (saved.runtimeApplied === true || saved.runtimeState === 'applied') {
    return '期望配置已保存，并且正在使用。'
  }
  return ''
}
