import fs from 'node:fs'
import path from 'node:path'
import assert from 'node:assert/strict'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { createServer } from 'vite'
import { createSSRApp, h } from 'vue'
import { renderToString } from '@vue/server-renderer'
import { parse as parseSfc } from '@vue/compiler-sfc'
import { parse, parseExpression } from '@babel/parser'
import { applyPrimaryColor, applySavedBrand, BRAND_PALETTES } from '../src/utils/brand.js'

const root = fileURLToPath(new URL('../src/', import.meta.url))
function filesAt(directory) {
  return fs.readdirSync(directory, { withFileTypes: true }).flatMap(entry => {
    const filename = path.join(directory, entry.name)
    return entry.isDirectory() ? filesAt(filename) : /\.(vue|js)$/.test(filename) ? [filename] : []
  })
}
function walk(node, visit) {
  if (!node || typeof node !== 'object') return
  visit(node)
  for (const [key, child] of Object.entries(node)) {
    if (key === 'loc') continue
    if (Array.isArray(child)) child.forEach(value => walk(value, visit))
    else if (child && typeof child === 'object') walk(child, visit)
  }
}
function iconUsage() {
  const names = new Set()
  function literalResults(node) {
    if (!node) return
    if (node.type === 'StringLiteral' && node.value) names.add(node.value)
    if (node.type === 'ConditionalExpression') { literalResults(node.consequent); literalResults(node.alternate) }
    if (node.type === 'LogicalExpression') { literalResults(node.left); literalResults(node.right) }
  }
  for (const filename of filesAt(root)) {
    const source = fs.readFileSync(filename, 'utf8')
    const descriptor = filename.endsWith('.vue') ? parseSfc(source).descriptor : null
    const script = descriptor ? [descriptor.script?.content || '', descriptor.scriptSetup?.content || ''].join('\n') : source
    const ast = parse(script, { sourceType: 'module' })
    const bindings = new Map()
    walk(ast, node => {
      if (node.type === 'VariableDeclarator' && node.id.type === 'Identifier') bindings.set(node.id.name, node.init)
      if (node.type === 'FunctionDeclaration' && node.id) bindings.set(node.id.name, node)
      if (node.type === 'ObjectProperty' && (node.key.name || node.key.value) === 'icon') literalResults(node.value)
    })
    walk(descriptor?.template?.ast, node => {
      if (node.type !== 1 || node.tag !== 't-icon') return
      for (const prop of node.props) {
        if (prop.type === 6 && prop.name === 'name') names.add(prop.value.content)
        if (prop.type !== 7 || prop.name !== 'bind' || prop.arg?.content !== 'name') continue
        const expression = parseExpression(prop.exp.content)
        literalResults(expression)
        const bindingName = expression.type === 'Identifier' ? expression.name : expression.type === 'CallExpression' && expression.callee.type === 'Identifier' ? expression.callee.name : null
        if (bindingName && bindings.has(bindingName)) {
          walk(bindings.get(bindingName), child => { if (child.type === 'ReturnStatement') literalResults(child.argument) })
        }
      }
    })
  }
  return names
}

test('all application icons render from a bounded local SVG registry', async t => {
  const server = await createServer({ server: { middlewareMode: true }, appType: 'custom', ssr: { noExternal: ['tdesign-icons-vue-next'] } })
  try {
    const { APP_ICONS, resolveAppIcon } = await server.ssrLoadModule('/src/components/common/appIcons.js')
    const { default: AppIcon } = await server.ssrLoadModule('/src/components/common/AppIcon.js')
    await t.test('covers static template names, dynamic icon fields, and computed/function returns', () => {
      const missing = [...iconUsage()].filter(name => !Object.hasOwn(APP_ICONS, name))
      assert.deepEqual(missing, [])
      assert(Object.keys(APP_ICONS).length < 100, 'bundle a deliberate icon set, not the entire library')
    })
    await t.test('every registered icon has inline geometry and no external sprite reference', async () => {
      for (const name of Object.keys(APP_ICONS)) {
        const html = await renderToString(createSSRApp({ render: () => h(AppIcon, { name }) }))
        assert.match(html, /<(?:path|circle|rect|line|polyline|polygon|ellipse)\b/, name)
        assert.doesNotMatch(html, /<use\b|<script\b|https?:\/\//, name)
      }
    })
    await t.test('unknown and prototype names use a local fallback', async () => {
      for (const name of ['unregistered-tool-icon', 'toString', '__proto__', 'https://example.com/icon.svg']) {
        assert.equal(resolveAppIcon(name), APP_ICONS['help-circle'])
        const html = await renderToString(createSSRApp({ render: () => h(AppIcon, { name }) }))
        assert.match(html, /<path\b/)
        assert.doesNotMatch(html, /<use\b|<script\b/)
      }
    })
    await t.test('forwards class, accessible labels, sizing and native click events', async () => {
      const html = await renderToString(createSSRApp({ render: () => h(AppIcon, { name: 'search', size: 24, class: 'test-icon', 'aria-label': '搜索' }) }))
      assert.match(html, /test-icon/)
      assert.match(html, /font-size:24px/)
      assert.match(html, /aria-label="搜索"/)
      assert.doesNotMatch(html, /aria-hidden="true"/)
      let emitted
      const vnode = AppIcon.setup({ name: 'search', size: '18px' }, { attrs: {}, emit: (...args) => { emitted = args } })()
      const event = { stopPropagation() {} }
      vnode.props.onClick({ e: event })
      assert.deepEqual(emitted, ['click', event])
    })
  } finally { await server.close() }
})

test('fresh settings use indigo while explicit existing color preferences remain intact', () => {
  const oldDocument = globalThis.document
  const oldStorage = globalThis.localStorage
  const values = new Map()
  let settings = '{}'
  globalThis.document = { documentElement: { style: { setProperty: (key, value) => values.set(key, value) } } }
  globalThis.localStorage = { getItem: () => settings }
  try {
    applySavedBrand()
    assert.equal(values.get('--bp-accent'), '#5b5fef')
    for (const color of ['green', 'blue', 'purple']) {
      settings = JSON.stringify({ primaryColor: color })
      applySavedBrand()
      assert.equal(values.get('--bp-accent'), BRAND_PALETTES[color].base)
      assert.equal(values.get('--bp-accent-soft-strong'), BRAND_PALETTES[color].focus)
      assert.equal(values.get('--td-brand-color-focus'), BRAND_PALETTES[color].focus)
    }
    settings = '{invalid'
    applySavedBrand()
    assert.equal(values.get('--bp-accent'), '#5b5fef')
    applyPrimaryColor('toString')
    assert.equal(values.get('--bp-accent'), '#5b5fef')
  } finally {
    if (oldDocument === undefined) delete globalThis.document
    else globalThis.document = oldDocument
    if (oldStorage === undefined) delete globalThis.localStorage
    else globalThis.localStorage = oldStorage
  }
})
