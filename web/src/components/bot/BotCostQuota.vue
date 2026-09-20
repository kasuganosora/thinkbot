<template>
  <div class="cq" data-testid="bot-cost-quota">
    <t-card title="额度（金钱预算）" :bordered="false" class="card">
      <div class="period-hint">
        计费周期由全局「系统设置 → 计费看板」统一设定，当前为
        <strong>{{ periodLabel }}</strong>，本页仅配置本 Bot 的预算。
      </div>

      <t-form :data="form" label-align="top" class="form">
        <div class="row2">
          <t-form-item label="启用额度限制">
            <t-switch v-model="form.enabled" data-testid="cq-enabled" />
          </t-form-item>
          <t-form-item label="货币">
            <t-select v-model="form.currency" :options="currencyOptions" filterable creatable style="width: 100%" data-testid="cq-currency" />
          </t-form-item>
        </div>

        <t-form-item label="Bot 总预算">
          <div class="total-row">
            <t-input-number v-model="form.total" :min="0" :step="10" :decimal-places="2" style="width: 220px" data-testid="cq-total" placeholder="0 = 不限制" />
            <span class="unit">{{ form.currency || 'CNY' }}</span>
            <span class="tip">本周期内该 Bot 全部功能合计可用上限，0 表示不限制。</span>
          </div>
        </t-form-item>

        <t-form-item label="按功能预算">
          <div class="feat-block">
            <div class="feat-grid" v-if="featureRows.length">
              <div class="feat-head fk">功能名</div>
              <div class="feat-head fv">预算（{{ form.currency || 'CNY' }}）</div>
              <div class="feat-head fo"></div>
              <div
                v-for="(row, i) in featureRows"
                :key="i"
                class="feat-line"
              >
                <t-input v-model="row.key" placeholder="如 dreaming / heartbeat / reply" class="fk" data-testid="cq-feat-key" />
                <t-input-number v-model="row.value" :min="0" :step="10" :decimal-places="2" class="fv" data-testid="cq-feat-value" />
                <t-icon name="close" class="fo op" title="删除" @click="removeFeature(i)" />
              </div>
            </div>
            <t-empty v-else description="尚未配置按功能预算" size="small" />
            <t-button variant="outline" size="small" class="feat-add" data-testid="cq-feat-add" @click="addFeature">
              <template #icon><t-icon name="add" /></template>
              添加功能预算
            </t-button>
            <div class="tip">常见功能：reply（对话）、dreaming（梦境）、heartbeat（心跳）、cron（定时任务）。命中任意一堵墙即拦截，先碰最低预算墙者生效。</div>
          </div>
        </t-form-item>
      </t-form>

      <div class="footer">
        <t-button theme="primary" :loading="saving" data-testid="cq-save" @click="save">保存额度</t-button>
      </div>
    </t-card>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted, computed } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { botApi, billingApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const currencyOptions = [
  { label: 'CNY（人民币）', value: 'CNY' },
  { label: 'USD（美元）', value: 'USD' },
  { label: 'JPY（日元）', value: 'JPY' },
  { label: 'EUR（欧元）', value: 'EUR' }
]

const periodMap = { daily: '日', weekly: '周', monthly: '月' }
const period = ref('monthly')
const periodLabel = computed(() => (periodMap[period.value] || '月') + '（' + period.value + '）')

const form = reactive({ enabled: false, currency: 'CNY', total: 0 })
const featureRows = ref([])
const saving = ref(false)

function rowsToMap() {
  const m = {}
  for (const r of featureRows.value) {
    const k = (r.key || '').trim()
    if (k && (r.value || 0) > 0) m[k] = r.value
  }
  return m
}
function mapToRows(m) {
  const rows = []
  for (const [k, v] of Object.entries(m || {})) rows.push({ key: k, value: v })
  return rows
}

onMounted(load)
async function load() {
  try {
    const q = await billingApi.quotas().catch(() => null)
    if (q && q.period) period.value = q.period
  } catch (e) { /* 周期读取失败不影响表单 */ }

  try {
    const b = await botApi.get(props.botId)
    const raw = b?.costQuota || ''
    let cfg = {}
    if (raw) {
      try { cfg = JSON.parse(raw) } catch (e) { cfg = {} }
    }
    form.enabled = !!cfg.enabled
    form.currency = cfg.currency || 'CNY'
    form.total = cfg.total || 0
    featureRows.value = mapToRows(cfg.features)
  } catch (e) {
    MessagePlugin.error('加载额度失败：' + (e.message || '请稍后重试'))
  }
}

function addFeature() { featureRows.value.push({ key: '', value: 0 }) }
function removeFeature(i) { featureRows.value.splice(i, 1) }

async function save() {
  saving.value = true
  const payload = JSON.stringify({
    enabled: form.enabled,
    currency: form.currency || 'CNY',
    total: form.total || 0,
    features: rowsToMap()
  })
  try {
    await botApi.update(props.botId, { costQuota: payload })
    MessagePlugin.success('额度已保存')
  } catch (e) {
    MessagePlugin.error('保存失败：' + (e.message || '请稍后重试'))
  } finally {
    saving.value = false
  }
}
</script>

<style scoped>
.card { margin-bottom: 20px; }
.period-hint {
  font-size: 13px; color: var(--bp-label-tertiary); margin-bottom: 18px;
  background: var(--bp-bg-subtle); border: var(--bp-hairline); border-radius: 10px; padding: 10px 14px;
}
.period-hint strong { color: var(--bp-label); }
.row2 { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
.total-row { display: flex; align-items: center; gap: 10px; }
.unit { font-size: 13px; color: var(--bp-label-secondary); }
.tip { font-size: 12px; color: var(--bp-label-tertiary); margin-left: 4px; }
.feat-block { width: 100%; }
.feat-grid { display: grid; grid-template-columns: 1fr 200px 32px; gap: 10px; align-items: center; margin-bottom: 12px; }
.feat-head { font-size: 12px; color: var(--bp-label-tertiary); }
.feat-line { display: contents; }
.feat-line .op { color: var(--bp-label-tertiary); cursor: pointer; justify-self: center; }
.feat-line .op:hover { color: var(--bp-danger); }
.feat-add { margin-bottom: 8px; }
.footer { display: flex; gap: 12px; margin-top: 8px; }
</style>
