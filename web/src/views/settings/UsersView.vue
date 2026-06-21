<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { Plus, Shield, KeyRound, Power, Trash2, X } from 'lucide-vue-next'
import { usersApi } from '@/api/client'
import { useToast } from '@/components/ui'
import {
  TButton, TInput, TSelect,
  TBadge, TSpinner, TEmpty, TPageHeader, TCard,
} from '@/components/ui'
// row-actions now use TButton, no more raw .row-btn
import type { UserRow } from '@/types/api'

const toast = useToast()
const users = ref<UserRow[]>([])
const loading = ref(true)
const showCreate = ref(false)

const newForm = ref({ username: '', password: '', role: 'member', displayName: '', email: '' })

function resetForm() {
  newForm.value = { username: '', password: '', role: 'member', displayName: '', email: '' }
}

async function load() {
  loading.value = true
  try {
    users.value = await usersApi.list()
  } finally {
    loading.value = false
  }
}

async function create() {
  try {
    await usersApi.create(newForm.value)
    showCreate.value = false
    resetForm()
    await load()
    toast.success('用户已创建')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '创建失败')
  }
}

async function toggleDisable(u: UserRow) {
  try {
    if (u.disabled) await usersApi.enable(u.id)
    else await usersApi.disable(u.id)
    await load()
    toast.success(u.disabled ? '已启用' : '已禁用')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '操作失败')
  }
}

async function changeRole(u: UserRow) {
  const role = u.role === 'admin' ? 'member' : 'admin'
  try {
    await usersApi.updateRole(u.id, role)
    await load()
    toast.success('角色已更新')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '操作失败')
  }
}

async function deleteUser(u: UserRow) {
  if (!confirm(`确认删除用户 ${u.username}?`)) return
  try {
    await usersApi.delete(u.id)
    await load()
    toast.success('已删除')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '删除失败')
  }
}

async function resetPwd(u: UserRow) {
  const pwd = prompt(`重置 ${u.username} 的密码 (至少 6 位):`)
  if (!pwd || pwd.length < 6) return
  try {
    await usersApi.resetPassword(u.id, pwd)
    toast.success('密码已重置')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '重置失败')
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <TPageHeader title="用户管理" subtitle="管理系统用户和权限">
      <template #actions>
        <TButton @click="showCreate = !showCreate">
          <Plus :size="14" />
          创建用户
        </TButton>
      </template>
    </TPageHeader>

    <!-- Inline create panel -->
    <div v-if="showCreate" class="page-content-narrow create-panel">
      <TCard padding="lg">
        <div class="create-panel-header">
          <h2 class="create-panel-title">创建新用户</h2>
          <TButton variant="ghost" size="icon" @click="showCreate = false">
            <X :size="16" />
          </TButton>
        </div>
        <div class="form-grid">
          <div class="form-field">
            <label>用户名</label>
            <TInput v-model="newForm.username" />
          </div>
          <div class="form-field">
            <label>密码 (至少 6 位)</label>
            <TInput v-model="newForm.password" type="password" />
          </div>
        </div>
        <div class="form-grid">
          <div class="form-field">
            <label>显示名</label>
            <TInput v-model="newForm.displayName" />
          </div>
          <div class="form-field">
            <label>角色</label>
            <TSelect v-model="newForm.role">
              <option value="member">member</option>
              <option value="admin">admin</option>
            </TSelect>
          </div>
        </div>
        <div class="form-actions">
          <TButton variant="ghost" @click="showCreate = false">取消</TButton>
          <TButton @click="create">创建</TButton>
        </div>
      </TCard>
    </div>

    <div v-if="loading" class="loading-state"><TSpinner size="lg" /></div>

    <TEmpty v-else-if="users.length === 0 && !showCreate" text="暂无用户" />

    <div v-else-if="users.length > 0" class="page-content">
      <div class="table-wrap">
        <table class="data-table">
          <thead>
            <tr><th>ID</th><th>用户名</th><th>显示名</th><th>角色</th><th>状态</th><th>最后登录</th><th>操作</th></tr>
          </thead>
          <tbody>
            <tr v-for="u in users" :key="u.id">
              <td class="mono">{{ u.id }}</td>
              <td>{{ u.username }}</td>
              <td>{{ u.displayName || '-' }}</td>
              <td>
                <TBadge :variant="u.role === 'admin' ? 'default' : 'secondary'">{{ u.role }}</TBadge>
              </td>
              <td>
                <span :style="{ color: u.disabled ? 'var(--destructive)' : 'var(--success)', fontWeight: 500 }">
                  {{ u.disabled ? '禁用' : '正常' }}
                </span>
              </td>
              <td class="muted">{{ u.lastLoginAt || '-' }}</td>
              <td>
                <div class="row-actions">
                  <TButton variant="ghost" size="icon-sm" title="切换角色" @click="changeRole(u)"><Shield :size="14" /></TButton>
                  <TButton variant="ghost" size="icon-sm" title="重置密码" @click="resetPwd(u)"><KeyRound :size="14" /></TButton>
                  <TButton variant="ghost" size="icon-sm" :title="u.disabled ? '启用' : '禁用'" @click="toggleDisable(u)">
                    <Power :size="14" />
                  </TButton>
                  <TButton variant="ghost" size="icon-sm" class="row-action-danger" title="删除" @click="deleteUser(u)"><Trash2 :size="14" /></TButton>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>

<style scoped>
@import '@/assets/page-common.css';

.row-actions {
  display: flex;
  gap: 0.125rem;
}

/* danger hover for delete button */
.row-action-danger:hover {
  color: var(--destructive);
}
</style>
