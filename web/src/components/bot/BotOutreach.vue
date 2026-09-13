<template>
  <div class="ou-wrap" data-testid="bot-outreach">
    <div class="ou-config">
      <div class="cfg-row">
        <div class="cfg-label">
          <div class="cl-title">启用主动开口</div>
          <div class="cl-sub">定时轮询到期的提醒 / 盯梢。能不能发由规则和配额决定，模型只负责措辞。默认关闭。</div>
        </div>
        <t-switch v-model="cfg.enabled" size="large" />
      </div>

      <div class="cfg-field" style="max-width: 280px; margin-bottom: 20px">
        <label class="lbl">轮询间隔（分钟）</label>
        <t-input-number v-model="cfg.interval_min" :min="1" :max="1440" :step="1" theme="normal" style="width: 100%" />
        <div class="field-hint">Bot 级定时器，各平台共用。改间隔后需重启 Bot 才会换 cron 周期。</div>
      </div>

      <div class="plat-grid">
        <div v-for="p in platforms" :key="p.key" class="plat-card" :class="{ off: !cfg.platforms[p.key].enabled }">
          <div class="plat-head">
            <div>
              <div class="plat-name">{{ p.label }}</div>
              <div class="plat-hint">{{ p.hint }}</div>
            </div>
            <t-switch v-model="cfg.platforms[p.key].enabled" size="large" />
          </div>
          <template v-if="cfg.platforms[p.key].enabled">
            <div class="plat-fields">
              <div class="cfg-field">
                <label class="lbl">每用户每天上限</label>
                <t-input-number v-model="cfg.platforms[p.key].max_per_user_per_day" :min="1" :max="20" theme="normal" style="width: 100%" />
                <div class="field-hint">只约束软条件（盯梢）。</div>
              </div>
              <div class="cfg-field">
                <label class="lbl">静默窗（小时）</label>
                <t-input-number v-model="cfg.platforms[p.key].quiet_hours" :min="0" :max="72" :step="0.5" theme="normal" style="width: 100%" />
                <div class="field-hint">距该平台上次互动不足此时长，软条件不发。</div>
              </div>
            </div>
            <div class="plat-toggles">
              <div class="toggle-row">
                <span>硬条件绕过静默窗</span>
                <t-switch v-model="cfg.platforms[p.key].hard_bypass_quiet" />
              </div>
              <div class="toggle-row">
                <span>硬条件绕过每日上限</span>
                <t-switch v-model="cfg.platforms[p.key].hard_bypass_daily_cap" />
              </div>
            </div>
          </template>
          <div class="plat-note">
            若该平台在「工具权限 → 渠道发言」不是「可发言」，这里打开也不会发出去。
          </div>
        </div>
      </div>

      <div class="cfg-foot">
        <t-button theme="default" :loading="saving" @click="saveConfig">保存设置</t-button>
      </div>
    </div>

    <t-divider />

    <div class="ou-pending">
      <div class="logs-head">
        <span class="logs-title">待办承诺</span>
        <t-button variant="outline" size="small" :loading="loadingPending" @click="loadPending">刷新</t-button>
      </div>
      <t-table :data="pending" :columns="pendingCols" row-key="id" size="medium" :bordered="false" empty="没有未到期的提醒 / 盯梢">
        <template #kind="{ row }">{{ row.kind === 'watch' ? '盯梢' : '提醒' }}</template>
        <template #dueAt="{ row }">{{ formatTime(row.dueAt) }}</template>
        <template #op="{ row }">
          <t-button variant="text" theme="danger" size="small" @click="cancelPending(row)">取消</t-button>
        </template>
      </t-table>
    </div>

    <t-divider />

    <div class="ou-logs">
      <div class="logs-head">
        <span class="logs-title">对账日志</span>
        <div class="logs-actions">
          <t-select v-model="logPlatform" :options="platformFilter" size="small" style="width: 130px" @change="loadLogs" />
          <t-select v-model="logStatus" :options="statusFilter" size="small" style="width: 160px" @change="loadLogs" />
          <t-button variant="outline" size="small" :loading="loadingLogs" @click="loadLogs">刷新</t-button>
        </div>
      </div>
      <t-table :data="logs" :columns="logCols" row-key="id" size="medium" :bordered="false" empty="暂无记录">
        <template #status="{ row }">
          <span class="st-tag" :class="statusMeta(row.status).cls">{{ statusMeta(row.status).label }}</span>
        </template>
        <template #createdAt="{ row }">{{ formatTime(row.createdAt) }}</template>
        <template #reason="{ row }">
          <div class="result-cell">{{ row.reason }}</div>
          <div v-if="row.content" class="trace-cell">{{ row.content }}</div>
          <div v-if="row.traceId" class="trace-cell">trace: {{ row.traceId }}</div>
        </template>
      </t-table>
    </div>
  </div>
</template>

<script setup>
import { reactive, ref } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { outreachApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

function emptyPlat() {
  return { enabled: false, max_per_user_per_day: 1, quiet_hours: 6, hard_bypass_quiet: true, hard_bypass_daily_cap: true }
}

const cfg = reactive({
  enabled: false,
  interval_min: 30,
  platforms: { web: emptyPlat(), telegram: emptyPlat(), misskey: emptyPlat() }
})

const platforms = [
  { key: 'web', label: 'Web', hint: '写入该用户的会话历史' },
  { key: 'telegram', label: 'Telegram', hint: '发到已有会话，不主动进陌生群' },
  { key: 'misskey', label: 'Misskey', hint: '仅 specified 私信，不发时间线' }
]

const saving = ref(false)
const pending = ref([])
const loadingPending = ref(false)
const logs = ref([])
const loadingLogs = ref(false)
const logStatus = ref('all')
const logPlatform = ref('')

const statusFilter = [
  { label: '全部', value: 'all' },
  { label: '已发送', value: 'sent' },
  { label: '静默', value: 'silent' },
  { label: '配额跳过', value: 'skipped_quota' },
  { label: '静默窗', value: 'skipped_quiet' },
  { label: '平台关闭', value: 'skipped_platform_disabled' },
  { label: '发言模式', value: 'skipped_speak_mode' },
  { label: '错误', value: 'error' }
]
const platformFilter = [
  { label: '全部平台', value: '' },
  { label: 'Web', value: 'web' },
  { label: 'Telegram', value: 'telegram' },
  { label: 'Misskey', value: 'misskey' }
]

const STATUS_LABELS = {
  sent: { label: '已发送', cls: 'acted' },
  silent: { label: '静默', cls: 'silent' },
  skipped_quota: { label: '配额', cls: 'suppressed' },
  skipped_quiet: { label: '静默窗', cls: 'suppressed' },
  skipped_platform_disabled: { label: '平台关', cls: 'suppressed' },
  skipped_speak_mode: { label: '发言模式', cls: 'suppressed' },
  skipped_no_target: { label: '无目标', cls: 'suppressed' },
  error: { label: '错误', cls: 'error' }
}
function statusMeta(s) {
  return STATUS_LABELS[s] || { label: s || '未知', cls: 'silent' }
}

const pendingCols = [
  { colKey: 'kind', title: '类型', width: 80 },
  { colKey: 'topic', title: '事项' },
  { colKey: 'channelType', title: '平台', width: 110 },
  { colKey: 'dueAt', title: '到期', width: 180 },
  { colKey: 'op', title: '', width: 80 }
]
const logCols = [
  { colKey: 'status', title: '状态', width: 110 },
  { colKey: 'channelType', title: '平台', width: 100 },
  { colKey: 'createdAt', title: '时间', width: 180 },
  { colKey: 'reason', title: '原因 / 内容' }
]

function formatTime(v) {
  if (!v) return '—'
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return String(v)
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}

function applyPlat(name, src) {
  const d = emptyPlat()
  if (!src || typeof src !== 'object') return d
  if (typeof src.enabled === 'boolean') d.enabled = src.enabled
  if (typeof src.max_per_user_per_day === 'number') d.max_per_user_per_day = src.max_per_user_per_day
  if (typeof src.quiet_hours === 'number') d.quiet_hours = src.quiet_hours
  if (typeof src.hard_bypass_quiet === 'boolean') d.hard_bypass_quiet = src.hard_bypass_quiet
  if (typeof src.hard_bypass_daily_cap === 'boolean') d.hard_bypass_daily_cap = src.hard_bypass_daily_cap
  return d
}

async function loadConfig() {
  try {
    const c = await outreachApi.getConfig(props.botId)
    cfg.enabled = !!c.enabled
    if (typeof c.interval_min === 'number') cfg.interval_min = c.interval_min
    const plats = c.platforms || {}
    cfg.platforms.web = applyPlat('web', plats.web)
    cfg.platforms.telegram = applyPlat('telegram', plats.telegram)
    cfg.platforms.misskey = applyPlat('misskey', plats.misskey)
  } catch (e) {
    MessagePlugin.error(`加载主动开口配置失败：${e.message || e}`)
  }
}

async function saveConfig() {
  saving.value = true
  try {
    await outreachApi.updateConfig(props.botId, {
      enabled: cfg.enabled,
      interval_min: cfg.interval_min,
      platforms: {
        web: { ...cfg.platforms.web },
        telegram: { ...cfg.platforms.telegram },
        misskey: { ...cfg.platforms.misskey }
      }
    })
    MessagePlugin.success('设置已保存')
  } catch (e) {
    MessagePlugin.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

async function loadPending() {
  loadingPending.value = true
  try {
    const res = await outreachApi.listCommitments(props.botId)
    pending.value = res.commitments || []
  } catch (e) {
    MessagePlugin.error(`加载承诺失败：${e.message || e}`)
  } finally {
    loadingPending.value = false
  }
}

function cancelPending(row) {
  const dlg = DialogPlugin.confirm({
    header: '取消承诺',
    body: `取消「${row.topic}」？到期后将不再主动开口。`,
    theme: 'warning',
    onConfirm: async () => {
      try {
        await outreachApi.cancelCommitment(props.botId, row.id)
        dlg.destroy()
        MessagePlugin.success('已取消')
        await loadPending()
      } catch (e) {
        MessagePlugin.error(e.message || '取消失败')
      }
    }
  })
}

async function loadLogs() {
  loadingLogs.value = true
  try {
    const res = await outreachApi.listLogs(props.botId, logStatus.value, logPlatform.value)
    logs.value = res.logs || []
  } catch (e) {
    MessagePlugin.error(`加载日志失败：${e.message || e}`)
  } finally {
    loadingLogs.value = false
  }
}

loadConfig()
loadPending()
loadLogs()
</script>

<style scoped>
.ou-wrap { width: 100%; }
.cfg-row { display: flex; align-items: flex-start; justify-content: space-between; gap: 24px; margin-bottom: 20px; }
.cl-title { font-size: 15px; font-weight: 600; color: var(--bp-label); }
.cl-sub { font-size: 13px; color: var(--bp-label-tertiary); margin-top: 4px; max-width: 560px; }
.cfg-field { display: flex; flex-direction: column; }
.lbl { font-size: 13px; font-weight: 600; color: var(--bp-label); margin-bottom: 8px; }
.field-hint { font-size: 12px; color: var(--bp-label-tertiary); margin-top: 6px; line-height: 1.5; }
.cfg-foot { display: flex; justify-content: flex-end; margin-top: 20px; }

.plat-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 16px; }
.plat-card {
  border: 1px solid var(--bp-separator);
  border-radius: 12px;
  padding: 16px;
  background: var(--bp-surface);
}
.plat-card.off { opacity: 0.72; }
.plat-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; margin-bottom: 12px; }
.plat-name { font-size: 15px; font-weight: 600; color: var(--bp-label); }
.plat-hint { font-size: 12px; color: var(--bp-label-tertiary); margin-top: 4px; }
.plat-fields { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.plat-toggles { margin-top: 12px; display: flex; flex-direction: column; gap: 8px; }
.toggle-row { display: flex; justify-content: space-between; align-items: center; font-size: 13px; color: var(--bp-label); }
.plat-note { margin-top: 12px; font-size: 12px; color: var(--bp-label-tertiary); line-height: 1.5; }

.logs-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 16px; }
.logs-title { font-size: 15px; font-weight: 600; color: var(--bp-label); }
.logs-actions { display: flex; align-items: center; gap: 10px; }

.st-tag {
  display: inline-flex; align-items: center; justify-content: center;
  font-size: 12px; font-weight: 600; padding: 4px 12px; border-radius: 8px;
}
.st-tag.acted      { background: var(--bp-success-soft); color: var(--bp-success); }
.st-tag.silent     { background: var(--bp-surface-fill); color: var(--bp-label-tertiary); }
.st-tag.suppressed { background: var(--bp-warning-soft); color: var(--bp-warning); }
.st-tag.error      { background: var(--bp-danger-soft); color: var(--bp-danger); }
.result-cell { font-size: 13px; color: var(--bp-label); line-height: 1.6; white-space: pre-wrap; word-break: break-word; }
.trace-cell { font-size: 11px; color: var(--bp-label-quaternary); margin-top: 4px; font-family: ui-monospace, monospace; }
</style>
