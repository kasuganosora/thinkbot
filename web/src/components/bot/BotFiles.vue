<template>
  <div class="bf-wrap" data-testid="bot-files">
    <!-- 工具栏 -->
    <div class="bf-bar">
      <div class="bf-crumb">
        <t-icon name="folder" />
        <template v-for="(seg, i) in crumbs" :key="i">
          <span class="crumb-sep" v-if="i > 0">›</span>
          <span class="crumb-link" @click="goCrumb(i)">{{ seg.label }}</span>
        </template>
      </div>
      <div class="bf-ops">
        <t-button variant="outline" size="small" @click="triggerUpload"><template #icon><t-icon name="upload" /></template>上传</t-button>
        <t-button variant="outline" size="small" @click="openMkdir"><template #icon><t-icon name="folder-add" /></template>新建文件夹</t-button>
        <t-button variant="outline" size="small" shape="square" @click="load"><t-icon name="refresh" /></t-button>
        <input ref="fileInput" type="file" hidden @change="onUpload" />
      </div>
    </div>

    <!-- 文件表 -->
    <div class="bf-table">
      <div class="bf-thead">
        <span class="col-name">名称</span>
        <span class="col-size">大小</span>
        <span class="col-time">修改时间</span>
      </div>
      <div class="bf-tbody">
        <div v-if="path !== '/'" class="bf-row" @click="goUp">
          <span class="col-name"><t-icon name="folder" class="row-icon dir" /> ..</span>
          <span class="col-size" /><span class="col-time" />
        </div>
        <div
          v-for="e in entries"
          :key="e.name"
          class="bf-row"
          :class="{ clickable: e.type === 'dir' }"
          @click="e.type === 'dir' && enter(e)"
        >
          <span class="col-name">
            <t-icon :name="e.type === 'dir' ? 'folder' : 'file'" class="row-icon" :class="e.type" />
            {{ e.name }}
          </span>
          <span class="col-size">{{ e.type === 'dir' ? '' : fmtSize(e.size) }}</span>
          <span class="col-time">{{ fmtTime(e.mtime) }}</span>
        </div>
        <t-empty v-if="!entries.length && path !== '/'" description="空目录" class="bf-empty" />
      </div>
    </div>

    <t-dialog v-model:visible="mkdirVisible" header="新建文件夹" :width="400" @confirm="confirmMkdir">
      <t-input v-model="mkdirName" placeholder="文件夹名称" />
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { botFileApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const path = ref('/data')
const entries = ref([])
const fileInput = ref()

const crumbs = computed(() => {
  const segs = path.value.split('/').filter(Boolean)
  const arr = [{ label: '/', path: '/' }]
  let acc = ''
  segs.forEach(s => { acc += `/${s}`; arr.push({ label: s, path: acc }) })
  return arr
})

async function load() { entries.value = await botFileApi.list(props.botId, path.value) }
onMounted(load)

function enter(e) { path.value = path.value === '/' ? `/${e.name}` : `${path.value}/${e.name}`; load() }
function goUp() { const p = path.value.split('/').slice(0, -1).join('/') || '/'; path.value = p; load() }
function goCrumb(i) { path.value = crumbs.value[i].path; load() }

function fmtSize(n) {
  if (!n) return '0 B'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}
function fmtTime(v) {
  if (!v) return ''
  const d = new Date(v); if (isNaN(d)) return String(v)
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}

function triggerUpload() { fileInput.value?.click() }
async function onUpload(e) {
  const f = e.target.files?.[0]; if (!f) return
  await botFileApi.upload(props.botId, path.value, f.name, f.size)
  e.target.value = ''
  await load(); MessagePlugin.success('上传成功')
}

const mkdirVisible = ref(false)
const mkdirName = ref('')
function openMkdir() { mkdirName.value = ''; mkdirVisible.value = true }
async function confirmMkdir() {
  if (!mkdirName.value.trim()) return MessagePlugin.warning('请输入名称')
  try {
    await botFileApi.mkdir(props.botId, path.value, mkdirName.value.trim())
    mkdirVisible.value = false; await load(); MessagePlugin.success('已创建')
  } catch (err) { MessagePlugin.error(err.message || '创建失败') }
}
</script>

<style scoped>
.bf-wrap { display: flex; flex-direction: column; height: 100%; }
.bf-bar { display: flex; align-items: center; justify-content: space-between; margin-bottom: 12px; }
.bf-crumb { display: flex; align-items: center; gap: 6px; color: var(--bp-label-secondary); font-size: 14px; }
.crumb-sep { color: var(--bp-label-quaternary); }
.crumb-link { cursor: pointer; }
.crumb-link:hover { color: var(--bp-label); text-decoration: underline; }
.bf-ops { display: flex; gap: 8px; }
.bf-table { flex: 1; border: none; box-shadow: var(--bp-shadow-sm); border-radius: 12px; overflow: hidden; display: flex; flex-direction: column; background: var(--bp-surface); }
.bf-thead { display: flex; padding: 12px 18px; border-bottom: var(--bp-hairline); font-size: 13px; color: var(--bp-label-tertiary); }
.bf-tbody { flex: 1; overflow-y: auto; }
.bf-row { display: flex; align-items: center; padding: 11px 18px; border-bottom: var(--bp-hairline); font-size: 14px; }
.bf-row.clickable { cursor: pointer; transition: background var(--bp-duration) var(--bp-ease-out), transform var(--bp-duration) var(--bp-ease-out); }
.bf-row.clickable:active { transform: scale(var(--bp-press-scale)); }
.bf-row.clickable:hover { background: var(--bp-surface-fill-hover); }
.col-name { flex: 1; display: flex; align-items: center; gap: 10px; color: var(--bp-label); }
.col-size { width: 120px; color: var(--bp-label-tertiary); font-size: 13px; }
.col-time { width: 150px; color: var(--bp-label-tertiary); font-size: 13px; }
.row-icon.dir { color: var(--bp-accent); }
.row-icon.file { color: var(--bp-label-tertiary); }
.bf-empty { margin: 40px auto; }
</style>
