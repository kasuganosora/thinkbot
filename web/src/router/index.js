import { createRouter, createWebHashHistory } from 'vue-router'
import { useUserStore } from '@/stores/user'
import { MessagePlugin } from 'tdesign-vue-next'

const routes = [
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/Login.vue'),
    meta: { public: true }
  },
  {
    path: '/',
    component: () => import('@/layouts/MainLayout.vue'),
    redirect: '/chat',
    children: [
      {
        path: 'chat',
        name: 'chat',
        component: () => import('@/views/Chat.vue')
      },
      // --- 个人 / 系统设置 ---
      {
        path: 'settings/system',
        name: 'system-settings',
        component: () => import('@/views/SystemSettings.vue')
      },
      {
        path: 'settings/user',
        name: 'user-settings',
        component: () => import('@/views/UserSettings.vue')
      },
      {
        path: 'settings/bot/:id?',
        name: 'bot-settings',
        component: () => import('@/views/BotSettings.vue')
      },
      // --- 管理后台（admin） ---
      {
        path: 'admin/users',
        name: 'admin-users',
        component: () => import('@/views/admin/UsersView.vue'),
        meta: { admin: true }
      },
      {
        path: 'admin/skills',
        name: 'admin-skills',
        component: () => import('@/views/admin/SkillsView.vue'),
        meta: { admin: true }
      },
      {
        path: 'admin/config',
        name: 'admin-config',
        component: () => import('@/views/admin/ConfigView.vue'),
        meta: { admin: true }
      },
      {
        path: 'admin/stats',
        name: 'admin-stats',
        component: () => import('@/views/admin/StatsView.vue'),
        meta: { admin: true }
      },
      {
        path: 'admin/system',
        name: 'admin-system',
        component: () => import('@/views/admin/SystemMonitorView.vue'),
        meta: { admin: true }
      }
    ]
  }
]

const router = createRouter({
  history: createWebHashHistory(),
  routes
})

router.beforeEach((to) => {
  const userStore = useUserStore()
  if (!to.meta.public && !userStore.isLoggedIn) {
    return { name: 'login' }
  }
  if (to.name === 'login' && userStore.isLoggedIn) {
    return { name: 'chat' }
  }
  // admin 页面权限校验
  if (to.meta.admin && userStore.user?.role !== 'admin') {
    MessagePlugin.warning('该功能仅管理员可访问')
    return { name: 'chat' }
  }
})

export default router
