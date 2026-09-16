#!/usr/bin/env node
// ============================================================================
// 静态检查：SFC 的 <template> 里引用的标识符，必须在 <script setup> 中有定义。
//
// 为什么需要它：
//   `vite build` 不做「模板 ↔ 脚本」一致性检查。当模板引用了脚本里不存在的
//   绑定（例如模板写了 v-model="cleanTiers" / :disabled="cleanTiers.length === 0"
//   但脚本忘了声明 cleanTiers），构建完全静默通过、产物里也有这段代码，但运行
//   时会抛 TypeError 并中断该组件的渲染 —— 表现为「页面少了一块、按钮没出现、
//   点了没反应」，且控制台之外没有明显信号。踩过一次（记忆清理卡片）。
//
// 手法：
//   1. compileScript 拿到 script setup 的 bindingMetadata（所有顶层绑定的名单）；
//   2. 用这份 metadata 编译模板。此时「脚本里找不到」的标识符会被编译成
//      `_ctx.<name>`，逐个收集即可；模板局部变量（v-for/v-slot 别名、内联箭头
//      函数参数）和 Vue 全局白名单（Math/Date/JSON 等）由编译器处理，不会误报。
//
// 用法：node scripts/check-template-bindings.mjs [srcDir]   （默认 src）
// 退出码：有未定义绑定 → 1，否则 0。
// ============================================================================
import fs from 'node:fs'
import path from 'node:path'
import { parse, compileScript, compileTemplate } from '@vue/compiler-sfc'

const root = path.resolve(process.argv[2] || 'src')

function walk(dir) {
  const out = []
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, entry.name)
    if (entry.isDirectory()) out.push(...walk(p))
    else if (entry.name.endsWith('.vue')) out.push(p)
  }
  return out
}

if (!fs.existsSync(root)) {
  console.error(`[check-bindings] 目录不存在: ${root}`)
  process.exit(2)
}

const files = walk(root)
const violations = []
let checked = 0

for (const file of files) {
  const source = fs.readFileSync(file, 'utf8')
  const { descriptor, errors } = parse(source, { filename: file })
  if (errors.length) {
    violations.push({ file, names: [`<SFC 解析失败: ${errors[0].message}>`] })
    continue
  }
  // 无脚本的组件（纯静态模板）没有绑定可言，跳过。
  if (!descriptor.scriptSetup && !descriptor.script) continue
  // 非 script setup 的选项式组件通过 this 暴露绑定，本检查不适用，跳过。
  if (!descriptor.scriptSetup) continue
  if (!descriptor.template) continue

  let bindings = {}
  try {
    bindings = compileScript(descriptor, { id: 'check-bindings' }).bindings || {}
  } catch (e) {
    violations.push({ file, names: [`<script setup 编译失败: ${e.message}>`] })
    continue
  }

  const res = compileTemplate({
    source: descriptor.template.content,
    filename: file,
    id: 'check-bindings',
    compilerOptions: { bindingMetadata: bindings, prefixIdentifiers: true },
  })
  if (res.errors && res.errors.length) {
    violations.push({ file, names: res.errors.map((e) => `<模板编译失败: ${e.message || e}>`) })
    continue
  }

  // 编译后仍带 _ctx. 前缀的，就是脚本里没定义的标识符。
  // $ 开头的（$slots/$attrs/$emit 等）是组件实例自带属性，不算问题。
  const names = new Set()
  for (const m of res.code.matchAll(/_ctx\.([A-Za-z_$][\w$]*)/g)) {
    if (!m[1].startsWith('$')) names.add(m[1])
  }
  checked++
  if (names.size) violations.push({ file, names: [...names].sort() })
}

if (violations.length) {
  console.error(`[check-bindings] 发现 ${violations.length} 个文件的模板引用了未定义绑定：\n`)
  for (const v of violations) {
    console.error(`  ${path.relative(process.cwd(), v.file)}`)
    console.error(`    未定义: ${v.names.join(', ')}`)
  }
  console.error('\n这些绑定在运行时取不到值，会中断组件渲染（页面缺块/按钮消失/点击无反应）。')
  console.error('请在 <script setup> 中声明它们（或修正模板中的拼写）。')
  process.exit(1)
}

console.log(`[check-bindings] OK — 已检查 ${checked} 个 script setup 组件，模板绑定全部有定义。`)
