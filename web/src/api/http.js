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

import { useUserStore } from '@/stores/user'

export const USE_MOCK = false

// 会话失效（401）统一处理：清本地登录态并跳回登录页。
// 这样即使 localStorage 残留登录信息、但服务端 cookie 已失效，
// 接口报 401 时也能自动回到登录页，而不是卡在受保护页面。
function handleUnauthorized() {
  if (window.location.hash === '#/login') return
  try {
    useUserStore().logout()
  } catch (e) {
    localStorage.removeItem('bp_user')
  }
  window.location.hash = '#/login'
}

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
 * 把 query 对象拼接到 URL 上（复用 URLSearchParams）。
 * - 兼容 url 上已存在的 querystring（用 & 追加）。
 * - 过滤 undefined / null / '' 等空值，不参与拼接。
 * @param {string} url
 * @param {object} [query]
 * @returns {string}
 */
function withQuery(url, query) {
  if (!query || typeof query !== 'object') return url
  const params = new URLSearchParams()
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === null || v === '') continue
    params.append(k, String(v))
  }
  const qs = params.toString()
  if (!qs) return url
  return url + (url.includes('?') ? '&' : '?') + qs
}

/**
 * 真实请求（JSON）。约定：解包统一响应，code!==0 时 throw，成功时 resolve data。
 * @param {string} method
 * @param {string} url
 * @param {object} [body]  JSON 请求体
 * @param {object} [query] 可选的 query 参数对象，会拼接到 URL 的 querystring 上
 */
export async function request(method, url, body, query) {
  const finalUrl = withQuery(url, query)
  const resp = await fetch(finalUrl, {
    method,
    headers: { 'Content-Type': 'application/json' },
    credentials: 'include',
    body: body ? JSON.stringify(body) : undefined
  })
  if (resp.status === 401) {
    handleUnauthorized()
    const json = await resp.json().catch(() => ({}))
    const err = new Error(json.message || '登录已失效，请重新登录')
    err.code = 401
    throw err
  }
  const json = await resp.json()
  if (json.code !== 0) {
    const err = new Error(json.message || '请求失败')
    err.code = json.code
    throw err
  }
  return json.data
}

/**
 * multipart/form-data 请求（文件上传专用）。
 * 与 request 一样解包统一响应 {code,message,data} 并携带 cookie；
 * 但**不手动设置 Content-Type**——交给浏览器根据 FormData 自动带上 boundary。
 * @param {string} method
 * @param {string} url
 * @param {FormData} formData
 * @param {object} [query] 可选 query 参数
 */
export async function uploadRequest(method, url, formData, query) {
  const finalUrl = withQuery(url, query)
  const resp = await fetch(finalUrl, {
    method,
    credentials: 'include',
    body: formData
  })
  if (resp.status === 401) {
    handleUnauthorized()
    const json = await resp.json().catch(() => ({}))
    const err = new Error(json.message || '登录已失效，请重新登录')
    err.code = 401
    throw err
  }
  const json = await resp.json()
  if (json.code !== 0) {
    const err = new Error(json.message || '上传失败')
    err.code = json.code
    throw err
  }
  return json.data
}
