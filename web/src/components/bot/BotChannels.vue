<template>
  <div>
    <div class="toolbar">
      <span class="hint">为该 Bot 配置消息渠道（Telegram / Misskey 等）。</span>
      <t-button theme="primary" size="small" @click="openCreate" data-testid="channel-create-btn">
        <template #icon><t-icon name="add" /></template>
        新增渠道
      </t-button>
    </div>

    <t-table :data="list" :columns="columns" row-key="id" :loading="loading" data-testid="channel-table">
      <template #type="{ row }">
        <t-tag variant="light">{{ typeName(row.type) }}</t-tag>
      </template>
      <template #enabled="{ row }">
        <t-switch :value="row.enabled" @change="v => toggle(row, v)" />
      </template>
      <template #op="{ row }">
        <t-space size="small">
          <t-button variant="text" theme="primary" size="small" @click="openEdit(row)">编辑</t-button>
          <t-button variant="text" theme="danger" size="small" @click="remove(row)">删除</t-button>
        </t-space>
      </template>
    </t-table>

    <t-dialog v-model:visible="dialog.visible" :header="dialog.isEdit ? '编辑渠道' : '新增渠道'" :on-confirm="submit" width="560px">
      <t-form :data="dialog.form" label-align="top">
        <t-form-item label="渠道名称">
          <t-input v-model="dialog.form.name" placeholder="便于识别的名称" data-testid="channel-form-name" />
        </t-form-item>
        <t-form-item label="渠道类型">
          <t-select v-model="dialog.form.type" :options="typeOptions" :disabled="dialog.isEdit" @change="onTypeChange" data-testid="channel-form-type" />
        </t-form-item>
        <t-form-item v-for="f in currentFields" :key="f.key" :label="f.label" :help="f.helpText">
          <t-switch v-if="f.type === 'boolean'" :value="dialog.config[f.key] === 'true'" @change="v => dialog.config[f.key] = v ? 'true' : 'false'" />
          <t-select v-else-if="f.type === 'select'" v-model="dialog.config[f.key]" :options="(f.options || []).map(o => ({ label: o || '(空)', value: o }))" />
          <t-input v-else-if="f.type === 'password'" v-model="dialog.config[f.key]" type="password" :placeholder="f.required ? '必填' : '选填'" />
          <t-input-number v-else-if="f.type === 'number'" v-model="dialog.config[f.key]" style="width: 100%" />
          <t-input v-else v-model="dialog.config[f.key]" :placeholder="f.required ? '必填' : '选填'" />
        </t-form-item>
      </t-form>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { channelApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const loading = ref(false)
const list = ref([])
const types = ref([])

const columns = [
  { colKey: 'name', title: '名称', width: 160 },
  { colKey: 'type', title: '类型', width: 120 },
  { colKey: 'enabled', title: '启用', width: 90 },
  { colKey: 'op', title: '操作', width: 130, fixed: 'right' }
]

const typeOptions = computed(() => types.value.map(t => ({ label: t.displayName, value: t.type })))
function typeName(t) { return types.value.find(x => x.type === t)?.displayName || t }

async function load() {
  loading.value = true
  try {
    const [l, t] = await Promise.all([channelApi.list(props.botId), channelApi.types()])
    list.value = l
    types.value = t.types || []
  } finally {
    loading.value = false
  }
}
onMounted(load)

const dialog = ref({ visible: false, isEdit: false, form: {}, config: {} })

const currentFields = computed(() => types.value.find(t => t.type === dialog.value.form.type)?.fields || [])

function defaultConfig(type) {
  const cfg = {}
  const fields = types.value.find(t => t.type === type)?.fields || []
  fields.forEach(f => { cfg[f.key] = f.default || '' })
  return cfg
}

function onTypeChange(type) {
  dialog.value.config = defaultConfig(type)
}

function openCreate() {
  const type = types.value[0]?.type || ''
  dialog.value = { visible: true, isEdit: false, form: { name: '', type }, config: defaultConfig(type) }
}

function openEdit(row) {
  let cfg = {}
  try { cfg = JSON.parse(row.config || '{}') } catch (e) { cfg = {} }
  // 转成字符串便于表单
  Object.keys(cfg).forEach(k => { cfg[k] = String(cfg[k]) })
  dialog.value = { visible: true, isEdit: true, form: { id: row.id, name: row.name, type: row.type }, config: cfg }
}

async function submit() {
  const f = dialog.value.form
  if (!f.name) return MessagePlugin.warning('请填写渠道名称')
  const configStr = JSON.stringify(dialog.value.config)
  try {
    if (dialog.value.isEdit) {
      await channelApi.update(props.botId, f.id, { name: f.name, config: configStr })
      MessagePlugin.success('渠道已更新')
    } else {
      await channelApi.create(props.botId, { name: f.name, type: f.type, config: configStr })
      MessagePlugin.success('渠道已创建')
    }
    dialog.value.visible = false
    load()
  } catch (e) {
    MessagePlugin.error(e.message || '操作失败')
  }
}

async function toggle(row, v) {
  await channelApi.update(props.botId, row.id, { enabled: v })
  row.enabled = v
  MessagePlugin.success(v ? '已启用' : '已停用')
}

function remove(row) {
  const dlg = DialogPlugin.confirm({
    header: '删除渠道', body: `确认删除渠道「${row.name}」？`, theme: 'warning',
    onConfirm: async () => { await channelApi.remove(props.botId, row.id); dlg.destroy(); MessagePlugin.success('已删除'); load() }
  })
}
</script>

<style scoped>
.toolbar { display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px; }
.hint { color: #888; font-size: 13px; }
</style>
