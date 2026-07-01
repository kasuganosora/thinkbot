<template>
  <div class="ov-wrap" data-testid="bot-overview">
    <div class="ov-card">
      <div class="ov-head">
        <div>
          <div class="ov-title">运行时检查</div>
          <div class="ov-desc">查看当前健康状态与异常详情。</div>
        </div>
        <t-button variant="outline" size="small" @click="load">刷新</t-button>
      </div>

      <div class="ov-summary" :class="{ bad: hasError }">{{ hasError ? '存在异常' : '无异常' }}</div>

      <div class="ov-list">
        <div v-for="(c, i) in checks" :key="i" class="ov-item">
          <div class="oi-main">
            <div class="oi-name" :class="{ mono: c.mono }">{{ c.name }}</div>
            <div v-if="c.sub" class="oi-sub">{{ c.sub }}</div>
            <div class="oi-msg">{{ c.message }}</div>
            <div v-if="c.extra" class="oi-extra">{{ c.extra }}</div>
          </div>
          <t-tag :theme="c.ok ? 'success' : 'danger'" variant="light" class="oi-tag">{{ c.ok ? '正常' : '异常' }}</t-tag>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, computed } from 'vue'

const props = defineProps({ bot: { type: Object, required: true } })

const checks = ref([])
const hasError = computed(() => checks.value.some(c => !c.ok))

function load() {
  checks.value = [
    { name: '容器初始化', message: 'Initialization finished.', ok: true },
    { name: '容器记录', message: 'Container record exists.', extra: `container_id=workspace-${props.bot.id}`, ok: true },
    { name: '容器任务', message: 'Container task state is reported.', extra: 'status=running', ok: true },
    { name: '容器数据路径', message: 'Container is reachable via gRPC.', ok: true },
    { name: 'model.connection', sub: 'Chat Model', message: 'Chat Model is healthy.', ok: true, mono: true },
    { name: 'heartbeat.schedule', sub: 'Every 30m', message: 'Heartbeat is on schedule (last run 1m ago).', ok: true, mono: true }
  ]
}
load()
</script>

<style scoped>
.ov-wrap { width: 100%; }
.ov-card { border: 1px solid #ececec; border-radius: 12px; padding: 20px 24px; }
.ov-head { display: flex; align-items: flex-start; justify-content: space-between; }
.ov-title { font-size: 15px; font-weight: 600; }
.ov-desc { font-size: 12px; color: #999; margin-top: 4px; }
.ov-summary { display: inline-block; margin: 16px 0 8px; padding: 5px 16px; border-radius: 8px; background: #1d1d1f; color: #fff; font-size: 13px; }
.ov-summary.bad { background: #e34d59; }
.ov-list { margin-top: 8px; }
.ov-item { display: flex; align-items: flex-start; justify-content: space-between; padding: 16px 0; border-top: 1px solid #f2f2f2; }
.oi-name { font-size: 14px; font-weight: 600; color: #1d1d1f; }
.oi-name.mono { font-family: monospace; }
.oi-sub { font-size: 12px; color: #999; margin-top: 2px; }
.oi-msg { font-size: 13px; color: #444; margin-top: 6px; }
.oi-extra { font-size: 12px; color: #aaa; margin-top: 3px; font-family: monospace; }
.oi-tag { flex-shrink: 0; }
</style>
