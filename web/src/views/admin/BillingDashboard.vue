<template>
  <div class="billing" data-testid="billing-dashboard">
    <!-- 全局额度配置 -->
    <t-card title="全局额度配置（system.cost_quota）" :bordered="false" class="blk">
      <div class="period-hint">
        计费周期<strong>全局强制一致</strong>：所有 Bot 与功能共享同一 period，计数器据此归零。
      </div>
      <t-form :data="g" label-align="top" class="form">
        <div class="row3">
          <t-form-item label="计费周期">
            <t-select v-model="g.period" :options="periodOptions" style="width: 100%" data-testid="bg-period" />
          </t-form-item>
          <t-form-item label="货币">
            <t-select v-model="g.currency" :options="currencyOptions" filterable creatable style="width: 100%" data-testid="bg-currency" />
          </t-form-item>
          <t-form-item label="全局总预算">
            <div class="inline">
              <t-input-number v-model="g.total" :min="0" :step="100" :decimal-places="2" style="width: 100%" data-testid="bg-total" placeholder="0 = 不限制" />
              <span class="unit">{{ g.currency || 'CNY' }}</span>
            </div>
          </t-form-item>
        </div>
        <t-form-item label="全局按功能预算">
          <div class="feat-block">
            <div class="feat-grid" v-if="gFeatureRows.length">
              <div class="feat-head fk">功能名</div>
              <div class="feat-head fv">预算（{{ g.currency || 'CNY' }}）</div>
              <div class="feat-head fo"></div>
              <div v-for="(row, i) in gFeatureRows" :key="i" class="feat-line">
                <t-select v-model="row.key" :options="featureOptions" filterable creatable placeholder="选择或输入功能名" class="fk" data-testid="bg-feat-key" />
                <t-input-number v-model="row.value" :min="0" :step="10" :decimal-places="2" class="fv" data-testid="bg-feat-value" />
                <t-icon name="close" class="fo op" title="删除" @click="removeGFeature(i)" />
              </div>
            </div>
            <t-empty v-else description="尚未配置全局功能预算" size="small" />
            <t-button variant="outline" size="small" class="feat-add" data-testid="bg-feat-add" @click="addGFeature">
              <template #icon><t-icon name="add" /></template>
              添加全局功能预算
            </t-button>
            <div class="tip">全局功能预算与 Bot 级功能预算独立评估，谁先撞最低墙谁生效（不显式取最小值）。<br />填 dreaming 即覆盖梦境全部阶段（dream_extract / dream_cluster / dream_score），填 memory 同理覆盖 memory_* —— 不必逐阶段配置。</div>
          </div>
        </t-form-item>
      </t-form>
      <div class="footer">
        <t-button theme="primary" :loading="gSaving" data-testid="bg-save" @click="saveGlobal">保存全局配置</t-button>
      </div>
    </t-card>

    <!-- 额度进度总览 -->
    <t-card title="额度进度总览" :bordered="false" class="blk">
      <div class="quota-section">
        <div class="q-title">全局总预算</div>
        <div class="bar-row">
          <div class="bar-track"><div class="bar-fill" :class="sysOver ? 'over' : ''" :style="{ width: sysPct + '%' }" /></div>
          <div class="bar-meta">{{ money(system.usedTotal) }} / {{ sysLimitLabel }}（{{ sysPctTxt }}）</div>
        </div>
      </div>
      <div class="q-list">
        <div v-for="b in bots" :key="b.botId" class="q-item">
          <div class="q-name">{{ b.name || b.botId }}</div>
          <div class="q-body">
            <div class="bar-row">
              <div class="bar-track"><div class="bar-fill" :class="b.progress >= 1 ? 'over' : ''" :style="{ width: botPct(b) + '%' }" /></div>
              <div class="bar-meta">{{ money(b.usedTotal) }} / {{ botLimitLabel(b) }}（{{ botPctTxt(b) }}）</div>
            </div>
          </div>
        </div>
        <t-empty v-if="!bots.length" description="暂无 Bot" size="small" />
      </div>
    </t-card>

    <!-- 用度下钻 -->
    <t-card title="本周期用度下钻" :bordered="false" class="blk">
      <div class="period-pick">
        <span class="pp-label">周期</span>
        <t-select v-model="usagePeriod" :options="periodOptions" style="width: 160px" data-testid="bg-usage-period" @change="loadUsage" />
        <span class="pp-total">合计花费：<strong>{{ money(usage.totalCost) }}</strong></span>
      </div>

      <t-tabs v-model="usageTab" class="usage-tabs">
        <t-tab-panel value="feature" label="按功能">
          <div class="bd-list">
            <div v-for="it in usage.byFeature" :key="it.key" class="bd-row">
              <div class="bd-name">{{ it.key }}</div>
              <div class="bd-bars">
                <div class="bd-bar-track"><div class="bd-bar-fill cost" :style="{ width: featCostPct(it) + '%' }" /></div>
                <div v-if="it.limit > 0" class="bd-bar-track sub">
                  <div class="bd-bar-fill budget" :class="it.progress >= 1 ? 'over' : ''" :style="{ width: Math.min(it.progress * 100, 100) + '%' }" />
                </div>
              </div>
              <div class="bd-meta">
                {{ money(it.cost) }}
                <span class="bd-sub">· {{ it.requests }} 次 · {{ fmtTokens(it.inputTokens + it.outputTokens) }}</span>
                <span v-if="it.limit > 0" class="bd-budget">· 预算 {{ money(it.limit) }}（{{ (it.progress * 100).toFixed(0) }}%）</span>
              </div>
            </div>
            <t-empty v-if="!usage.byFeature.length" description="本周期无功能用度" size="small" />
          </div>
        </t-tab-panel>

        <t-tab-panel value="model" label="按模型">
          <div class="bd-list">
            <div v-for="it in usage.byModel" :key="it.key" class="bd-row">
              <div class="bd-name">{{ it.key || '（未知）' }}</div>
              <div class="bd-bars">
                <div class="bd-bar-track"><div class="bd-bar-fill cost" :style="{ width: modelCostPct(it) + '%' }" /></div>
              </div>
              <div class="bd-meta">
                {{ money(it.cost) }}
                <span class="bd-sub">· {{ it.requests }} 次 · {{ fmtTokens(it.inputTokens + it.outputTokens) }}</span>
              </div>
            </div>
            <t-empty v-if="!usage.byModel.length" description="本周期无模型用度" size="small" />
          </div>
        </t-tab-panel>

        <t-tab-panel value="bot" label="按 Bot">
          <div class="bd-list">
            <div v-for="it in usage.byBot" :key="it.key" class="bd-row">
              <div class="bd-name">{{ botName(it.key) }}</div>
              <div class="bd-bars">
                <div class="bd-bar-track"><div class="bd-bar-fill cost" :style="{ width: botCostPct(it) + '%' }" /></div>
              </div>
              <div class="bd-meta">
                {{ money(it.cost) }}
                <span class="bd-sub">· {{ it.requests }} 次 · {{ fmtTokens(it.inputTokens + it.outputTokens) }}</span>
              </div>
            </div>
            <t-empty v-if="!usage.byBot.length" description="本周期无 Bot 用度" size="small" />
          </div>
        </t-tab-panel>
      </t-tabs>
    </t-card>
  </div>
</template>

<script setup>
import { ref, reactive, computed, onMounted } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { billingApi, configApi } from '@/api/services'

const periodOptions = [
  { label: '日（daily）', value: 'daily' },
  { label: '周（weekly）', value: 'weekly' },
  { label: '月（monthly）', value: 'monthly' }
]
const currencyOptions = [
  { label: 'CNY（人民币）', value: 'CNY' },
  { label: 'USD（美元）', value: 'USD' },
  { label: 'JPY（日元）', value: 'JPY' },
  { label: 'EUR（欧元）', value: 'EUR' }
]

// ---- 全局配置 ----
const g = reactive({ period: 'monthly', currency: 'CNY', total: 0 })
const gFeatureRows = ref([])
const gSaving = ref(false)

// ---- 额度总览 ----
const system = ref({ usedTotal: 0, total: 0, progress: 0, currency: 'CNY' })
const bots = ref([])

// ---- 用度下钻 ----
const usagePeriod = ref('monthly')
const usageTab = ref('feature')
const usage = reactive({ totalCost: 0, byFeature: [], byModel: [], byBot: [] })

const currency = computed(() => g.currency || system.value.currency || 'CNY')
function money(v) {
  const n = Number(v || 0)
  return n.toFixed(2) + ' ' + currency.value
}
function fmtTokens(n) {
  n = Number(n || 0)
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M'
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k'
  return String(n)
}

const sysOver = computed(() => system.value.total > 0 && system.value.usedTotal >= system.value.total)
const sysPct = computed(() => {
  if (!system.value.total) return 0
  return Math.min((system.value.usedTotal / system.value.total) * 100, 100)
})
const sysPctTxt = computed(() => sysPct.value.toFixed(0) + '%')
const sysLimitLabel = computed(() => system.value.total > 0 ? money(system.value.total) : '不限制')

function botPct(b) { return Math.min((b.progress || 0) * 100, 100) }
function botPctTxt(b) { return (botPct(b)).toFixed(0) + '%' }
function botLimitLabel(b) { return (b.total || 0) > 0 ? money(b.total) : '不限制' }
function botName(id) {
  const b = bots.value.find(x => x.botId === id)
  return b?.name || id
}

// feature / model / bot 花费相对最大值的占比（条形图长度）
const maxFeatureCost = computed(() => Math.max(0, ...usage.byFeature.map(x => x.cost)))
const maxModelCost = computed(() => Math.max(0, ...usage.byModel.map(x => x.cost)))
const maxBotCost = computed(() => Math.max(0, ...usage.byBot.map(x => x.cost)))
function featCostPct(it) { return maxFeatureCost.value ? (it.cost / maxFeatureCost.value) * 100 : 0 }
function modelCostPct(it) { return maxModelCost.value ? (it.cost / maxModelCost.value) * 100 : 0 }
function botCostPct(it) { return maxBotCost.value ? (it.cost / maxBotCost.value) * 100 : 0 }

function gRowsToMap() {
  const m = {}
  for (const r of gFeatureRows.value) {
    const k = (r.key || '').trim()
    if (k && (r.value || 0) > 0) m[k] = r.value
  }
  return m
}
function mapToRows(m) {
  return Object.entries(m || {}).map(([k, v]) => ({ key: k, value: v }))
}
// 功能名候选：预设 + 本周期实际出现过的标签。
//
// 后者是必需的 —— 统计表里梦境的标签是 dream_extract 这类**阶段名**，
// 若只给预设，用户要么填错、要么以为预算没生效。把真实出现过的标签列出来，
// 用户既能直接选阶段，也能选 dreaming 这个覆盖全部阶段的组名。
const PRESET_FEATURES = [
  { label: 'reply · 对话回复', value: 'reply' },
  { label: 'dreaming · 梦境（含 dream_* 各阶段）', value: 'dreaming' },
  { label: 'heartbeat · 心跳', value: 'heartbeat' },
  { label: 'cron · 定时任务', value: 'cron' },
  { label: 'memory · 记忆整理（含 memory_*）', value: 'memory' },
  { label: 'subagent · 子代理/工作流', value: 'subagent' }
]
const featureOptions = computed(() => {
  const seen = new Set(PRESET_FEATURES.map((o) => o.value))
  const extra = usage.byFeature
    .map((x) => x.key)
    .filter((k) => k && !seen.has(k) && k !== '(未分类)')
    .map((k) => ({ label: `${k} · 本周期实际出现`, value: k }))
  return [...PRESET_FEATURES, ...extra]
})

function addGFeature() { gFeatureRows.value.push({ key: '', value: 0 }) }
function removeGFeature(i) { gFeatureRows.value.splice(i, 1) }

onMounted(async () => {
  await loadQuotas()
  await loadUsage()
})

async function loadQuotas() {
  try {
    const q = await billingApi.quotas()
    g.period = q.period || 'monthly'
    g.currency = q.currency || 'CNY'
    g.total = q.system?.total || 0
    gFeatureRows.value = mapToRows(q.system?.features)
    system.value = q.system || system.value
    bots.value = q.bots || []
    usagePeriod.value = q.period || 'monthly'
  } catch (e) {
    MessagePlugin.error('加载额度失败：' + (e.message || '请稍后重试'))
  }
}

async function loadUsage() {
  try {
    const u = await billingApi.usage({ period: usagePeriod.value })
    usage.totalCost = u.totalCost || 0
    usage.byFeature = u.byFeature || []
    usage.byModel = u.byModel || []
    usage.byBot = u.byBot || []
    if (u.currency) g.currency = u.currency
  } catch (e) {
    MessagePlugin.error('加载用度失败：' + (e.message || '请稍后重试'))
  }
}

async function saveGlobal() {
  gSaving.value = true
  const payload = JSON.stringify({
    period: g.period || 'monthly',
    currency: g.currency || 'CNY',
    total: g.total || 0,
    features: gRowsToMap()
  })
  try {
    await configApi.set('system.cost_quota', payload)
    MessagePlugin.success('全局配置已保存')
    await loadQuotas()
  } catch (e) {
    MessagePlugin.error('保存失败：' + (e.message || '请稍后重试'))
  } finally {
    gSaving.value = false
  }
}
</script>

<style scoped>
.billing { display: flex; flex-direction: column; gap: 20px; }
.blk { margin-bottom: 0; }
.period-hint {
  font-size: 13px; color: var(--bp-label-tertiary); margin-bottom: 16px;
  background: var(--bp-bg-subtle); border: var(--bp-hairline); border-radius: 10px; padding: 10px 14px;
}
.period-hint strong { color: var(--bp-label); }
.row3 { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 16px; }
.inline { display: flex; align-items: center; gap: 10px; width: 100%; }
.unit { font-size: 13px; color: var(--bp-label-secondary); }

.feat-block { width: 100%; }
.feat-grid { display: grid; grid-template-columns: 1fr 200px 32px; gap: 10px; align-items: center; margin-bottom: 12px; }
.feat-head { font-size: 12px; color: var(--bp-label-tertiary); }
.feat-line { display: contents; }
.feat-line .op { color: var(--bp-label-tertiary); cursor: pointer; justify-self: center; }
.feat-line .op:hover { color: var(--bp-danger); }
.feat-add { margin-bottom: 8px; }
.tip { font-size: 12px; color: var(--bp-label-tertiary); margin-left: 4px; }
.footer { display: flex; gap: 12px; margin-top: 8px; }

/* 额度进度条 */
.q-section, .quota-section { margin-bottom: 8px; }
.q-title { font-size: 14px; font-weight: 600; color: var(--bp-label); margin-bottom: 10px; }
.q-list { display: flex; flex-direction: column; gap: 12px; margin-top: 16px; }
.q-item { display: flex; flex-direction: column; gap: 6px; }
.q-name { font-size: 13px; color: var(--bp-label-secondary); }
.bar-row { display: flex; align-items: center; gap: 12px; }
.bar-track { flex: 1; height: 10px; border-radius: 6px; background: var(--bp-surface-fill); overflow: hidden; }
.bar-fill { height: 100%; border-radius: 6px; background: var(--bp-accent); transition: width 0.3s ease; }
.bar-fill.over { background: var(--bp-danger); }
.bar-meta { font-size: 12px; color: var(--bp-label-tertiary); white-space: nowrap; font-variant-numeric: tabular-nums; }

/* 用度下钻 */
.period-pick { display: flex; align-items: center; gap: 12px; margin-bottom: 14px; }
.pp-label { font-size: 13px; color: var(--bp-label-secondary); }
.pp-total { font-size: 13px; color: var(--bp-label-tertiary); margin-left: auto; }
.pp-total strong { color: var(--bp-label); }
.usage-tabs { margin-top: 4px; }
.bd-list { display: flex; flex-direction: column; gap: 14px; margin-top: 8px; }
.bd-row { display: flex; flex-direction: column; gap: 6px; }
.bd-name { font-size: 13px; font-weight: 600; color: var(--bp-label); }
.bd-bars { display: flex; flex-direction: column; gap: 4px; }
.bd-bar-track { height: 9px; border-radius: 6px; background: var(--bp-surface-fill); overflow: hidden; }
.bd-bar-track.sub { height: 6px; opacity: 0.85; }
.bd-bar-fill { height: 100%; border-radius: 6px; transition: width 0.3s ease; }
.bd-bar-fill.cost { background: var(--bp-accent); }
.bd-bar-fill.budget { background: var(--bp-warning); }
.bd-bar-fill.budget.over { background: var(--bp-danger); }
.bd-meta { font-size: 12px; color: var(--bp-label); font-variant-numeric: tabular-nums; }
.bd-sub { color: var(--bp-label-tertiary); }
.bd-budget { color: var(--bp-warning); margin-left: 6px; }
</style>
