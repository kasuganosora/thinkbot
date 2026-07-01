<template>
  <SettingsShell title="技能管理">
    <t-card title="技能列表" :bordered="false" class="card">
      <t-table
        row-key="name"
        data-testid="skills-table"
        :data="skills"
        :columns="columns"
        :loading="loading"
        size="small"
        hover
      >
        <template #source="{ row }">
          <t-tag variant="light" theme="primary">{{ row.source || '-' }}</t-tag>
        </template>
        <template #capabilities="{ row }">
          <div class="cap-tags">
            <t-tag v-if="row.hasContent" size="small" variant="light">内容</t-tag>
            <t-tag v-if="row.hasScripts" size="small" variant="light" theme="warning">脚本</t-tag>
            <t-tag v-if="row.hasReferences" size="small" variant="light" theme="success">引用</t-tag>
            <t-tag v-if="row.hasAssets" size="small" variant="light" theme="primary">资源</t-tag>
          </div>
        </template>
        <template #enabled="{ row }">
          <t-switch
            :value="row.enabled"
            :loading="toggling === row.name"
            :data-testid="`skill-switch-${row.name}`"
            @change="(val) => toggle(row, val)"
          />
        </template>
      </t-table>
    </t-card>
  </SettingsShell>
</template>

<script setup>
import { ref, onMounted } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import SettingsShell from '@/components/SettingsShell.vue'
import { skillApi } from '@/api/services'

const loading = ref(false)
const toggling = ref('')
const skills = ref([])

const columns = [
  { colKey: 'name', title: '名称', width: 160 },
  { colKey: 'description', title: '描述', ellipsis: true },
  { colKey: 'source', title: '来源', width: 110 },
  { colKey: 'capabilities', title: '能力', width: 220 },
  { colKey: 'enabled', title: '启用', width: 90, fixed: 'right' }
]

async function loadList() {
  loading.value = true
  try {
    const res = await skillApi.list()
    skills.value = res.skills || []
  } catch (e) {
    MessagePlugin.error(`加载技能列表失败：${e.message || e}`)
  } finally {
    loading.value = false
  }
}

async function toggle(row, val) {
  toggling.value = row.name
  try {
    if (val) {
      await skillApi.enable(row.name)
      MessagePlugin.success(`已启用技能「${row.name}」`)
    } else {
      await skillApi.disable(row.name)
      MessagePlugin.success(`已禁用技能「${row.name}」`)
    }
    row.enabled = val
  } catch (e) {
    MessagePlugin.error(`操作失败：${e.message || e}`)
  } finally {
    toggling.value = ''
  }
}

onMounted(loadList)
</script>

<style scoped>
.card {
  margin-bottom: 20px;
}
.cap-tags {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}
</style>
