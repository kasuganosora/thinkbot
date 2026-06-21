import { createRouter, createWebHistory } from 'vue-router'
import { useAuth } from '@/composables/useAuth'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/',
      name: 'chat',
      component: () => import('@/views/ChatView.vue'),
      meta: { section: 'chat' },
    },
    {
      path: '/settings/bots',
      name: 'bots',
      component: () => import('@/views/settings/BotsView.vue'),
      meta: { section: 'settings' },
    },
    {
      path: '/settings/bots/:id',
      name: 'bot-detail',
      component: () => import('@/views/settings/BotDetailView.vue'),
      meta: { section: 'settings' },
    },
    {
      path: '/settings/models',
      name: 'models',
      component: () => import('@/views/settings/ModelsView.vue'),
      meta: { section: 'settings', adminOnly: true },
    },
    {
      path: '/settings/users',
      name: 'users',
      component: () => import('@/views/settings/UsersView.vue'),
      meta: { section: 'settings', adminOnly: true },
    },
    {
      path: '/settings/system',
      name: 'system',
      component: () => import('@/views/settings/SystemView.vue'),
      meta: { section: 'settings', adminOnly: true },
    },
    {
      path: '/settings/stats',
      name: 'stats',
      component: () => import('@/views/settings/StatsView.vue'),
      meta: { section: 'settings', adminOnly: true },
    },
    {
      path: '/settings/skills',
      name: 'skills',
      component: () => import('@/views/settings/SkillsView.vue'),
      meta: { section: 'settings' },
    },
    {
      path: '/settings/profile',
      name: 'profile',
      component: () => import('@/views/settings/ProfileView.vue'),
      meta: { section: 'settings' },
    },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

router.beforeEach((to) => {
  const { user } = useAuth()
  if (!user.value) return
  if (to.meta.adminOnly && user.value.role !== 'admin') {
    return { name: 'chat' }
  }
})
