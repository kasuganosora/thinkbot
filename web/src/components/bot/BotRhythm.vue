<template>
  <div v-if="cfg" class="rhythm-wrap" data-testid="bot-rhythm">
    <!-- 总开关 -->
    <div class="rh-top">
      <div>
        <div class="rh-top-title">启用智能聊天节奏</div>
        <div class="rh-top-desc">控制机器人在群聊中何时以及如何回复（防抖、时机判断、发言值、中断）</div>
      </div>
      <t-switch v-model="cfg.enabled" size="large" />
    </div>

    <template v-if="cfg.enabled">
      <!-- 消息防抖 -->
      <div class="rh-card">
        <div class="rh-card-title">消息防抖</div>
        <div class="rh-grid2">
          <div class="rh-field">
            <label>静默等待（秒）</label>
            <t-input-number v-model="cfg.debounce.quietWait" :min="0" theme="normal" style="width:100%" />
          </div>
          <div class="rh-field">
            <label>最大等待（秒）</label>
            <t-input-number v-model="cfg.debounce.maxWait" :min="0" theme="normal" style="width:100%" />
          </div>
        </div>
      </div>

      <!-- 时机判断 -->
      <div class="rh-card rh-row">
        <div>
          <div class="rh-card-title">时机判断</div>
          <div class="rh-card-desc">使用 LLM 判断机器人是否应立即回复或等待</div>
        </div>
        <t-switch v-model="cfg.timing.enabled" size="large" />
      </div>

      <!-- 发言倾向 -->
      <div class="rh-card">
        <div class="rh-card-title">发言倾向</div>
        <div class="rh-card-desc">机器人在群聊中的活跃程度（0.01 = 安静，1.0 = 回复所有消息）</div>
        <div class="rh-slider">
          <t-slider v-model="cfg.speakTendency" :min="0.01" :max="1" :step="0.01" style="flex:1" />
          <span class="rh-slider-val">{{ cfg.speakTendency.toFixed(2) }}</span>
        </div>
      </div>

      <!-- 计划中断 -->
      <div class="rh-card">
        <div class="rh-row">
          <div>
            <div class="rh-card-title">计划中断</div>
            <div class="rh-card-desc">收到新的相关消息时取消正在进行的回复</div>
          </div>
          <t-switch v-model="cfg.interrupt.enabled" size="large" />
        </div>
        <div v-if="cfg.interrupt.enabled" class="rh-grid2" style="margin-top:14px">
          <div class="rh-field">
            <label>最大连续中断次数</label>
            <t-input-number v-model="cfg.interrupt.maxConsecutive" :min="0" theme="normal" style="width:100%" />
          </div>
          <div class="rh-field">
            <label>最大中断轮次</label>
            <t-input-number v-model="cfg.interrupt.maxRounds" :min="0" theme="normal" style="width:100%" />
          </div>
        </div>
      </div>

      <!-- 空闲补偿 -->
      <div class="rh-card">
        <div class="rh-row">
          <div>
            <div class="rh-card-title">空闲补偿</div>
            <div class="rh-card-desc">在长时间沉默后给予机器人更多发言机会</div>
          </div>
          <t-switch v-model="cfg.idleComp.enabled" size="large" />
        </div>
        <div v-if="cfg.idleComp.enabled" class="rh-grid2" style="margin-top:14px">
          <div class="rh-field">
            <label>空闲窗口（分钟）</label>
            <t-input-number v-model="cfg.idleComp.idleWindow" :min="0" theme="normal" style="width:100%" />
          </div>
          <div class="rh-field">
            <label>最低空闲时长（分钟）</label>
            <t-input-number v-model="cfg.idleComp.minIdle" :min="0" theme="normal" style="width:100%" />
          </div>
        </div>
      </div>
    </template>

    <div class="rh-footer">
      <t-button theme="primary" @click="save">保存设置</t-button>
    </div>
  </div>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { botRhythmApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })
const cfg = ref(null)

async function load() { cfg.value = await botRhythmApi.get(props.botId) }
onMounted(load)

async function save() {
  await botRhythmApi.update(props.botId, cfg.value)
  MessagePlugin.success('聊天节奏已保存')
}
</script>

<style scoped>
.rhythm-wrap { max-width: 900px; }
.rh-top { display: flex; align-items: flex-start; justify-content: space-between; margin-bottom: 22px; }
.rh-top-title { font-size: 15px; font-weight: 600; }
.rh-top-desc { font-size: 13px; color: #888; margin-top: 4px; }
.rh-card { border: 1px solid #ececec; border-radius: 12px; padding: 18px 20px; margin-bottom: 16px; background: #fff; }
.rh-card-title { font-size: 14px; font-weight: 600; }
.rh-card-desc { font-size: 12px; color: #999; margin-top: 4px; }
.rh-row { display: flex; align-items: flex-start; justify-content: space-between; }
.rh-grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; margin-top: 14px; }
.rh-field label { display: block; font-size: 13px; color: #555; margin-bottom: 6px; }
.rh-slider { display: flex; align-items: center; gap: 16px; margin-top: 16px; }
.rh-slider-val { font-size: 14px; color: #333; width: 42px; text-align: right; }
.rh-footer { display: flex; justify-content: flex-end; margin-top: 8px; }
</style>
