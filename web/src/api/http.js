// ============================================================================
// HTTP 层 — 统一响应封装 + mock/真实切换开关
//
// 设计目标：前端组件只依赖本目录下的 api 函数，不直接碰 mock 还是真实请求。
// 后端对齐 mock 后，只需把 USE_MOCK 置为 false 并实现 request()，组件零改动。
//
// 统一响应结构（与 thinkbot 后端 api/response.go 一致）：
//   成功：{ code: 0, message: 'ok', data: ... }
//   失败：{ code: <非零>, message: '错误描述', data: null }
// ============================================================================

export const USE_MOCK = true

// 模拟网络延迟（毫秒），让 loading 状态可见
const MOCK_LATENCY = 280

/**
 * 包装成功响应。mock 与真实后端返回结构一致。
 * @param {*} data
 * @returns {{code:number, message:string, data:*}}
 */
export function ok(data) {
  return { code: 0, message: 'ok', data }
}

/**
 * 包装失败响应。
 * @param {number} code
 * @param {string} message
 */
export function fail(code, message) {
  return { code, message, data: null }
}

/**
 * mock 模式下的延迟返回。返回的是 data 部分（已解包），
 * 若后端返回非零 code 则抛出错误，调用方用 try/catch 处理。
 * @template T
 * @param {() => T} producer 生成 data 的函数
 * @returns {Promise<T>}
 */
export function mockResolve(producer) {
  return new Promise((resolve, reject) => {
    setTimeout(() => {
      try {
        const data = producer()
        resolve(data)
      } catch (e) {
        reject(e)
      }
    }, MOCK_LATENCY)
  })
}

/**
 * 真实请求占位（后端就绪后实现）。当前 USE_MOCK=true 时不会被调用。
 * 约定：解包统一响应，code!==0 时 throw，成功时 resolve data。
 * @param {string} method
 * @param {string} url
 * @param {object} [body]
 */
export async function request(method, url, body) {
  const resp = await fetch(url, {
    method,
    headers: { 'Content-Type': 'application/json' },
    credentials: 'include',
    body: body ? JSON.stringify(body) : undefined
  })
  const json = await resp.json()
  if (json.code !== 0) {
    const err = new Error(json.message || '请求失败')
    err.code = json.code
    throw err
  }
  return json.data
}
