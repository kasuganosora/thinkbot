<template>
  <div class="wrap">
    <t-card title="基础信息" :bordered="false" class="card">
      <div class="avatar-row">
        <div class="bot-avatar-preview">{{ form.avatar }}</div>
        <div class="avatar-pick">
          <span class="pick-label">选择图标</span>
          <div class="emoji-list">
            <span
              v-for="e in emojis"
              :key="e"
              class="emoji"
              :class="{ active: e === form.avatar }"
              @click="form.avatar = e"
            >{{ e }}</span>
          </div>
        </div>
      </div>

      <t-form :data="form" label-align="top" class="form">
        <t-form-item label="Bot 名称">
          <t-input v-model="form.name" placeholder="给 Bot 起个名字" data-testid="bot-name" />
        </t-form-item>
        <t-form-item label="描述">
          <t-input v-model="form.desc" placeholder="一句话介绍这个 Bot" data-testid="bot-desc" />
        </t-form-item>
      </t-form>
    </t-card>

    <t-card title="模型参数" :bordered="false" class="card">
      <t-form :data="form" label-align="top" class="form">
        <div class="row2">
          <t-form-item label="兼容模型（model）">
            <t-select v-model="form.model" :options="modelOptions" filterable creatable data-testid="bot-model" />
          </t-form-item>
          <t-form-item label="推理强度（reasoningEffort）">
            <t-select v-model="form.reasoningEffort" :options="effortOptions" data-testid="bot-effort" />
          </t-form-item>
        </div>
        <div class="row2">
          <t-form-item label="主模型（llmMain）">
            <t-select v-model="form.llmMain" :options="modelOptions" filterable creatable clearable placeholder="主 LLM 模型 ID" data-testid="bot-llm-main" />
          </t-form-item>
          <t-form-item label="轻量模型（llmLight）">
            <t-select v-model="form.llmLight" :options="modelOptions" filterable creatable clearable placeholder="轻量 LLM 模型 ID" data-testid="bot-llm-light" />
          </t-form-item>
        </div>
        <t-form-item label="温度（Temperature）">
          <div class="slider-row">
            <div class="slider-box">
              <t-slider class="temp-slider" v-model="form.temperature" :min="0" :max="2" :step="0.1" />
            </div>
            <span class="slider-val">{{ form.temperature }}</span>
          </div>
        </t-form-item>
        <div class="row2">
          <t-form-item label="最大 Token（maxTokens）">
            <t-input-number v-model="form.maxTokens" :min="1" :step="512" style="width: 100%" data-testid="bot-maxtokens" />
          </t-form-item>
          <t-form-item label="并发 Worker 数（workers）">
            <t-input-number v-model="form.workers" :min="1" :max="64" style="width: 100%" data-testid="bot-workers" />
          </t-form-item>
        </div>
        <t-form-item label="系统提示词（System Prompt）">
          <t-textarea
            v-model="form.systemPrompt"
            :autosize="{ minRows: 5, maxRows: 14 }"
            placeholder="定义 Bot 的角色、风格与约束"
            data-testid="bot-system-prompt"
          />
        </t-form-item>
      </t-form>
    </t-card>

    <div class="footer-actions">
      <t-button theme="primary" @click="$emit('save')" data-testid="bot-save-btn">保存</t-button>
      <t-button
        :theme="running ? 'warning' : 'success'"
        variant="outline"
        :loading="toggling"
        @click="$emit('toggle-status')"
        data-testid="bot-toggle-btn"
      >
        {{ running ? '禁用 Bot' : '启用 Bot' }}
      </t-button>
      <t-button variant="outline" @click="$router.push({ name: 'chat' })">返回聊天</t-button>
      <t-button theme="danger" variant="outline" @click="$emit('remove')" style="margin-left: auto" data-testid="bot-delete-btn">
        删除 Bot
      </t-button>
    </div>
  </div>
</template>

<script setup>
defineProps({
  form: { type: Object, required: true },
  emojis: { type: Array, default: () => [] },
  modelOptions: { type: Array, default: () => [] },
  running: { type: Boolean, default: false },
  toggling: { type: Boolean, default: false }
})
defineEmits(['save', 'remove', 'toggle-status'])

const effortOptions = [
  { label: '默认（不指定）', value: '' },
  { label: 'minimal', value: 'minimal' },
  { label: 'low', value: 'low' },
  { label: 'medium', value: 'medium' },
  { label: 'high', value: 'high' }
]
</script>

<style scoped>
.card { margin-bottom: 20px; }
.avatar-row { display: flex; align-items: center; gap: 20px; margin-bottom: 8px; }
.bot-avatar-preview {
  width: 64px; height: 64px; border-radius: 14px; background: #f3f3f5;
  display: flex; align-items: center; justify-content: center; font-size: 34px; flex-shrink: 0;
}
.pick-label { font-size: 13px; color: #888; }
.emoji-list { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 8px; }
.emoji {
  width: 36px; height: 36px; border-radius: 8px; display: flex; align-items: center;
  justify-content: center; font-size: 20px; cursor: pointer; border: 1px solid transparent; transition: all 0.15s;
}
.emoji:hover { background: #f0f0f0; }
.emoji.active { border-color: #00a870; background: #e6f4ef; }
.row2 { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
.slider-row { display: flex; align-items: center; gap: 16px; width: 100%; }
.slider-box { flex: 1; min-width: 0; }
.slider-box :deep(.t-slider) { width: 100%; }
.temp-slider { width: 100%; display: block; }
.temp-slider :deep(.t-slider__container),
.temp-slider :deep(.t-slider__rail) { width: 100%; }
.slider-val { width: 32px; text-align: right; font-variant-numeric: tabular-nums; }
.footer-actions { display: flex; gap: 12px; align-items: center; }
</style>
