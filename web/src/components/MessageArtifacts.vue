<template>
  <section v-if="artifacts.length && botId" class="message-artifacts" aria-label="本回复写入的文件" data-testid="message-artifacts">
    <div class="artifacts-heading"><t-icon name="file" /><span>本回复写入的文件 <span class="artifact-version">· 当前版本</span></span></div>
    <ul class="artifacts-list">
      <li v-for="file in artifacts" :key="file.path" class="artifact-item">
        <button class="artifact-file" type="button" :title="`${file.path} · 在工作区查看`" :aria-label="`在工作区查看 ${file.name}`" @click="openFile(file)">
          <t-icon name="file-attachment" class="artifact-icon" />
          <span class="artifact-label"><span class="artifact-name">{{ file.name }}</span><span class="artifact-path">{{ file.path }}</span></span>
          <t-icon name="jump" class="artifact-open" />
        </button>
        <button class="artifact-download" type="button" :disabled="downloading.has(file.path)" :aria-label="`下载 ${file.name}`" :title="downloading.has(file.path) ? '正在下载' : '下载当前文件'" @click="download(file)">
          <t-loading v-if="downloading.has(file.path)" size="14px" /><t-icon v-else name="download" />
        </button>
        <div v-if="errors[file.path]" class="artifact-error" role="alert"><span>{{ errors[file.path] }}</span><button type="button" :disabled="downloading.has(file.path)" @click="download(file)">重试下载</button></div>
      </li>
    </ul>
    <p class="artifacts-note">文件位于 Bot 共享工作区，内容可能被后续任务修改。</p>
  </section>
</template>

<script setup>
import { computed, onBeforeUnmount, ref } from 'vue'
import { sessionToolApi } from '@/api/services'
import { useLayoutStore } from '@/stores/layout'
import { extractMessageArtifacts } from '@/utils/messageArtifacts'

const props = defineProps({
  message: { type: Object, required: true },
  botId: { type: [String, Number], required: true },
  sessionId: { type: [String, Number], default: null },
})
const layout = useLayoutStore()
const artifacts = computed(() => extractMessageArtifacts(props.message, { botId: props.botId, sessionId: props.sessionId }))
const downloading = ref(new Set())
const errors = ref({})
const requests = new Set()
let alive = true

function openFile(file) { layout.openFile({ botId: props.botId, path: file.path }) }

async function download(file) {
  if (!props.botId || downloading.value.has(file.path)) return
  const botId = String(props.botId)
  const controller = new AbortController()
  requests.add(controller)
  downloading.value.add(file.path)
  delete errors.value[file.path]
  try {
    const response = await fetch(sessionToolApi.downloadUrl(botId, file.path), { credentials: 'include', signal: controller.signal })
    if (!response.ok) {
      const detail = await response.json().catch(() => ({}))
      throw new Error(detail.message || (response.status === 404 ? '文件已不存在或无权访问' : '下载失败，请重试'))
    }
    // Successful JSON files are valid artifacts too; HTTP status identifies API failures.
    const blob = await response.blob()
    if (!alive || String(props.botId) !== botId) return
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = file.name
    document.body.appendChild(link)
    link.click()
    link.remove()
    setTimeout(() => URL.revokeObjectURL(url), 1500)
  } catch (error) {
    if (alive && error.name !== 'AbortError' && String(props.botId) === botId) errors.value[file.path] = error.message || '下载失败，请重试'
  } finally {
    requests.delete(controller)
    downloading.value.delete(file.path)
  }
}

onBeforeUnmount(() => {
  alive = false
  for (const controller of requests) controller.abort()
})
</script>

<style scoped>
.message-artifacts { margin-top: 14px; padding-top: 12px; border-top: var(--bp-hairline); font-size: 12px; }
.artifacts-heading { display: flex; align-items: center; gap: 6px; margin-bottom: 8px; color: var(--bp-label-secondary); font-weight: 550; }
.artifact-version { font-weight: 400; color: var(--bp-label-tertiary); }
.artifacts-list { display: flex; flex-direction: column; gap: 6px; padding: 0; margin: 0; list-style: none; }
.artifact-item { display: flex; flex-wrap: wrap; align-items: center; gap: 4px; border: var(--bp-hairline); border-radius: var(--bp-radius-sm); background: var(--bp-surface); }
.artifact-file { display: flex; align-items: center; gap: 9px; flex: 1; min-width: 0; padding: 9px 10px; border: 0; border-radius: var(--bp-radius-sm); background: transparent; font: inherit; text-align: left; color: var(--bp-label); cursor: pointer; }
.artifact-file:hover { background: var(--bp-accent-soft); }
.artifact-icon { font-size: 18px; color: var(--bp-accent); flex-shrink: 0; }
.artifact-label { min-width: 0; flex: 1; display: flex; flex-direction: column; gap: 2px; }
.artifact-name, .artifact-path { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.artifact-name { font-weight: 550; }
.artifact-path { color: var(--bp-label-secondary); font-size: 11px; }
.artifact-open { color: var(--bp-label-tertiary); flex-shrink: 0; font-size: 13px; }
.artifact-download { width: 34px; height: 34px; margin-right: 5px; display: inline-flex; align-items: center; justify-content: center; border: 0; border-radius: var(--bp-radius-xs); background: transparent; color: var(--bp-label-secondary); cursor: pointer; font-size: 16px; }
.artifact-download:hover { background: var(--bp-accent-soft); color: var(--bp-accent); }
.artifact-download:disabled { opacity: .6; cursor: wait; }
.artifacts-note { margin: 8px 0 0; color: var(--bp-label-tertiary); font-size: 11px; line-height: 1.5; }
.artifact-error { width: 100%; display: flex; flex-wrap: wrap; align-items: center; gap: 5px 10px; padding: 0 10px 9px; color: var(--bp-danger); font-size: 11px; }
.artifact-error button { border: 0; border-radius: 4px; padding: 3px; color: var(--bp-accent); background: transparent; cursor: pointer; font: inherit; }
button:focus-visible { outline: 2px solid var(--bp-accent); outline-offset: 2px; }
</style>
