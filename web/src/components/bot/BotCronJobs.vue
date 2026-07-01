<template>
  <div>
    <div class="toolbar">
      <span class="title">定时任务</span>
      <t-space size="small">
        <t-button variant="outline" size="small" @click="load">
          <template #icon><t-icon name="refresh" /></template>
          刷新
        </t-button>
        <t-button theme="primary" size="small" @click="openCreate" data-testid="cron-create-btn">
          <template #icon><t-icon name="add" /></template>
          新建任务
        </t-button>
      </t-space>
    </div>

    <!-- 空状态 -->
    <div v-if="!loading && list.length === 0" class="empty">
      <div class="empty-icon"><t-icon name="calendar" size="28px" /></div>
      <div class="empty-text">暂无定时任务</div>
      <t-button variant="outline" size="small" @click="openCreate">
        <template #icon><t-icon name="add" /></template>
        新建任务
      </t-button>
    </div>

    <!-- 任务列表 -->
    <t-table
      v-else
      :data="list"
      :columns="columns"
      row-key="id"
      :loading="loading"
      data-testid="cron-table"
    >
      <template #enabled="{ row }">
        <t-tag :theme="stateTheme(row.state)" variant="light">{{ stateLabel(row.state) }}</t-tag>
      </template>
      <template #next_run_at="{ row }">{{ formatTime(row.next_run_at) }}</template>
      <template #last_run_at="{ row }">{{ formatTime(row.last_run_at) }}</template>
      <template #op="{ row }">
        <t-space size="small">
          <t-button variant="text" size="small" @click="trigger(row)">立即触发</t-button>
          <t-button v-if="row.enabled" variant="text" theme="warning" size="small" @click="pause(row)">暂停</t-button>
          <t-button v-else variant="text" theme="success" size="small" @click="resume(row)">恢复</t-button>
          <t-button variant="text" theme="primary" size="small" @click="openEdit(row)">编辑</t-button>
          <t-button variant="text" theme="danger" size="small" @click="remove(row)">删除</t-button>
        </t-space>
      </template>
    </t-table>

    <!-- 新建 / 编辑弹窗 -->
    <t-dialog
      v-model:visible="dialog.visible"
      :header="dialog.isEdit ? '编辑任务' : '新建任务'"
      :width="560"
      :confirm-btn="dialog.isEdit ? '保存' : '创建'"
      :on-confirm="submit"
      dialogClassName="cron-dialog"
    >
      <div class="cron-form">
        <!-- 名称 + 启用 -->
        <div class="field">
          <div class="field-row">
            <div class="grow">
              <label class="lbl">名称</label>
              <t-input v-model="form.name" placeholder="例如：每日早报" data-testid="cron-form-name" />
            </div>
            <div class="enable-box">
              <span class="lbl">启用</span>
              <t-switch v-model="form.enabled" />
            </div>
          </div>
        </div>

        <!-- 描述 -->
        <div class="field">
          <label class="lbl">描述 <span class="opt">(可选)</span></label>
          <t-input v-model="form.description" placeholder="这个任务做什么" />
        </div>

        <!-- 指令 -->
        <div class="field">
          <label class="lbl">指令</label>
          <t-textarea
            v-model="form.prompt"
            :autosize="{ minRows: 3, maxRows: 8 }"
            placeholder="每次触发时让 Bot 做什么"
            data-testid="cron-form-prompt"
          />
          <div class="tip">任务触发时，这段内容会作为消息发送给 Bot。</div>
        </div>

        <!-- 调度规则 -->
        <div class="section-title">调度规则</div>

        <div class="field">
          <label class="lbl">模式</label>
          <t-select v-model="form.mode" :options="modeOptions" @change="onModeChange" />
          <div class="tip">{{ modeHint }}</div>
        </div>

        <!-- 周（每周模式） -->
        <div class="field" v-if="form.mode === 'weekly'">
          <label class="lbl">星期</label>
          <div class="tip">选择一个或多个星期。</div>
          <div class="chip-grid week">
            <div
              v-for="d in weekDays"
              :key="d.value"
              class="chip"
              :class="{ active: form.weekdays.includes(d.value) }"
              @click="toggleWeekday(d.value)"
            >{{ d.label }}</div>
          </div>
        </div>

        <!-- 小时（每天 / 每周模式） -->
        <div class="field" v-if="form.mode === 'daily' || form.mode === 'weekly'">
          <label class="lbl">小时</label>
          <div class="tip">点击选择一个或多个小时。</div>
          <div class="chip-grid hours">
            <div
              v-for="h in hours"
              :key="h"
              class="chip"
              :class="{ active: form.hours.includes(h) }"
              @click="toggleHour(h)"
            >{{ pad(h) }}</div>
          </div>
        </div>

        <!-- 分钟（每天 / 每周 / 每小时） -->
        <div class="field" v-if="form.mode !== 'custom'">
          <label class="lbl">分钟</label>
          <t-input-number
            v-model="form.minute"
            :min="0"
            :max="59"
            theme="normal"
            style="width: 100%"
          />
        </div>

        <!-- 间隔（每 N 分钟模式） -->
        <div class="field" v-if="form.mode === 'interval'">
          <label class="lbl">每隔（分钟）</label>
          <t-input-number v-model="form.interval" :min="1" :max="1440" style="width: 100%" />
        </div>

        <!-- 自定义 cron -->
        <div class="field" v-if="form.mode === 'custom'">
          <label class="lbl">Cron 表达式</label>
          <t-input v-model="form.customCron" placeholder="如 0 9 * * 1-5" data-testid="cron-form-schedule" />
          <div class="tip">分 时 日 月 周，标准 5 段 cron。</div>
        </div>

        <!-- Cron 预览卡片 -->
        <div class="cron-preview">
          <div class="cp-label">Cron 表达式</div>
          <div class="cp-expr">{{ cronExpr }}</div>
          <div class="cp-desc">{{ cronDescribe }}</div>
          <div class="cp-divider"></div>
          <div class="cp-next-title">接下来的触发时间（{{ tz }}）</div>
          <ul class="cp-next">
            <li v-for="(t, i) in nextRuns" :key="i">{{ t }}</li>
            <li v-if="nextRuns.length === 0" class="cp-empty">无法解析，请检查规则</li>
          </ul>
        </div>

        <!-- 运行次数限制 -->
        <div class="field">
          <div class="field-row">
            <label class="lbl" style="margin:0">运行次数限制</label>
            <div class="enable-box">
              <t-switch v-model="form.limitRuns" />
              <span class="lbl" style="margin:0">{{ form.limitRuns ? '限制' : '不限制' }}</span>
            </div>
          </div>
          <t-input-number
            v-if="form.limitRuns"
            v-model="form.maxRuns"
            :min="1"
            style="width: 100%; margin-top: 8px"
            placeholder="最多执行次数"
          />
        </div>
      </div>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, reactive, computed } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { cronApi } from '@/api/services'
import { formatTime } from '@/utils/format'

const props = defineProps({ botId: { type: String, required: true } })

const tz = 'Asia/Hong_Kong'
const loading = ref(false)
const list = ref([])

const columns = [
  { colKey: 'name', title: '名称', width: 130 },
  { colKey: 'schedule_display', title: 'Cron', width: 130 },
  { colKey: 'enabled', title: '状态', width: 90 },
  { colKey: 'run_count', title: '执行次数', width: 90 },
  { colKey: 'next_run_at', title: '下次运行', width: 160 },
  { colKey: 'last_run_at', title: '上次运行', width: 160 },
  { colKey: 'op', title: '操作', width: 280, fixed: 'right' }
]

const stateThemes = { active: 'success', paused: 'warning', done: 'default', failed: 'danger', disabled: 'default' }
const stateLabels = { active: '运行中', paused: '已暂停', done: '已完成', failed: '失败', disabled: '已禁用' }
function stateTheme(s) { return stateThemes[s] || 'default' }
function stateLabel(s) { return stateLabels[s] || s }

async function load() {
  loading.value = true
  try {
    const res = await cronApi.list(props.botId)
    list.value = res.jobs || []
  } finally {
    loading.value = false
  }
}
load()

/* ---------------- 弹窗与表单 ---------------- */
const dialog = reactive({ visible: false, isEdit: false, id: null })

const modeOptions = [
  { label: '每天', value: 'daily' },
  { label: '每周', value: 'weekly' },
  { label: '每小时', value: 'hourly' },
  { label: '每 N 分钟', value: 'interval' },
  { label: '自定义 (Cron)', value: 'custom' }
]
const modeHints = {
  daily: '每天在选定的小时触发。',
  weekly: '每周选定的星期、选定的小时触发。',
  hourly: '每小时在选定的分钟触发。',
  interval: '从启用时刻起，每隔固定分钟触发一次。',
  custom: '直接填写标准 cron 表达式。'
}

const hours = Array.from({ length: 24 }, (_, i) => i)
const weekDays = [
  { label: '一', value: 1 }, { label: '二', value: 2 }, { label: '三', value: 3 },
  { label: '四', value: 4 }, { label: '五', value: 5 }, { label: '六', value: 6 }, { label: '日', value: 0 }
]

function defaultForm() {
  return {
    name: '', description: '', prompt: '', enabled: true,
    mode: 'daily', hours: [9], weekdays: [1], minute: 0, interval: 30, customCron: '0 9 * * *',
    limitRuns: false, maxRuns: 1
  }
}
const form = reactive(defaultForm())

function pad(n) { return String(n).padStart(2, '0') }
function resetForm(src) { Object.assign(form, defaultForm(), src || {}) }

function openCreate() {
  dialog.isEdit = false; dialog.id = null
  resetForm()
  dialog.visible = true
}

function openEdit(row) {
  dialog.isEdit = true; dialog.id = row.id
  resetForm(parseSchedule(row))
  dialog.visible = true
}

function toggleHour(h) {
  const i = form.hours.indexOf(h)
  if (i >= 0) form.hours.splice(i, 1); else form.hours.push(h)
  form.hours.sort((a, b) => a - b)
}
function toggleWeekday(d) {
  const i = form.weekdays.indexOf(d)
  if (i >= 0) form.weekdays.splice(i, 1); else form.weekdays.push(d)
  form.weekdays.sort((a, b) => a - b)
}
const modeHint = computed(() => modeHints[form.mode] || '')
function onModeChange() {
  if (form.mode === 'daily' && form.hours.length === 0) form.hours = [9]
  if (form.mode === 'weekly' && form.weekdays.length === 0) form.weekdays = [1]
}

/* ---------------- Cron 计算 ---------------- */
const cronExpr = computed(() => {
  const m = form.minute
  switch (form.mode) {
    case 'daily': {
      const h = form.hours.length ? [...form.hours].sort((a, b) => a - b).join(',') : '*'
      return `${m} ${h} * * *`
    }
    case 'weekly': {
      const h = form.hours.length ? [...form.hours].sort((a, b) => a - b).join(',') : '*'
      const w = form.weekdays.length ? [...form.weekdays].sort((a, b) => a - b).join(',') : '*'
      return `${m} ${h} * * ${w}`
    }
    case 'hourly':
      return `${m} * * * *`
    case 'interval':
      return `*/${form.interval} * * * *`
    case 'custom':
      return (form.customCron || '').trim()
  }
  return ''
})

const weekNames = ['周日', '周一', '周二', '周三', '周四', '周五', '周六']
const cronDescribe = computed(() => {
  const m = form.minute
  switch (form.mode) {
    case 'daily':
      return form.hours.length
        ? `每天 ${form.hours.map(h => `${pad(h)}:${pad(m)}`).join('、')} 触发`
        : '请至少选择一个小时'
    case 'weekly': {
      if (!form.weekdays.length) return '请至少选择一个星期'
      if (!form.hours.length) return '请至少选择一个小时'
      const w = form.weekdays.map(d => weekNames[d]).join('、')
      const h = form.hours.map(h => `${pad(h)}:${pad(m)}`).join('、')
      return `每${w} 的 ${h} 触发`
    }
    case 'hourly':
      return `每小时的第 ${m} 分钟触发`
    case 'interval':
      return `每隔 ${form.interval} 分钟触发`
    case 'custom':
      return cronExpr.value ? '自定义 cron 规则' : '请输入 cron 表达式'
  }
  return ''
})

// 简易 cron 预测：仅支持 minute/hour/weekday 的 数值、逗号列表、* 和 */n
function parseField(field, min, max) {
  if (field === '*' || field === '?') return null // 任意
  const set = new Set()
  for (const part of field.split(',')) {
    const step = part.match(/^\*\/(\d+)$/)
    if (step) {
      const n = +step[1]
      for (let i = min; i <= max; i += n) set.add(i)
    } else if (/^\d+$/.test(part)) {
      set.add(+part)
    } else {
      return undefined // 不支持的语法
    }
  }
  return set
}
const nextRuns = computed(() => {
  const expr = cronExpr.value
  const seg = expr.split(/\s+/)
  if (seg.length !== 5) return []
  const mins = parseField(seg[0], 0, 59)
  const hrs = parseField(seg[1], 0, 23)
  const dows = parseField(seg[4], 0, 6)
  if (mins === undefined || hrs === undefined || dows === undefined) return []
  const out = []
  const now = new Date()
  const cur = new Date(now.getTime())
  cur.setSeconds(0, 0)
  cur.setMinutes(cur.getMinutes() + 1)
  for (let i = 0; i < 60 * 24 * 14 && out.length < 3; i++) {
    const okMin = !mins || mins.has(cur.getMinutes())
    const okHr = !hrs || hrs.has(cur.getHours())
    const okDow = !dows || dows.has(cur.getDay())
    if (okMin && okHr && okDow) {
      out.push(fmtLocal(cur))
    }
    cur.setMinutes(cur.getMinutes() + 1)
  }
  return out
})
function fmtLocal(d) {
  return `${d.getFullYear()}/${pad(d.getMonth() + 1)}/${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

// 反解析（编辑时尽量还原可视化模式）
function parseSchedule(row) {
  const base = {
    id: row.id, name: row.name, description: row.description || '',
    prompt: row.prompt, enabled: row.enabled !== false,
    limitRuns: !!row.max_runs && row.max_runs > 0, maxRuns: row.max_runs || 1,
    customCron: row.schedule || ''
  }
  const seg = (row.schedule || '').trim().split(/\s+/)
  if (seg.length === 5) {
    const [mm, hh, , , dow] = seg
    const interval = mm.match(/^\*\/(\d+)$/)
    if (interval && hh === '*') {
      return { ...base, mode: 'interval', interval: +interval[1] }
    }
    if (hh === '*' && /^\d+$/.test(mm)) {
      return { ...base, mode: 'hourly', minute: +mm }
    }
    const toList = (s) => s.split(',').filter(x => /^\d+$/.test(x)).map(Number)
    if (dow !== '*' && dow !== '?') {
      return { ...base, mode: 'weekly', minute: +mm || 0, hours: toList(hh), weekdays: toList(dow) }
    }
    if (/^[\d,]+$/.test(hh)) {
      return { ...base, mode: 'daily', minute: +mm || 0, hours: toList(hh) }
    }
  }
  return { ...base, mode: 'custom' }
}

/* ---------------- 提交 ---------------- */
async function submit() {
  if (!form.name) return MessagePlugin.warning('请填写任务名称')
  if (!form.prompt) return MessagePlugin.warning('请填写指令')
  const schedule = cronExpr.value
  if (!schedule || schedule.split(/\s+/).length !== 5) return MessagePlugin.warning('调度规则无效，请检查')
  if (form.mode === 'daily' && form.hours.length === 0) return MessagePlugin.warning('请至少选择一个小时')
  if (form.mode === 'weekly' && (form.weekdays.length === 0 || form.hours.length === 0)) return MessagePlugin.warning('请选择星期和小时')

  const payload = {
    name: form.name,
    description: form.description,
    schedule,
    prompt: form.prompt,
    enabled: form.enabled,
    maxRuns: form.limitRuns ? form.maxRuns : 0
  }
  try {
    if (dialog.isEdit) {
      await cronApi.update(props.botId, dialog.id, payload)
      MessagePlugin.success('任务已更新')
    } else {
      await cronApi.create(props.botId, payload)
      MessagePlugin.success('任务已创建')
    }
    dialog.visible = false
    load()
  } catch (e) {
    MessagePlugin.error(e.message || '操作失败')
  }
}

async function trigger(row) { await cronApi.trigger(props.botId, row.id); MessagePlugin.success('已触发执行'); load() }
async function pause(row) { await cronApi.pause(props.botId, row.id); MessagePlugin.success('已暂停'); load() }
async function resume(row) { await cronApi.resume(props.botId, row.id); MessagePlugin.success('已恢复'); load() }

function remove(row) {
  const dlg = DialogPlugin.confirm({
    header: '删除任务', body: `确认删除任务「${row.name}」？`, theme: 'warning',
    onConfirm: async () => { await cronApi.remove(props.botId, row.id); dlg.destroy(); MessagePlugin.success('已删除'); load() }
  })
}
</script>

<style scoped>
.toolbar { display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px; }
.title { font-size: 16px; font-weight: 600; color: #1d1d1f; }

/* 空状态 */
.empty {
  display: flex; flex-direction: column; align-items: center; justify-content: center;
  gap: 12px; padding: 80px 0; color: #999;
}
.empty-icon {
  width: 56px; height: 56px; border-radius: 50%;
  display: flex; align-items: center; justify-content: center;
  background: #f5f5f7; color: #999;
}
.empty-text { font-size: 14px; color: #999; }

/* 表单 */
.cron-form { display: flex; flex-direction: column; gap: 16px; }
.field { display: flex; flex-direction: column; }
.field-row { display: flex; align-items: flex-end; gap: 16px; }
.field-row .grow { flex: 1; }
.enable-box { display: flex; align-items: center; gap: 8px; padding-bottom: 6px; }
.lbl { font-size: 13px; font-weight: 600; color: #1d1d1f; margin-bottom: 6px; }
.opt { font-weight: 400; color: #999; }
.tip { font-size: 12px; color: #999; margin-top: 6px; }
.section-title {
  font-size: 14px; font-weight: 600; color: #1d1d1f;
  border-top: 1px solid #f0f0f0; margin-top: 4px; padding-top: 16px;
}

/* 小时 / 星期 chip */
.chip-grid { display: grid; gap: 8px; margin-top: 8px; }
.chip-grid.hours { grid-template-columns: repeat(8, 1fr); }
.chip-grid.week { grid-template-columns: repeat(7, 1fr); }
.chip {
  height: 36px; display: flex; align-items: center; justify-content: center;
  border: 1px solid #e0e0e0; border-radius: 8px; font-size: 13px; cursor: pointer;
  font-family: 'SF Mono', Menlo, monospace; color: #555; user-select: none;
  transition: all .15s;
}
.chip:hover { border-color: #999; }
.chip.active { background: #2a2a32; color: #fff; border-color: #2a2a32; }

/* cron 预览卡片 */
.cron-preview {
  border: 1px solid #eee; border-radius: 10px; padding: 14px 16px; background: #fafafa;
}
.cp-label { font-size: 13px; color: #555; margin-bottom: 6px; }
.cp-expr { font-family: 'SF Mono', Menlo, monospace; font-size: 15px; color: #1d1d1f; letter-spacing: 1px; }
.cp-desc { font-size: 13px; color: #1d1d1f; margin-top: 8px; }
.cp-divider { height: 1px; background: #eee; margin: 12px 0; }
.cp-next-title { font-size: 13px; color: #888; margin-bottom: 6px; }
.cp-next { list-style: none; padding: 0; margin: 0; }
.cp-next li {
  font-family: 'SF Mono', Menlo, monospace; font-size: 13px; color: #555; padding: 2px 0;
}
.cp-next li::before { content: '· '; color: #aaa; }
.cp-empty { color: #c0392b; }
</style>

<!-- 弹窗在 body 下，scoped 命中不到 -->
<style>
.cron-dialog.t-dialog { padding: 20px 20px 16px; }
.cron-dialog .t-dialog__header { padding: 0; margin-bottom: 16px; }
.cron-dialog .t-dialog__body { padding: 0; }
.cron-dialog .t-dialog__footer { padding: 16px 0 0; }
</style>
