<template>
  <SettingsShell title="系统监控">
    <div class="toolbar">
      <t-button
        theme="primary"
        variant="outline"
        size="small"
        :loading="loading"
        data-testid="sys-refresh-btn"
        @click="loadAll"
      >
        <template #icon><t-icon name="refresh" /></template>
        刷新
      </t-button>
    </div>

    <t-loading :loading="loading">
      <t-card title="健康状态" :bordered="false" class="card">
        <t-descriptions :column="2" bordered size="small" data-testid="sys-health">
          <t-descriptions-item label="状态">
            <t-tag :theme="health.status === 'ok' ? 'success' : 'danger'" variant="light">{{ health.status || '-' }}</t-tag>
          </t-descriptions-item>
          <t-descriptions-item label="主机">{{ health.host || '-' }}</t-descriptions-item>
          <t-descriptions-item label="运行时长">{{ health.uptime || '-' }}</t-descriptions-item>
          <t-descriptions-item label="Goroutines">{{ health.goroutines ?? '-' }}</t-descriptions-item>
          <t-descriptions-item label="Go 版本">{{ health.goVersion || '-' }}</t-descriptions-item>
          <t-descriptions-item label="运行中 Bot 数">{{ health.bots?.running ?? '-' }}</t-descriptions-item>
        </t-descriptions>
      </t-card>

      <t-card title="内存指标" :bordered="false" class="card">
        <t-descriptions :column="2" bordered size="small" data-testid="sys-memory">
          <t-descriptions-item label="已分配 (MB)">{{ health.memory?.allocMB ?? '-' }}</t-descriptions-item>
          <t-descriptions-item label="累计分配 (MB)">{{ health.memory?.totalAllocMB ?? '-' }}</t-descriptions-item>
          <t-descriptions-item label="系统占用 (MB)">{{ health.memory?.sysMB ?? '-' }}</t-descriptions-item>
          <t-descriptions-item label="GC 次数">{{ health.memory?.gcCount ?? '-' }}</t-descriptions-item>
        </t-descriptions>
      </t-card>

      <t-card title="事件总线指标" :bordered="false" class="card">
        <t-descriptions :column="2" bordered size="small" data-testid="sys-events">
          <t-descriptions-item label="启用状态">
            <t-tag :theme="events.enabled ? 'success' : 'default'" variant="light">{{ events.enabled ? '已启用' : '未启用' }}</t-tag>
          </t-descriptions-item>
          <t-descriptions-item label="活跃订阅数">{{ events.activeSubscriptions ?? '-' }}</t-descriptions-item>
          <t-descriptions-item label="最新序号">{{ events.latestSeq ?? '-' }}</t-descriptions-item>
          <t-descriptions-item label="已发布">{{ events.metrics?.published ?? '-' }}</t-descriptions-item>
          <t-descriptions-item label="已丢弃">{{ events.metrics?.dropped ?? '-' }}</t-descriptions-item>
        </t-descriptions>
      </t-card>
    </t-loading>
  </SettingsShell>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import SettingsShell from '@/components/SettingsShell.vue'
import { systemApi } from '@/api/services'

const loading = ref(false)
const health = ref({})
const events = ref({})

async function loadAll() {
  loading.value = true
  try {
    const [h, e] = await Promise.all([systemApi.health(), systemApi.eventMetrics()])
    health.value = h
    events.value = e
  } catch (err) {
    MessagePlugin.error(`加载系统监控数据失败：${err.message || err}`)
  } finally {
    loading.value = false
  }
}

onMounted(loadAll)
</script>

<style scoped>
.toolbar {
  display: flex;
  justify-content: flex-end;
  margin-bottom: 16px;
}
.card {
  margin-bottom: 20px;
}
</style>
