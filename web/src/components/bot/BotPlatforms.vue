<template>
  <div class="plat-wrap" data-testid="bot-platform">
    <!-- 左：平台列表 -->
    <div class="plat-list">
      <div
        v-for="p in list"
        :key="p.id"
        class="plat-item"
        :class="{ active: cur && cur.id === p.id }"
        @click="select(p)"
      >
        <div class="pi-icon" :style="{ background: (metaOf(p.type)?.color || '#ccc') + '22' }">{{ metaOf(p.type)?.icon || p.name.slice(0, 1) }}</div>
        <div class="pi-main">
          <div class="pi-name">{{ p.name }}</div>
          <div class="pi-sub">{{ p.configured ? '已配置' : '未配置' }}</div>
        </div>
      </div>
      <button class="plat-add" data-testid="platform-add" @click="openAdd">
        <t-icon name="add" /> 添加平台
      </button>
    </div>

    <!-- 右：详情 -->
    <div v-if="cur" class="plat-detail">
      <div class="pd-head">
        <div class="pd-avatar" :style="{ background: (curMeta?.color || '#f0f0f0') + '22' }">{{ curMeta?.icon || cur.name.slice(0, 1) }}</div>
        <div class="pd-title">
          <div class="pd-name">{{ cur.name }}</div>
          <div class="pd-id">平台标识：{{ cur.type }}</div>
        </div>
      </div>

      <h4 class="sec-title">凭据配置</h4>
      <div class="cred-form">
        <div v-for="f in fields" :key="f.key" class="cred-item">
          <label>{{ f.label }} <span v-if="f.optional" class="opt">(可选)</span></label>
          <div v-if="f.help" class="cred-help">{{ f.help }}</div>
          <t-switch v-if="f.type === 'switch'" v-model="cur.config[f.key]" />
          <t-input
            v-else
            v-model="cur.config[f.key]"
            :type="f.type === 'password' && !showKey[f.key] ? 'password' : 'text'"
            :placeholder="f.placeholder"
          >
            <template v-if="f.type === 'password'" #suffix-icon>
              <t-icon :name="showKey[f.key] ? 'browse' : 'browse-off'" style="cursor:pointer" @click="showKey[f.key] = !showKey[f.key]" />
            </template>
          </t-input>
        </div>
      </div>

      <div class="pd-footer">
        <t-button variant="outline" @click="save(false)">仅保存</t-button>
        <t-button theme="primary" @click="save(true)">立即启用</t-button>
        <t-button theme="danger" variant="text" class="pd-del" @click="remove">删除</t-button>
      </div>
    </div>
    <t-empty v-else description="请选择或添加一个平台" class="plat-empty" />

    <!-- 添加平台弹窗（图1：图标平台列表） -->
    <t-dialog v-model:visible="addVisible" header="添加平台" :width="440" dialogClassName="add-dialog">
      <div class="add-list">
        <div
          v-for="t in types"
          :key="t.type"
          class="add-item"
          :class="{ active: addType === t.type }"
          @click="addType = t.type"
          @dblclick="confirmAdd"
        >
          <div class="ai-icon" :style="{ background: (t.color || '#ccc') + '22' }">{{ t.icon || t.name.slice(0, 1) }}</div>
          <span class="ai-name">{{ t.name }}</span>
        </div>
      </div>
      <template #footer>
        <t-button variant="outline" @click="addVisible = false">取消</t-button>
        <t-button theme="primary" :disabled="!addType" @click="confirmAdd">添加</t-button>
      </template>
    </t-dialog>
  </div>
</template>

<script setup>
import { ref, computed, onMounted, reactive } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import { botPlatformApi } from '@/api/services'

const props = defineProps({ botId: { type: String, required: true } })

const list = ref([])
const cur = ref(null)
const types = ref([])
const showKey = reactive({})

const fields = computed(() => types.value.find(t => t.type === cur.value?.type)?.fields || [])
const curMeta = computed(() => types.value.find(t => t.type === cur.value?.type))
function metaOf(type) { return types.value.find(t => t.type === type) }

async function load() {
  const [cat, l] = await Promise.all([botPlatformApi.toolCatalog(), botPlatformApi.list(props.botId)])
  types.value = cat.types
  list.value = l
  cur.value = l[0] || null
}
onMounted(load)

function select(p) { cur.value = p }

async function save(enable) {
  if (enable) cur.value.enabled = true
  await botPlatformApi.update(props.botId, cur.value.id, {
    enabled: cur.value.enabled, config: cur.value.config, tools: cur.value.tools, name: cur.value.name
  })
  cur.value.configured = true
  MessagePlugin.success(enable ? '已保存并启用' : '平台配置已保存')
}

function remove() {
  const dlg = DialogPlugin.confirm({
    header: '删除平台', body: `确认删除「${cur.value.name}」？`, theme: 'warning',
    onConfirm: async () => {
      await botPlatformApi.remove(props.botId, cur.value.id)
      dlg.destroy()
      MessagePlugin.success('已删除')
      await load()
    }
  })
}

const addVisible = ref(false)
const addType = ref('')
function openAdd() { addType.value = types.value[0]?.type || ''; addVisible.value = true }
async function confirmAdd() {
  const meta = types.value.find(t => t.type === addType.value)
  const created = await botPlatformApi.create(props.botId, { type: addType.value, name: meta?.name, config: {}, tools: [] })
  addVisible.value = false
  await load()
  cur.value = list.value.find(p => p.id === created.id) || cur.value
  MessagePlugin.success('平台已添加')
}
</script>

<style scoped>
.plat-wrap { display: flex; gap: 20px; height: 100%; }
/* 左列表 */
.plat-list {
  width: 240px; flex-shrink: 0; display: flex; flex-direction: column; gap: 8px;
  border-right: 1px solid #f0f0f0; padding-right: 16px;
}
.plat-item {
  display: flex; align-items: center; gap: 10px; padding: 12px 14px;
  border: 1px solid #ececec; border-radius: 10px; cursor: pointer; background: #fff;
}
.plat-item.active { border-color: #d9d9d9; box-shadow: 0 2px 8px rgba(0,0,0,.05); background: #fafafa; }
.pi-icon {
  width: 34px; height: 34px; border-radius: 50%; flex-shrink: 0;
  display: flex; align-items: center; justify-content: center; font-size: 16px;
}
.pi-main { flex: 1; min-width: 0; }
.pi-name { font-size: 14px; font-weight: 600; color: #1d1d1f; }
.pi-sub { font-size: 12px; color: #999; margin-top: 2px; }
.plat-add {
  display: flex; align-items: center; justify-content: center; gap: 6px;
  padding: 11px; border: 1px dashed #d0d0d0; border-radius: 10px; background: #fff;
  color: #555; font-size: 14px; cursor: pointer;
}
.plat-add:hover { border-color: #999; }
/* 右详情 */
.plat-detail { flex: 1; min-width: 0; overflow-y: auto; padding-right: 4px; }
.pd-head { display: flex; align-items: center; gap: 12px; margin-bottom: 24px; }
.pd-avatar {
  width: 40px; height: 40px; border-radius: 50%;
  display: flex; align-items: center; justify-content: center; font-size: 18px; color: #666;
}
.pd-title { flex: 1; }
.pd-name { font-size: 16px; font-weight: 600; }
.pd-id { font-size: 12px; color: #999; margin-top: 2px; }
.sec-title { font-size: 14px; font-weight: 600; margin: 0 0 14px; color: #1d1d1f; }
.cred-form { display: flex; flex-direction: column; gap: 18px; margin-bottom: 28px; }
.cred-item label { display: block; font-size: 13px; font-weight: 600; margin-bottom: 6px; }
.cred-item label .opt { font-weight: 400; color: #999; font-size: 12px; margin-left: 4px; }
.cred-help { font-size: 12px; color: #999; margin-bottom: 8px; }
.pd-footer { margin-top: 20px; padding-top: 18px; border-top: 1px solid #f0f0f0; display: flex; justify-content: flex-end; gap: 12px; align-items: center; }
.pd-del { margin-right: auto; }
.plat-empty { margin: 60px auto; }
/* 添加平台弹窗 */
.add-list {
  display: flex; flex-direction: column; gap: 2px;
  max-height: 420px; overflow-y: auto;
}
.add-list::-webkit-scrollbar { width: 6px; }
.add-list::-webkit-scrollbar-thumb { background: #e0e0e0; border-radius: 3px; }
.add-item {
  display: flex; align-items: center; gap: 12px; padding: 8px 12px;
  border-radius: 10px; cursor: pointer; transition: background .15s;
}
.add-item:hover { background: #f7f7f8; }
.add-item.active { background: #f0f1f3; }
.ai-icon {
  width: 36px; height: 36px; border-radius: 50%; flex-shrink: 0;
  display: flex; align-items: center; justify-content: center; font-size: 17px;
}
.ai-name { font-size: 15px; color: #1d1d1f; }
</style>

<!-- 弹窗渲染在 body 下，scoped 命中不到，用全局样式 -->
<style>
.add-dialog.t-dialog { padding: 10px; }
.add-dialog .t-dialog__header { padding: 0; margin-bottom: 6px; }
.add-dialog .t-dialog__body { padding: 0; }
.add-dialog .t-dialog__footer { padding: 8px 0 0; }
</style>
