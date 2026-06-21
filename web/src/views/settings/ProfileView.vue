<script setup lang="ts">
import { ref } from 'vue'
import { Sun, Moon, KeyRound, LogOut, Ticket } from 'lucide-vue-next'
import { useAuth } from '@/composables/useAuth'
import { authApi, bindApi } from '@/api/client'
import { useDarkMode } from '@/composables/useDarkMode'
import { useToast, TButton, TInput, TPageHeader, TCard } from '@/components/ui'

const { user, logout } = useAuth()
const { theme, toggle } = useDarkMode()
const toast = useToast()

const oldPwd = ref('')
const newPwd = ref('')

async function changePassword() {
  if (newPwd.value.length < 6) {
    toast.warning('新密码至少 6 位')
    return
  }
  try {
    await authApi.changePassword(oldPwd.value, newPwd.value)
    oldPwd.value = ''
    newPwd.value = ''
    toast.success('密码修改成功')
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '修改失败')
  }
}

const bindcode = ref<string | null>(null)
const generating = ref(false)

async function genBindCode() {
  generating.value = true
  try {
    const r = await bindApi.generate()
    bindcode.value = r.code
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '生成失败')
  } finally {
    generating.value = false
  }
}
</script>

<template>
  <div class="page">
    <TPageHeader title="个人设置" subtitle="管理账户信息和偏好" />

    <div class="page-content-narrow">
      <div class="profile-content">
        <!-- 账户信息 -->
        <TCard padding="lg" elevated>
          <h2 class="profile-section-title">账户信息</h2>
          <div class="info-grid">
            <div class="info-item">
              <span class="info-label">用户名</span>
              <span class="info-value">{{ user?.username }}</span>
            </div>
            <div class="info-item">
              <span class="info-label">角色</span>
              <span class="info-value">{{ user?.role }}</span>
            </div>
            <div class="info-item">
              <span class="info-label">显示名</span>
              <span class="info-value">{{ user?.displayName }}</span>
            </div>
            <div class="info-item">
              <span class="info-label">最后登录</span>
              <span class="info-value">{{ user?.lastLoginAt || '-' }}</span>
            </div>
          </div>
        </TCard>

        <!-- 显示设置 -->
        <TCard padding="lg">
          <h2 class="profile-section-title">显示设置</h2>
          <div class="setting-row">
            <div>
              <p class="setting-label">主题模式</p>
              <p class="setting-desc">当前: {{ theme === 'dark' ? '暗色' : '亮色' }}</p>
            </div>
            <TButton variant="outline" @click="toggle">
              <component :is="theme === 'dark' ? Sun : Moon" :size="14" />
              {{ theme === 'dark' ? '切换到亮色' : '切换到暗色' }}
            </TButton>
          </div>
        </TCard>

        <!-- 修改密码 -->
        <TCard padding="lg">
          <h2 class="profile-section-title"><KeyRound :size="15" /> 修改密码</h2>
          <div class="form-field">
            <label>当前密码</label>
            <TInput v-model="oldPwd" type="password" />
          </div>
          <div class="form-field">
            <label>新密码 (至少 6 位)</label>
            <TInput v-model="newPwd" type="password" />
          </div>
          <div class="form-actions">
            <TButton @click="changePassword">修改密码</TButton>
          </div>
        </TCard>

        <!-- 授权码 -->
        <TCard padding="lg">
          <h2 class="profile-section-title"><Ticket :size="15" /> 绑定授权码</h2>
          <p class="section-desc">生成授权码用于将外部平台账号绑定到你的账户</p>
          <div class="form-actions" style="justify-content: flex-start">
            <TButton :loading="generating" @click="genBindCode">生成授权码</TButton>
          </div>
          <div v-if="bindcode" class="bindcode-display">
            <code>{{ bindcode }}</code>
          </div>
        </TCard>

        <!-- 登出 -->
        <TCard padding="lg">
          <h2 class="profile-section-title danger"><LogOut :size="15" /> 退出登录</h2>
          <div class="form-actions" style="justify-content: flex-start">
            <TButton variant="destructive" @click="logout">退出</TButton>
          </div>
        </TCard>
      </div>
    </div>
  </div>
</template>

<style scoped>
@import '@/assets/page-common.css';

.profile-content {
  display: flex;
  flex-direction: column;
  gap: 1rem;
}

/* Override the shared .section-title to avoid collision with StatsView/BotDetailView */
.profile-section-title {
  font-size: 0.9375rem;
  font-weight: 600;
  margin-bottom: 1rem;
  display: flex;
  align-items: center;
  gap: 0.5rem;
  color: var(--foreground);
}
.profile-section-title.danger { color: var(--destructive); }

.section-desc {
  font-size: 0.8125rem;
  color: var(--muted-foreground);
  margin-bottom: 0.75rem;
}

.info-grid { display: flex; flex-direction: column; gap: 0; }
.info-item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0.625rem 0;
  border-bottom: 1px solid var(--border);
}
.info-item:last-child { border-bottom: none; }
.info-label { font-size: 0.8125rem; color: var(--muted-foreground); }
.info-value { font-size: 0.8125rem; font-weight: 500; }

.setting-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.setting-label { font-size: 0.8125rem; font-weight: 500; }
.setting-desc { font-size: 0.6875rem; color: var(--muted-foreground); margin-top: 0.125rem; }

.bindcode-display {
  margin-top: 0.75rem;
  padding: 0.75rem 1rem;
  background: var(--accent);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
}
.bindcode-display code {
  font-size: 0.9375rem;
  font-weight: 600;
  font-family: var(--font-mono);
  color: var(--foreground);
}
</style>
