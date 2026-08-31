<template>
  <div class="hb-wrap" data-testid="bot-heartbeat">
    <!-- 配置区 -->
    <div class="hb-config">
      <div class="cfg-row">
        <div class="cfg-label">
          <div class="cl-title">启用心跳</div>
          <div class="cl-sub">定期触发 Agent 自主检查是否有需要关注的事项（平台策略 + bot 自主两级闸门）</div>
        </div>
        <t-switch v-model="cfg.enabled" size="large" />
      </div>

      <div class="cfg-grid">
        <div class="cfg-field">
          <label class="lbl">心跳间隔（分钟）</label>
          <t-input-number
            v-model="cfg.interval"
            :min="1" :max="1440" :step="1"
            theme="normal" style="width: 100%" placeholder="30"
          />
          <div class="field-hint">两次心跳触发的最小间隔，1–1440。</div>
        </div>

        <div class="cfg-field">
          <label class="lbl">空闲放行周期（IdleWakeEvery）</label>
          <t-input-number
            v-model="cfg.idle_wake_every"
            :min="1" :max="100" :step="1"
            theme="normal" style="width: 100%" placeholder="4"
          />
          <div class="field-hint">连续无信号的心跳被准入关卡拒绝（0-step 不耗主 LLM）；达到该次数强制放行一次。设为 1 = 关闭准入关卡。</div>
        </div>

        <div class="cfg-field">
          <label class="lbl">允许主动发言（allow_post）</label>
          <t-switch v-model="cfg.allow_post" size="large" />
          <div class="field-hint">第一级闸门（平台策略）。关闭后心跳仍可思考+记笔记，但不对外发言。</div>
        </div>

        <div class="cfg-field">
          <label class="lbl">连续行动上限（MaxConsecutiveWakes）</label>
          <t-input-number
            v-model="cfg.max_consecutive_wakes"
            :min="1" :max="100" :step="1"
            theme="normal" style="width: 100%" placeholder="3"
          />
          <div class="field-hint">连续「产生行动的唤醒」超过该值后降级为纯注入（不发言），专治自激链。</div>
        </div>

        <div class="cfg-field">
          <label class="lbl">行动冷却（分钟，CooldownMin）</label>
          <t-input-number
            v-model="cfg.cooldown_min"
            :min="0" :max="1440" :step="1"
            theme="normal" style="width: 100%" placeholder="0"
          />
          <div class="field-hint">两次「产生行动的唤醒」最小冷却；0 = 退化为心跳周期。用于重置连续唤醒预算。</div>
        </div>
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
          <t-select v-model="filter" :options="filterOptions" size="small" style="width: 150px" @change="loadLogs" />
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
          <span class="st-tag" :class="statusMeta(row.status).cls">{{ statusMeta(row.status).label }}</span>
          <span v-if="row.reason" class="reason-tag" :title="reasonTitle(row.reason)">{{ reasonLabel(row.reason) }}</span>
        </template>
        <template #cost="{ row }">{{ row.cost }}s</template>
        <template #actions="{ row }">
          <span v-if="row.actions && row.actions.length" class="act-cell">{{ row.actions.join(' / ') }}</span>
          <span v-else class="muted">—</span>
        </template>
        <template #admitted="{ row }">
          <span class="adm" :class="row.admitted ? 'adm-yes' : 'adm-no'">
            {{ row.admitted ? '已准入' : '跳过(0-step)' }}
          </span>
        </template>
        <template #result="{ row }">
          <div class="result-cell">
            <div>{{ row.result }}</div>
            <div v-if="row.traceId" class="trace-cell">trace: {{ row.traceId }}</div>
          </div>
        </template>
      </t-table>
    </div>
  </div>
</template>

<script setup>
import { ref, reactive } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { botHeartbeatApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const STATUS_LABELS = {
  acted:      { label: '已行动', cls: 'acted' },
  note:       { label: '记笔记', cls: 'note' },
  silent:     { label: '静默',   cls: 'silent' },
  suppressed: { label: '已压制', cls: 'suppressed' },
  error:      { label: '错误',   cls: 'error' }
}
function statusMeta(s) {
  return STATUS_LABELS[s] || { label: s || '未知', cls: 'silent' }
}

const REASON_LABELS = {
  platform_policy: '平台策略',
  frequency_cap: '频控降级'
}
function reasonLabel(r) {
  return REASON_LABELS[r] || r
}
function reasonTitle(r) {
  if (r === 'platform_policy') return 'allow_post=false：平台级关闭主动发言（第一级闸门）'
  if (r === 'frequency_cap') return '连续行动达上限 / 冷却窗内：频控降级（第二级闸门）'
  return r
}

/* 配置 */
const cfg = reactive({
  enabled: true,
  interval: 30,
  allow_post: false,
  max_consecutive_wakes: 3,
  cooldown_min: 0,
  idle_wake_every: 4
})
const saving = ref(false)

async function loadConfig() {
  try {
    const c = await botHeartbeatApi.getConfig(props.botId)
    cfg.enabled = c.enabled
    cfg.interval = c.interval
    if (typeof c.allow_post === 'boolean') cfg.allow_post = c.allow_post
    if (typeof c.max_consecutive_wakes === 'number') cfg.max_consecutive_wakes = c.max_consecutive_wakes
    if (typeof c.cooldown_min === 'number') cfg.cooldown_min = c.cooldown_min
    if (typeof c.idle_wake_every === 'number') cfg.idle_wake_every = c.idle_wake_every
  } catch (e) {
    // 静默失败会让用户对着一堆默认值以为这就是当前配置，一保存就把真配置覆盖掉
    MessagePlugin.error(`加载心跳配置失败：${e.message || e}`)
  }
}
async function saveConfig() {
  saving.value = true
  try {
    await botHeartbeatApi.updateConfig(props.botId, {
      enabled: cfg.enabled,
      interval: cfg.interval,
      allow_post: cfg.allow_post,
      max_consecutive_wakes: cfg.max_consecutive_wakes,
      cooldown_min: cfg.cooldown_min,
      idle_wake_every: cfg.idle_wake_every
    })
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
  { label: '全部',   value: 'all' },
  { label: '已行动', value: 'acted' },
  { label: '记笔记', value: 'note' },
  { label: '静默',   value: 'silent' },
  { label: '已压制', value: 'suppressed' },
  { label: '错误',   value: 'error' }
]
const logs = ref([])
const loadingLogs = ref(false)
const clearing = ref(false)

const columns = [
  { colKey: 'status', title: '状态', width: 110 },
  { colKey: 'time', title: '时间', width: 190 },
  { colKey: 'cost', title: '耗时', width: 90 },
  { colKey: 'actions', title: '行动', width: 130 },
  { colKey: 'admitted', title: '准入', width: 120 },
  { colKey: 'result', title: '结果 / trace' }
]

async function loadLogs() {
  loadingLogs.value = true
  try {
    const res = await botHeartbeatApi.listLogs(props.botId, filter.value)
    logs.value = res.logs || []
  } catch (e) {
    // 请求失败时保持上一次的数据不动，只提示；避免「空列表」被误读为「没有心跳」
    MessagePlugin.error(`加载心跳日志失败：${e.message || e}`)
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
      } catch (e) {
        MessagePlugin.error(`清空失败：${e.message || e}`)
      } finally {
        clearing.value = false
      }
    }
  })
}

// setup 里直接调用即可，早期这里同时挂了 onMounted(loadLogs) 与本处调用，
// 每次进入面板都会打两次同样的日志查询。
loadConfig()
loadLogs()
</script>

<style scoped>
.hb-wrap { width: 100%; }

/* 配置 */
.hb-config { max-width: 100%; }
.cfg-row { display: flex; align-items: flex-start; justify-content: space-between; gap: 24px; margin-bottom: 20px; }
.cl-title { font-size: 15px; font-weight: 600; color: #1d1d1f; }
.cl-sub { font-size: 13px; color: #999; margin-top: 4px; max-width: 520px; }
.cfg-grid { display: grid; grid-template-columns: repeat(2, minmax(240px, 1fr)); gap: 16px 28px; }
.cfg-field { display: flex; flex-direction: column; }
.lbl { font-size: 13px; font-weight: 600; color: #1d1d1f; margin-bottom: 8px; }
.field-hint { font-size: 12px; color: #999; margin-top: 6px; line-height: 1.5; }
.cfg-foot { display: flex; justify-content: flex-end; margin-top: 20px; }

/* 日志 */
.logs-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 16px; }
.logs-title { font-size: 15px; font-weight: 600; color: #1d1d1f; }
.logs-actions { display: flex; align-items: center; gap: 10px; }

.st-tag {
  display: inline-flex; align-items: center; justify-content: center;
  font-size: 12px; font-weight: 600; padding: 4px 12px; border-radius: 8px;
}
.st-tag.acted      { background: #e8f3ec; color: #1a7f4b; }
.st-tag.note       { background: #e8f0ff; color: #245bdb; }
.st-tag.silent     { background: #f2f3f5; color: #888; }
.st-tag.suppressed { background: #fff3e8; color: #c8730c; }
.st-tag.error      { background: #fdecec; color: #d54941; }

.reason-tag {
  display: inline-flex; align-items: center; justify-content: center;
  font-size: 11px; font-weight: 600; padding: 2px 8px; border-radius: 6px;
  margin-left: 6px; background: #fff3e8; color: #c8730c;
}

.act-cell { font-size: 12px; color: #333; }
.adm { font-size: 12px; font-weight: 600; }
.adm-yes { color: #1a7f4b; }
.adm-no  { color: #c8730c; }
.muted { color: #bbb; }

.result-cell { font-size: 13px; color: #444; line-height: 1.6; white-space: pre-wrap; word-break: break-word; }
.trace-cell { font-size: 11px; color: #aaa; margin-top: 4px; font-family: ui-monospace, monospace; }
.logs-table :deep(td) { vertical-align: top; }
</style>
