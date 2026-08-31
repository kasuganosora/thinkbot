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

      <div v-if="backend && !isSandbox" class="ov-warn" data-testid="ov-realenv-warn">
        ⚠️ 未启用沙箱隔离：当前运行于<b>真实环境</b>（backend={{ backend }}），命令与文件操作将直接作用于宿主机，可能影响本机数据。请谨慎操作。
      </div>

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
import { botApi } from '@/api/services'
import { MessagePlugin } from 'tdesign-vue-next'

const props = defineProps({ bot: { type: Object, required: true } })

const checks = ref([])
const backend = ref('')
const loading = ref(false)
const hasError = computed(() => checks.value.some(c => !c.ok))
// docker = 沙箱隔离；其余（如 local）视为真实环境
const isSandbox = computed(() => backend.value === 'docker')

async function load() {
  loading.value = true
  try {
    // 容器相关检查项来自后端真实 sandbox 状态
    const resp = await botApi.runtimeChecks(props.bot.id)
    backend.value = resp?.backend || ''
    const containerChecks = (resp?.checks || []).map(c => ({
      name: c.name,
      sub: c.sub,
      message: c.message,
      extra: c.extra,
      ok: !!c.ok,
      mono: !!c.mono
    }))
    checks.value = containerChecks
  } catch (e) {
    checks.value = [
      { name: '运行时检查', message: '无法获取容器状态：' + (e?.message || e), ok: false }
    ]
    MessagePlugin.error('获取运行时检查失败')
  } finally {
    loading.value = false
  }
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
.ov-warn { margin: 4px 0 12px; padding: 10px 14px; border: 1px solid #f5c6cb; border-radius: 8px; background: #fdecea; color: #d93026; font-size: 13px; line-height: 1.6; }
.ov-warn b { color: #b71c1c; }
.ov-list { margin-top: 8px; }
.ov-item { display: flex; align-items: flex-start; justify-content: space-between; padding: 16px 0; border-top: 1px solid #f2f2f2; }
.oi-name { font-size: 14px; font-weight: 600; color: #1d1d1f; }
.oi-name.mono { font-family: monospace; }
.oi-sub { font-size: 12px; color: #999; margin-top: 2px; }
.oi-msg { font-size: 13px; color: #444; margin-top: 6px; }
.oi-extra { font-size: 12px; color: #aaa; margin-top: 3px; font-family: monospace; }
.oi-tag { flex-shrink: 0; }
</style>
