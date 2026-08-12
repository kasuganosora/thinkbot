<template>
  <div v-if="map && map[activePlatform]" class="rhythm-wrap" data-testid="bot-rhythm">
    <div class="rh-note">
      Web 渠道不参与聊天节奏控制（始终即时回复）。节奏仅对 Telegram / Misskey 生效，且按会话类型细分：
      <b>单聊默认关闭</b>（即时回复），<b>群聊 / 频道默认开启</b>（受控不刷屏）。
    </div>

    <!-- 平台 tab -->
    <div class="rh-tabs">
      <div
        v-for="p in platforms"
        :key="p.key"
        class="rh-tab"
        :class="{ active: activePlatform === p.key }"
        @click="activePlatform = p.key"
      >{{ p.label }}</div>
    </div>

    <!-- 平台总开关 -->
    <div class="rh-top">
      <div>
        <div class="rh-top-title">启用「{{ activePlatformLabel }}」聊天节奏</div>
        <div class="rh-top-desc">关闭后该平台所有会话类型均不应用节奏控制</div>
      </div>
      <t-switch v-model="platformCfg.enabled" size="large" />
    </div>

    <template v-if="platformCfg.enabled">
      <div v-for="ct in chatTypes" :key="ct.key" class="rh-card">
        <div class="rh-row">
          <div>
            <div class="rh-card-title">{{ ct.label }}</div>
            <div class="rh-card-desc">
              {{ ct.key === 'private' ? '关闭 = 即时回复（推荐）；开启 = 受节奏控制' : '建议开启：避免刷屏' }}
            </div>
          </div>
          <t-switch v-model="platformCfg[ct.key].enabled" size="large" />
        </div>

        <template v-if="platformCfg[ct.key].enabled">
          <div class="rh-card-desc" style="margin-top: 12px">
            发言倾向（0.01 = 安静，1.0 = 回复所有消息）
          </div>
          <div class="rh-slider">
            <t-slider
              v-model="platformCfg[ct.key].speakTendency"
              :min="0.01"
              :max="1"
              :step="0.01"
              style="flex: 1"
            />
            <span class="rh-slider-val">{{ platformCfg[ct.key].speakTendency.toFixed(2) }}</span>
          </div>

          <div class="rh-grid2" style="margin-top: 14px">
            <div class="rh-field">
              <label>防抖静默等待（秒）</label>
              <t-input-number
                v-model="platformCfg[ct.key].debounce.quietWait"
                :min="0"
                theme="normal"
                style="width: 100%"
              />
            </div>
            <div class="rh-field">
              <label>连续发言上限（0 = 不限）</label>
              <t-input-number
                v-model="platformCfg[ct.key].interrupt.maxConsecutive"
                :min="0"
                theme="normal"
                style="width: 100%"
              />
            </div>
          </div>
        </template>
      </div>
    </template>

    <div class="rh-footer">
      <t-button theme="primary" @click="save">保存设置（{{ activePlatformLabel }}）</t-button>
    </div>
  </div>
</template>

<script setup>
import { ref, onMounted, computed } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { botRhythmApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const map = ref({})
const activePlatform = ref('telegram')

const platforms = [
  { key: 'telegram', label: 'Telegram' },
  { key: 'misskey', label: 'Misskey' }
]
const chatTypes = [
  { key: 'private', label: '单聊（1对1）' },
  { key: 'group', label: '群聊 / 超级群' },
  { key: 'channel', label: '频道' }
]

const platformCfg = computed(() => map.value[activePlatform.value])
const activePlatformLabel = computed(
  () => (platforms.find((p) => p.key === activePlatform.value) || {}).label || activePlatform.value
)

async function load() {
  map.value = await botRhythmApi.get(props.botId)
  if (!map.value[activePlatform.value]) {
    map.value[activePlatform.value] = await botRhythmApi.getPlatform(props.botId, activePlatform.value)
  }
}
onMounted(load)

async function save() {
  await botRhythmApi.updatePlatform(props.botId, activePlatform.value, platformCfg.value)
  MessagePlugin.success('聊天节奏已保存（' + activePlatformLabel.value + '）')
}
</script>

<style scoped>
.rhythm-wrap { max-width: 900px; }
.rh-note { font-size: 13px; color: #666; background: #f6f8fa; border: 1px solid #ececec; border-radius: 10px; padding: 12px 14px; margin-bottom: 18px; line-height: 1.6; }
.rh-tabs { display: flex; gap: 8px; margin-bottom: 18px; }
.rh-tab { padding: 8px 18px; border: 1px solid #e3e3e3; border-radius: 999px; cursor: pointer; font-size: 14px; color: #555; background: #fff; }
.rh-tab.active { background: #0052d9; color: #fff; border-color: #0052d9; }
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
