export const MCP_SECRET_MASK = '********'

export function mcpRows(values = {}) {
  return Object.entries(values).map(([key, value]) => ({ key, value, originalKey: key, originalValue: value }))
}

// A mask is only a reference to an unchanged, existing field. It is not a credential.
export function mcpRowsToObject(rows) {
  const values = {}
  for (const row of rows) {
    const key = row.key.trim()
    if (!key) continue
    if (row.value === MCP_SECRET_MASK && (row.originalValue !== MCP_SECRET_MASK || key !== row.originalKey)) {
      throw new Error(`「${key}」的凭据已隐藏，改名或新增时请重新填写真实值`)
    }
    if (Object.hasOwn(values, key)) throw new Error(`配置键「${key}」重复，请合并后保存`)
    values[key] = row.value
  }
  return values
}

export function validateMcpImport(config) {
  if (!config?.mcpServers || typeof config.mcpServers !== 'object' || Array.isArray(config.mcpServers)) {
    throw new Error('缺少有效的 mcpServers 对象')
  }
  const entries = Object.values(config.mcpServers)
  if (!entries.length) throw new Error('没有可导入的 MCP 服务器')
  for (const server of entries) {
    if (!server || typeof server !== 'object' || Array.isArray(server)) throw new Error('服务器配置必须是对象')
    for (const map of [server.env, server.headers]) {
      if (map && Object.values(map).includes(MCP_SECRET_MASK)) {
        throw new Error('配置含已隐藏的凭据（********），无法用于导入。请重新填写真实值。')
      }
    }
  }
}
