<template>
  <SettingsShell title="用户管理">
    <t-card :bordered="false" class="card">
      <div class="toolbar">
        <t-input v-model="keyword" placeholder="搜索用户名 / 昵称" clearable style="width: 240px" data-testid="user-search" />
        <t-button theme="primary" @click="openCreate" data-testid="user-create-btn">
          <template #icon><t-icon name="add" /></template>
          新建用户
        </t-button>
      </div>

      <t-table
        :data="filtered"
        :columns="columns"
        row-key="id"
        :loading="loading"
        size="medium"
        data-testid="user-table"
      >
        <template #role="{ row }">
          <t-tag :theme="row.role === 'admin' ? 'primary' : 'default'" variant="light">
            {{ row.role === 'admin' ? '管理员' : '成员' }}
          </t-tag>
        </template>
        <template #status="{ row }">
          <t-tag :theme="row.status === 'active' ? 'success' : 'danger'" variant="light">
            {{ row.status === 'active' ? '正常' : '已禁用' }}
          </t-tag>
        </template>
        <template #lastLoginAt="{ row }">{{ formatTime(row.lastLoginAt) }}</template>
        <template #op="{ row }">
          <t-space size="small">
            <t-button variant="text" theme="primary" size="small" @click="openEdit(row)" :data-testid="`user-edit-${row.id}`">编辑</t-button>
            <t-button variant="text" size="small" @click="toggleRole(row)">{{ row.role === 'admin' ? '降为成员' : '设为管理员' }}</t-button>
            <t-button variant="text" :theme="row.status === 'active' ? 'warning' : 'success'" size="small" @click="toggleStatus(row)">
              {{ row.status === 'active' ? '禁用' : '启用' }}
            </t-button>
            <t-button variant="text" theme="danger" size="small" @click="remove(row)" :data-testid="`user-delete-${row.id}`">删除</t-button>
          </t-space>
        </template>
      </t-table>
    </t-card>

    <t-dialog
      v-model:visible="dialog.visible"
      :header="dialog.isEdit ? '编辑用户' : '新建用户'"
      :on-confirm="submit"
      width="480px"
      data-testid="user-dialog"
    >
      <t-form :data="dialog.form" label-align="top">
        <t-form-item label="用户名" v-if="!dialog.isEdit">
          <t-input v-model="dialog.form.username" placeholder="登录用户名" data-testid="user-form-username" />
        </t-form-item>
        <t-form-item label="初始密码" v-if="!dialog.isEdit">
          <t-input v-model="dialog.form.password" type="password" placeholder="至少 6 位" data-testid="user-form-password" />
        </t-form-item>
        <t-form-item label="昵称">
          <t-input v-model="dialog.form.displayName" placeholder="显示名称" data-testid="user-form-displayname" />
        </t-form-item>
        <t-form-item label="邮箱">
          <t-input v-model="dialog.form.email" placeholder="邮箱地址" data-testid="user-form-email" />
        </t-form-item>
        <t-form-item label="角色" v-if="!dialog.isEdit">
          <t-select v-model="dialog.form.role" :options="roleOptions" />
        </t-form-item>
      </t-form>
    </t-dialog>
  </SettingsShell>
</template>

<script setup>
import { ref, computed, onMounted } from 'vue'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import SettingsShell from '@/components/SettingsShell.vue'
import { userApi } from '@/api/services'
import { formatTime } from '@/utils/format'

const loading = ref(false)
const users = ref([])
const keyword = ref('')

const roleOptions = [
  { label: '管理员', value: 'admin' },
  { label: '成员', value: 'member' }
]

const columns = [
  { colKey: 'id', title: 'ID', width: 70 },
  { colKey: 'username', title: '用户名', width: 130 },
  { colKey: 'displayName', title: '昵称', width: 130 },
  { colKey: 'email', title: '邮箱' },
  { colKey: 'role', title: '角色', width: 110 },
  { colKey: 'status', title: '状态', width: 100 },
  { colKey: 'lastLoginAt', title: '最近登录', width: 170 },
  { colKey: 'op', title: '操作', width: 280, fixed: 'right' }
]

const filtered = computed(() => {
  const k = keyword.value.trim().toLowerCase()
  if (!k) return users.value
  return users.value.filter(u => (u.username + u.displayName).toLowerCase().includes(k))
})

async function load() {
  loading.value = true
  try {
    users.value = await userApi.list()
  } finally {
    loading.value = false
  }
}
onMounted(load)

const dialog = ref({ visible: false, isEdit: false, form: {} })

function openCreate() {
  dialog.value = { visible: true, isEdit: false, form: { username: '', password: '', displayName: '', email: '', role: 'member' } }
}
function openEdit(row) {
  dialog.value = { visible: true, isEdit: true, form: { id: row.id, displayName: row.displayName, email: row.email } }
}

async function submit() {
  const f = dialog.value.form
  try {
    if (dialog.value.isEdit) {
      await userApi.update(f.id, { displayName: f.displayName, email: f.email })
      MessagePlugin.success('已更新用户资料')
    } else {
      if (!f.username) return MessagePlugin.warning('请填写用户名')
      if (!f.password || f.password.length < 6) return MessagePlugin.warning('密码至少 6 位')
      await userApi.create(f)
      MessagePlugin.success('用户已创建')
    }
    dialog.value.visible = false
    load()
  } catch (e) {
    MessagePlugin.error(e.message || '操作失败')
  }
}

async function toggleRole(row) {
  const next = row.role === 'admin' ? 'member' : 'admin'
  await userApi.setRole(row.id, next)
  MessagePlugin.success('角色已更新')
  load()
}

async function toggleStatus(row) {
  if (row.status === 'active') await userApi.disable(row.id)
  else await userApi.enable(row.id)
  MessagePlugin.success('状态已更新')
  load()
}

function remove(row) {
  const dlg = DialogPlugin.confirm({
    header: '删除用户',
    body: `确认删除用户「${row.username}」？`,
    theme: 'warning',
    onConfirm: async () => {
      await userApi.remove(row.id)
      dlg.destroy()
      MessagePlugin.success('已删除')
      load()
    }
  })
}
</script>

<style scoped>
.card { margin-bottom: 20px; }
.toolbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 16px;
}
</style>
