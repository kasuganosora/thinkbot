<template>
  <div class="ctn-wrap" data-testid="bot-container">
    <!-- 头部 -->
    <div class="ctn-head">
      <div class="ch-text">
        <h3 class="ch-title">容器管理</h3>
        <p class="ch-desc">管理当前 Bot 对应的运行容器。</p>
      </div>
      <div class="ch-actions">
        <t-button variant="outline" :loading="loading" @click="load">刷新</t-button>
        <t-button
          v-if="info.containerStatus === 'running'"
          variant="outline"
          :loading="acting"
          @click="onStop"
        >停止</t-button>
        <t-button v-else variant="outline" :loading="acting" @click="onStart">启动</t-button>
      </div>
    </div>

    <!-- 容器信息卡 -->
    <div class="info-card">
      <div class="info-grid">
        <div class="info-cell">
          <div class="ic-label">容器 ID</div>
          <div class="ic-value mono">{{ info.containerId || '-' }}</div>
        </div>
        <div class="info-cell">
          <div class="ic-label">容器状态</div>
          <div class="ic-value">{{ statusText(info.containerStatus) }}</div>
        </div>

        <div class="info-cell">
          <div class="ic-label">任务状态</div>
          <div class="ic-value">{{ statusText(info.taskStatus) }}</div>
        </div>
        <div class="info-cell">
          <div class="ic-label">命名空间</div>
          <div class="ic-value">{{ info.namespace || '-' }}</div>
        </div>

        <div class="info-cell">
          <div class="ic-label">镜像</div>
          <div class="ic-value">{{ info.image || '-' }}</div>
        </div>
        <div class="info-cell"></div>

        <div class="info-cell">
          <div class="ic-label">CDI 设备</div>
          <div class="ic-value">{{ info.cdiDevice || '未附加 GPU' }}</div>
        </div>
        <div class="info-cell"></div>

        <div class="info-cell">
          <div class="ic-label">容器路径</div>
          <div class="ic-value">{{ info.containerPath || ' ' }}</div>
        </div>
        <div class="info-cell"></div>

        <div class="info-cell">
          <div class="ic-label">保留数据</div>
          <div class="ic-value">{{ info.keepData ? '是' : '无' }}</div>
        </div>
        <div class="info-cell">
          <div class="ic-label">创建时间</div>
          <div class="ic-value">{{ fmt(info.createdAt) }}</div>
        </div>

        <div class="info-cell">
          <div class="ic-label">更新时间</div>
          <div class="ic-value">{{ fmt(info.updatedAt) }}</div>
        </div>
        <div class="info-cell"></div>
      </div>
    </div>

    <!-- GPU 提示 -->
    <div class="ctn-tip">GPU 配置变更需要重建容器后才会生效，单纯启动或停止不会更新当前已附加的设备。</div>

    <!-- 数据操作 -->
    <div class="block">
      <h4 class="blk-title">数据操作</h4>
      <p class="blk-desc">独立管理容器 <code>/data</code> 目录的导入、导出与恢复。</p>
      <div class="btn-row">
        <t-button variant="outline" :loading="busy.export" @click="onExport">导出数据</t-button>
        <t-button variant="outline" @click="onImport">导入数据</t-button>
        <t-button variant="outline" :loading="busy.restore" @click="onRestore">恢复保留数据</t-button>
      </div>
    </div>

    <!-- 删除容器 -->
    <div class="block">
      <h4 class="blk-title danger">删除容器</h4>
      <p class="blk-desc">删除容器前请明确选择是否先保留 <code>/data</code>，避免误删用户数据。</p>
      <div class="btn-row">
        <t-button variant="outline" @click="onRemove(true)">保留数据后删除</t-button>
        <t-button theme="danger" @click="onRemove(false)">彻底删除</t-button>
      </div>
    </div>

    <!-- 创建快照 -->
    <div class="block snap-create">
      <div class="sc-row">
        <t-input v-model="snapName" placeholder="快照显示名称（可选）" class="sc-input" />
        <t-button theme="default" class="sc-btn" :loading="busy.snap" @click="onCreateSnapshot">创建快照</t-button>
      </div>
      <p class="blk-desc">这里只填写用户可见的显示名称，系统会自动生成内部快照名。</p>
    </div>

    <!-- 快照表格 -->
    <div class="snap-table">
      <t-table
        row-key="id"
        :data="snapshots"
        :columns="columns"
        :loading="loading"
        size="medium"
        :bordered="false"
        cell-empty-content="-"
      />
    </div>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted, h } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { botContainerApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const loading = ref(false)
const acting = ref(false)
const busy = reactive({ export: false, restore: false, snap: false })
const info = ref({})
const snapshots = ref([])
const snapName = ref('')

const columns = [
  { colKey: 'name', title: '名称', width: 380, ellipsis: true,
    cell: (_h, { row }) => h('span', { class: 'snap-name' }, row.name) },
  { colKey: 'version', title: '版本', width: 70 },
  { colKey: 'source', title: '来源', width: 120 },
  { colKey: 'parent', title: '父级', ellipsis: true,
    cell: (_h, { row }) => h('span', { class: 'snap-parent' }, row.parent || '-') },
  { colKey: 'createdAt', title: '创建时间', width: 170,
    cell: (_h, { row }) => fmt(row.createdAt) },
  { colKey: 'op', title: '操作', width: 80, cell: () => '-' }
]

function statusText(s) {
  return ({ running: '运行中', stopped: '已停止', removed: '已删除' })[s] || s || '-'
}
function fmt(iso) {
  if (!iso) return '-'
  const d = new Date(iso)
  if (isNaN(d)) return iso
  const p = n => String(n).padStart(2, '0')
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

async function load() {
  loading.value = true
  try {
    const [i, s] = await Promise.all([
      botContainerApi.get(props.botId),
      botContainerApi.snapshots(props.botId)
    ])
    info.value = i
    snapshots.value = s
  } finally {
    loading.value = false
  }
}
onMounted(load)

async function onStart() {
  acting.value = true
  try { info.value = await botContainerApi.start(props.botId); MessagePlugin.success('容器已启动') }
  finally { acting.value = false }
}
async function onStop() {
  acting.value = true
  try { info.value = await botContainerApi.stop(props.botId); MessagePlugin.success('容器已停止') }
  finally { acting.value = false }
}

async function onExport() {
  busy.export = true
  try { await botContainerApi.exportData(props.botId); MessagePlugin.success('已开始导出 /data') }
  finally { busy.export = false }
}
function onImport() {
  const dlg = DialogPlugin.confirm({
    header: '导入数据',
    body: '请选择要导入到 /data 的数据包（模拟）。导入会覆盖容器内现有数据。',
    confirmBtn: '确认导入',
    onConfirm: async () => {
      await botContainerApi.importData(props.botId, {})
      dlg.destroy(); MessagePlugin.success('数据导入完成')
    }
  })
}
async function onRestore() {
  busy.restore = true
  try { await botContainerApi.restoreData(props.botId); MessagePlugin.success('已恢复保留数据') }
  finally { busy.restore = false }
}

function onRemove(keepData) {
  const dlg = DialogPlugin.confirm({
    header: keepData ? '保留数据后删除容器' : '彻底删除容器',
    theme: keepData ? 'warning' : 'danger',
    body: keepData
      ? '将删除容器但保留 /data 目录，后续可恢复。确认继续？'
      : '将彻底删除容器及其 /data 数据，该操作不可恢复。确认继续？',
    confirmBtn: keepData ? '保留数据删除' : '彻底删除',
    onConfirm: async () => {
      await botContainerApi.remove(props.botId, keepData)
      dlg.destroy()
      MessagePlugin.success(keepData ? '已删除容器（保留数据）' : '容器已彻底删除')
      await load()
    }
  })
}

async function onCreateSnapshot() {
  busy.snap = true
  try {
    await botContainerApi.createSnapshot(props.botId, snapName.value.trim())
    snapName.value = ''
    MessagePlugin.success('快照已创建')
    await load()
  } finally { busy.snap = false }
}
</script>

<style scoped>
.ctn-wrap { width: 100%; display: flex; flex-direction: column; gap: 18px; }

/* 头部 */
.ctn-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.ch-title { font-size: 18px; font-weight: 700; margin: 0; color: #1d1d1f; }
.ch-desc { font-size: 13px; color: #999; margin: 6px 0 0; }
.ch-actions { display: flex; gap: 10px; flex-shrink: 0; }

/* 信息卡 */
.info-card { border: 1px solid #ececec; border-radius: 14px; padding: 22px 26px; }
.info-grid { display: grid; grid-template-columns: 1fr 1fr; row-gap: 18px; column-gap: 40px; }
.ic-label { font-size: 13px; color: #999; margin-bottom: 4px; }
.ic-value { font-size: 14px; color: #1d1d1f; word-break: break-all; }
.ic-value.mono { font-family: 'SF Mono', Menlo, Consolas, monospace; font-size: 13px; }

/* 提示条 */
.ctn-tip {
  border: 1px solid #ececec; border-radius: 12px; padding: 14px 18px;
  font-size: 13px; color: #888; background: #fafafa;
}

/* 区块 */
.block { display: flex; flex-direction: column; }
.blk-title { font-size: 15px; font-weight: 700; margin: 0 0 4px; color: #1d1d1f; }
.blk-title.danger { color: #d54941; }
.blk-desc { font-size: 13px; color: #999; margin: 0 0 14px; }
.blk-desc code { background: #f2f3f5; padding: 1px 6px; border-radius: 4px; font-size: 12px; color: #555; }
.btn-row { display: flex; gap: 12px; flex-wrap: wrap; }

/* 创建快照 */
.snap-create { padding-top: 4px; }
.sc-row { display: flex; gap: 14px; align-items: center; }
.sc-input { max-width: 360px; }
.sc-btn { background: #1d1d1f; border-color: #1d1d1f; color: #fff; }
.sc-btn:hover { background: #333; border-color: #333; }
.snap-create .blk-desc { margin-top: 10px; }

/* 快照表格 */
.snap-table { margin-top: 4px; }
.snap-table :deep(.snap-name) { font-weight: 600; color: #1d1d1f; word-break: break-all; }
.snap-table :deep(.snap-parent) { color: #555; word-break: break-all; font-family: 'SF Mono', Menlo, Consolas, monospace; font-size: 12px; }
.snap-table :deep(.t-table th) { background: #fafafa; color: #888; font-weight: 600; }
</style>
