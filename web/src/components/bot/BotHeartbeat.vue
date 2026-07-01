<template>
  <div class="hb-wrap" data-testid="bot-heartbeat">
    <!-- 配置区 -->
    <div class="hb-config">
      <div class="cfg-row">
        <div class="cfg-label">
          <div class="cl-title">启用心跳</div>
          <div class="cl-sub">定期触发 Agent 检查是否有需要关注的事项</div>
        </div>
        <t-switch v-model="cfg.enabled" size="large" />
      </div>

      <div class="cfg-field">
        <label class="lbl">心跳间隔（分钟）</label>
        <t-input-number
          v-model="cfg.interval"
          :min="1"
          :max="1440"
          :step="1"
          theme="normal"
          style="width: 100%"
          placeholder="30"
        />
      </div>

      <div class="cfg-foot">
        <t-button theme="default" :loading="saving" @click="saveConfig">保存设置</t-button>
      </div>
    </div>

    <t-divider />

    <!-- 日志区 -->
    <div class="hb-logs">
      <div class="logs-head">
        <span class="logs-title">心跳日志</span>
        <div class="logs-actions">
          <t-select v-model="filter" :options="filterOptions" size="small" style="width: 120px" @change="loadLogs" />
          <t-button variant="outline" size="small" :loading="clearing" @click="clearLogs">清空日志</t-button>
          <t-button variant="outline" size="small" :loading="loadingLogs" @click="loadLogs">刷新</t-button>
        </div>
      </div>

      <t-table
        :data="logs"
        :columns="columns"
        row-key="id"
        :loading="loadingLogs"
        size="medium"
        :bordered="false"
        empty="暂无心跳日志"
        class="logs-table"
      >
        <template #status="{ row }">
          <span class="st-tag" :class="row.status">{{ row.status === 'alert' ? '告警' : '正常' }}</span>
        </template>
        <template #cost="{ row }">{{ row.cost }}s</template>
        <template #result="{ row }"><div class="result-cell">{{ row.result }}</div></template>
      </t-table>
    </div>
  </div>
</template>

<script setup>
import { ref, reactive } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { botHeartbeatApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

/* 配置 */
const cfg = reactive({ enabled: true, interval: 30 })
const saving = ref(false)

async function loadConfig() {
  const c = await botHeartbeatApi.getConfig(props.botId)
  cfg.enabled = c.enabled
  cfg.interval = c.interval
}
async function saveConfig() {
  saving.value = true
  try {
    await botHeartbeatApi.updateConfig(props.botId, { enabled: cfg.enabled, interval: cfg.interval })
    MessagePlugin.success('设置已保存')
  } catch (e) {
    MessagePlugin.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

/* 日志 */
const filter = ref('all')
const filterOptions = [
  { label: '全部', value: 'all' },
  { label: '正常', value: 'normal' },
  { label: '告警', value: 'alert' }
]
const logs = ref([])
const loadingLogs = ref(false)
const clearing = ref(false)

const columns = [
  { colKey: 'status', title: '状态', width: 120 },
  { colKey: 'time', title: '时间', width: 200 },
  { colKey: 'cost', title: '耗时', width: 120 },
  { colKey: 'result', title: '结果' }
]

async function loadLogs() {
  loadingLogs.value = true
  try {
    const res = await botHeartbeatApi.listLogs(props.botId, filter.value)
    logs.value = res.logs || []
  } finally {
    loadingLogs.value = false
  }
}

function clearLogs() {
  const dlg = DialogPlugin.confirm({
    header: '清空心跳日志', body: '确认清空所有心跳日志？该操作不可恢复。', theme: 'warning',
    onConfirm: async () => {
      clearing.value = true
      try {
        await botHeartbeatApi.clearLogs(props.botId)
        dlg.destroy()
        MessagePlugin.success('已清空')
        await loadLogs()
      } finally {
        clearing.value = false
      }
    }
  })
}

loadConfig()
loadLogs()
</script>

<style scoped>
.hb-wrap { width: 100%; }

/* 配置 */
.hb-config { max-width: 100%; }
.cfg-row { display: flex; align-items: flex-start; justify-content: space-between; gap: 24px; margin-bottom: 20px; }
.cl-title { font-size: 15px; font-weight: 600; color: #1d1d1f; }
.cl-sub { font-size: 13px; color: #999; margin-top: 4px; }
.cfg-field { display: flex; flex-direction: column; margin-bottom: 16px; }
.lbl { font-size: 13px; font-weight: 600; color: #1d1d1f; margin-bottom: 8px; }
.cfg-foot { display: flex; justify-content: flex-end; }

/* 日志 */
.logs-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 16px; }
.logs-title { font-size: 15px; font-weight: 600; color: #1d1d1f; }
.logs-actions { display: flex; align-items: center; gap: 10px; }

.st-tag {
  display: inline-flex; align-items: center; justify-content: center;
  font-size: 12px; font-weight: 600; padding: 4px 14px; border-radius: 8px;
}
.st-tag.alert { background: #1d1d1f; color: #fff; }
.st-tag.normal { background: #f2f3f5; color: #666; }

.result-cell { font-size: 13px; color: #444; line-height: 1.6; white-space: pre-wrap; word-break: break-word; }
.logs-table :deep(td) { vertical-align: top; }
</style>
