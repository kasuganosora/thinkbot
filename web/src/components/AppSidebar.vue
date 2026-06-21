<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Bot, MessageSquare, Cpu, Wrench, User, Users, Settings, BarChart3, ArrowLeft, LogOut, Sun, Moon } from 'lucide-vue-next'
import type { Component } from 'vue'
import { useAuth } from '@/composables/useAuth'
import { useDarkMode } from '@/composables/useDarkMode'

const { user, isAdmin, logout } = useAuth()
const { theme, toggle } = useDarkMode()
const route = useRoute()
const router = useRouter()

const section = computed(() => (route.meta.section as string) ?? 'chat')

const iconMap: Record<string, Component> = {
  chat: MessageSquare,
  bot: Bot,
  cpu: Cpu,
  gear: Settings,
  user: User,
  users: Users,
  system: Settings,
  chart: BarChart3,
  skill: Wrench,
  back: ArrowLeft,
}

const chatNav = [{ label: '对话', icon: 'chat', route: '/' }]

const settingsNav = computed(() => {
  const items = [
    { label: '机器人', icon: 'bot', route: '/settings/bots' },
    { label: '技能', icon: 'skill', route: '/settings/skills' },
    { label: '个人', icon: 'user', route: '/settings/profile' },
  ]
  if (isAdmin.value) {
    items.splice(1, 0, { label: '模型', icon: 'cpu', route: '/settings/models' })
    items.splice(2, 0, { label: '用户', icon: 'users', route: '/settings/users' })
    items.splice(3, 0, { label: '系统', icon: 'system', route: '/settings/system' })
    items.splice(4, 0, { label: '统计', icon: 'chart', route: '/settings/stats' })
  }
  return items
})

function navTo(r: string) {
  router.push(r)
}
</script>

<template>
  <aside class="sidebar">
    <!-- Logo -->
    <div class="sidebar-header">
      <div class="brand" @click="navTo('/')">
        <Bot :size="20" class="brand-icon" />
        <span>ThinkBot</span>
      </div>
      <button class="icon-btn" @click="toggle" :title="theme === 'dark' ? '亮色' : '暗色'">
        <Sun v-if="theme === 'dark'" :size="16" />
        <Moon v-else :size="16" />
      </button>
    </div>

    <nav class="nav-sections">
      <!-- 对话区 -->
      <div class="nav-group" v-show="section === 'chat'">
        <p class="group-label">导航</p>
        <button
          v-for="item in chatNav" :key="item.route"
          class="nav-item" :class="{ active: route.path === item.route }"
          @click="navTo(item.route)"
        >
          <component :is="iconMap[item.icon]" :size="16" />
          <span>{{ item.label }}</span>
        </button>
        <button class="nav-item" @click="navTo('/settings/bots')">
          <component :is="iconMap.gear" :size="16" />
          <span>设置</span>
        </button>
      </div>

      <!-- 设置区 -->
      <div class="nav-group" v-show="section === 'settings'">
        <p class="group-label">设置</p>
        <button
          v-for="item in settingsNav" :key="item.route"
          class="nav-item"
          :class="{ active: route.path.startsWith(item.route) }"
          @click="navTo(item.route)"
        >
          <component :is="iconMap[item.icon]" :size="16" />
          <span>{{ item.label }}</span>
        </button>
        <button class="nav-item back" @click="navTo('/')">
          <component :is="iconMap.back" :size="16" />
          <span>返回对话</span>
        </button>
      </div>
    </nav>

    <!-- 用户信息 -->
    <div class="sidebar-footer">
      <div class="user-info">
        <div class="user-avatar">
          {{ (user?.displayName || user?.username || '?').charAt(0).toUpperCase() }}
        </div>
        <div class="user-text">
          <p class="user-name">{{ user?.displayName || user?.username }}</p>
          <p class="user-role">{{ user?.role }}</p>
        </div>
      </div>
      <button class="icon-btn logout" title="登出" @click="logout">
        <LogOut :size="16" />
      </button>
    </div>
  </aside>
</template>

<style scoped>
.sidebar {
  width: var(--sidebar-w);
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  background: var(--sidebar);
  border-right: 1px solid var(--sidebar-border);
}

.sidebar-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0.875rem 1rem;
  border-bottom: 1px solid var(--sidebar-border);
}

.brand {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  font-weight: 600;
  font-size: 1rem;
  letter-spacing: -0.02em;
  cursor: pointer;
  color: var(--sidebar-foreground);
}

.brand-icon { color: var(--muted-foreground); }

.icon-btn {
  width: 32px; height: 32px;
  border-radius: var(--radius-sm);
  display: flex; align-items: center; justify-content: center;
  color: var(--muted-foreground);
  transition: all 0.15s ease;
}
.icon-btn:hover {
  background: var(--sidebar-accent);
  color: var(--sidebar-foreground);
}

.nav-sections { flex: 1; overflow-y: auto; padding: 0.5rem; }

.group-label {
  padding: 0.5rem 0.625rem 0.375rem;
  font-size: 0.6875rem;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  color: var(--muted-foreground);
}

.nav-item {
  width: 100%;
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.4375rem 0.625rem;
  border-radius: var(--radius-sm);
  transition: all 0.15s ease;
  text-align: left;
  font-size: 0.8125rem;
  font-weight: 500;
  color: var(--muted-foreground);
  position: relative;
}

.nav-item:hover {
  background: var(--sidebar-accent);
  color: var(--sidebar-foreground);
}

/* Active item: accent bg + 2px brand left border (purple scarcity law) */
.nav-item.active {
  background: var(--sidebar-accent);
  color: var(--sidebar-foreground);
}
.nav-item.active::before {
  content: '';
  position: absolute;
  left: 0;
  top: 50%;
  transform: translateY(-50%);
  width: 2px;
  height: 16px;
  background: var(--brand);
  border-radius: 0 1px 1px 0;
}

.nav-item.back { margin-top: 0.5rem; }

.sidebar-footer {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.625rem 1rem;
  border-top: 1px solid var(--sidebar-border);
}

.user-info { flex: 1; display: flex; align-items: center; gap: 0.5rem; min-width: 0; }

.user-avatar {
  width: 32px; height: 32px; flex-shrink: 0;
  border-radius: var(--radius-sm);
  background: var(--foreground);
  color: var(--background);
  display: flex; align-items: center; justify-content: center;
  font-size: 0.8125rem; font-weight: 600;
}

.user-text { min-width: 0; }
.user-name {
  font-size: 0.8125rem; font-weight: 500;
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.user-role {
  font-size: 0.6875rem; color: var(--muted-foreground);
  text-transform: capitalize;
}

.icon-btn.logout:hover { color: var(--destructive); }
</style>
