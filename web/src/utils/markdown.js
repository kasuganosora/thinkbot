// ============================================================================
// Markdown 渲染 + XSS 净化（LLM 输出的唯一渲染入口）
//
// 为什么集中到一处：
//  1. LLM 输出经 v-html 落到 DOM，净化策略必须统一，不能各组件各写一遍；
//  2. DOMPurify.addHook 是**全局**的，分散注册会重复挂钩、彼此覆盖；
//  3. 渲染结果需要缓存——聊天流式期间模板每帧都会重新调用渲染函数，
//     不缓存就会对全部历史消息反复做 parse + sanitize。
// ============================================================================

import { marked } from 'marked'
import DOMPurify from 'dompurify'

marked.setOptions({ breaks: true, gfm: true })

// 允许的链接协议白名单：只放行 http/https/mailto/tel 与相对路径。
// 显式收紧的原因是 javascript:、vbscript:、data:text/html 这类伪协议一旦
// 出现在模型输出的 [text](...) 里，点击即执行脚本。
const ALLOWED_URI_REGEXP = /^(?:(?:https?|mailto|tel):|[^a-z]|[a-z+.-]+(?:[^a-z+.\-:]|$))/i

const SANITIZE_CONFIG = {
  ALLOWED_URI_REGEXP,
  // 需要显式放行，否则下面 hook 写入的 target/rel 会被再次剥掉
  ADD_ATTR: ['target', 'rel'],
  // 表单/内联样式对渲染 markdown 没有意义，但都是常见的注入面
  FORBID_TAGS: ['style', 'form', 'input', 'button', 'iframe', 'object', 'embed'],
  FORBID_ATTR: ['style', 'srcset', 'formaction', 'ping'],
}

let _hookInstalled = false
function installHooks() {
  if (_hookInstalled) return
  _hookInstalled = true
  DOMPurify.addHook('afterSanitizeAttributes', (node) => {
    if (node.tagName !== 'A') return
    // 外链一律新窗口打开，并断开 window.opener 引用（防反向操控本页）
    node.setAttribute('target', '_blank')
    node.setAttribute('rel', 'noopener noreferrer nofollow')
  })
}

// 渲染缓存：key = 原文，value = 净化后的 HTML。
// 流式期间原文每帧都在变，命中率主要来自「已完成的历史消息」，
// 因此容量给小一点、满了直接整体清空（简单且不会无界增长）。
const MAX_CACHE = 80
const _cache = new Map()

/**
 * 把 markdown 文本渲染为可安全交给 v-html 的 HTML。
 * @param {string} text
 * @returns {string}
 */
export function renderMarkdown(text) {
  if (!text) return ''
  const hit = _cache.get(text)
  if (hit !== undefined) return hit
  installHooks()
  const html = DOMPurify.sanitize(marked.parse(text), SANITIZE_CONFIG)
  if (_cache.size >= MAX_CACHE) _cache.clear()
  _cache.set(text, html)
  return html
}

/** 清空渲染缓存（切换会话/工作流时调用，避免缓存长期驻留） */
export function clearMarkdownCache() {
  _cache.clear()
}
