<template>
  <div>
    <t-card title="梦境巩固配置" :bordered="false" class="card">
      <t-loading :loading="loading">
        <t-form label-align="top">
          <t-form-item label="启用梦境巩固">
            <t-switch v-model="config.enabled" data-testid="dreaming-enabled" />
            <span class="tip">定期对长期记忆进行整理、去重与提炼。</span>
          </t-form-item>
          <t-form-item label="调度计划（Cron）">
            <t-input v-model="config.schedule" placeholder="如 0 3 * * *" style="width: 280px" data-testid="dreaming-schedule" />
          </t-form-item>
        </t-form>
        <t-space>
          <t-button theme="primary" @click="save" data-testid="dreaming-save-btn">保存配置</t-button>
          <t-button
            variant="outline"
            :disabled="!config.enabled || triggering"
            :loading="triggering"
            @click="trigger"
            data-testid="dreaming-trigger-btn"
          >{{ triggering ? '巩固进行中…' : '立即触发一次' }}</t-button>
        </t-space>
      </t-loading>
    </t-card>

    <t-alert v-if="triggering" theme="info" class="card running-tip">
      梦境巩固正在后台执行（通常 30 秒 ~ 2 分钟，取决于记忆条数与模型响应），完成后会自动展示结果，请勿关闭页面。
    </t-alert>

    <t-card v-if="status && status.enabled" title="运行状态" :bordered="false" class="card">
      <t-descriptions :column="2" bordered>
        <t-descriptions-item label="运行中">{{ status.running ? '是' : '否' }}</t-descriptions-item>
        <t-descriptions-item label="状态">{{ status.cronJob?.state || '-' }}</t-descriptions-item>
        <t-descriptions-item label="下次运行">{{ formatTime(status.cronJob?.nextRunAt) }}</t-descriptions-item>
        <t-descriptions-item label="上次运行">{{ formatTime(status.cronJob?.lastRunAt) }}</t-descriptions-item>
        <t-descriptions-item label="上次结果">{{ status.cronJob?.lastResult || '-' }}</t-descriptions-item>
        <t-descriptions-item label="累计运行">{{ status.cronJob?.runCount ?? 0 }} 次</t-descriptions-item>
      </t-descriptions>
    </t-card>

    <t-card v-if="lastTrigger" title="最近一次巩固结果" :bordered="false" class="card">
      <t-descriptions :column="3" bordered size="small">
        <t-descriptions-item label="浅层摄入">{{ lastTrigger.lightIngested }}</t-descriptions-item>
        <t-descriptions-item label="去重">{{ lastTrigger.lightDeduped }}</t-descriptions-item>
        <t-descriptions-item label="丢弃">{{ lastTrigger.lightDropped }}</t-descriptions-item>
        <t-descriptions-item label="REM 主题">{{ lastTrigger.remThemes }}</t-descriptions-item>
        <t-descriptions-item label="深层评分">{{ lastTrigger.deepScored }}</t-descriptions-item>
        <t-descriptions-item label="深层晋升">{{ lastTrigger.deepPromoted }}</t-descriptions-item>
        <t-descriptions-item label="耗时">{{ lastTrigger.duration }}</t-descriptions-item>
        <t-descriptions-item label="阶段">{{ lastTrigger.phase }}</t-descriptions-item>
      </t-descriptions>
    </t-card>
  </div>
</template>

<script setup>
import { ref, watch } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { dreamingApi } from '@/api/services'
import { formatTime } from '@/utils/format'

const props = defineProps({ botId: { type: String, required: true } })

const loading = ref(false)
const triggering = ref(false)
const config = ref({ enabled: false, schedule: '0 3 * * *' })
const status = ref(null)
const lastTrigger = ref(null)

async function load() {
  loading.value = true
  try {
    config.value = await dreamingApi.getConfig(props.botId)
    status.value = await dreamingApi.status(props.botId)
  } finally {
    loading.value = false
  }
}
watch(() => props.botId, load, { immediate: true })

async function save() {
  await dreamingApi.updateConfig(props.botId, { enabled: config.value.enabled, schedule: config.value.schedule })
  MessagePlugin.success('梦境配置已保存，需重启 Bot 才生效')
  status.value = await dreamingApi.status(props.botId)
}

async function trigger() {
  if (triggering.value) return
  // 后端是同步执行整条梦境管线（实测 30s ~ 2min），期间必须给出明确反馈，
  // 否则按钮看起来"点了没反应"（踩过一次）。
  triggering.value = true
  const tip = MessagePlugin.info('梦境巩固已开始，正在整理记忆，完成后会显示结果…', 0)
  try {
    lastTrigger.value = await dreamingApi.trigger(props.botId)
    MessagePlugin.success('梦境巩固完成')
    status.value = await dreamingApi.status(props.botId)
    await load()
  } catch (e) {
    MessagePlugin.error('触发失败：' + (e.message || '请稍后重试'))
  } finally {
    triggering.value = false
    tip && tip.close && tip.close()
  }
}
</script>

<style scoped>
.card { margin-bottom: 20px; }
.tip { margin-left: 12px; color: var(--bp-label-tertiary); font-size: 13px; }
</style>
