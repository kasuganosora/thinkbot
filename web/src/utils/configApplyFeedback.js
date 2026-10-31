// Saving a setting and applying it to a running container are separate outcomes.
export function configApplyFeedback(result, label = '配置') {
  const data = result?.data ?? result
  if (data?.failed === true || data?.failed?.length) {
    return { theme: 'warning', message: `${label}已保存，但部分容器应用失败${data.needsRecreate ? '，另有容器需要重建' : ''}。请检查容器状态后重试或重建。` }
  }
  if (data?.needsRecreate) {
    return { theme: 'warning', message: `${label}已保存；需重建工作容器后才生效` }
  }
  if (data?.applied === false || data?.failed?.length || data?.failed === true) {
    return { theme: 'warning', message: `${label}已保存，但未能全部应用到运行中的容器。请检查容器状态后重试或重建。` }
  }
  if (data?.applied === true) return { theme: 'success', message: `${label}已保存并生效` }
  return { theme: 'success', message: `${label}已保存` }
}
