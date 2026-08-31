<template>
  <SettingsShell title="系统设置" wide>
    <div class="sys-layout">
      <!-- 左侧子导航 -->
      <nav class="sys-nav" data-testid="system-settings-nav" aria-label="系统设置导航">
        <button
          v-for="item in visibleNavItems"
          :key="item.key"
          class="nav-item"
          :class="{ active: activeKey === item.key }"
          :data-testid="`system-nav-${item.key}`"
          :aria-current="activeKey === item.key ? 'page' : undefined"
          @click="onNavClick(item)"
        >
          <t-icon :name="item.icon" class="nav-icon" />
          <span>{{ item.label }}</span>
        </button>
      </nav>

      <!-- 右侧内容 -->
      <section class="sys-content" data-testid="system-settings-content">
        <!-- Bots -->
        <div v-show="activeKey === 'bots'" class="panel panel-wide" data-testid="system-panel-bots">
          <div class="panel-head">
            <div>
              <h3 class="panel-title">Bots</h3>
              <p class="panel-desc">管理平台内的 Bot，点击卡片进入详细设置。</p>
            </div>
          </div>
          <div class="panel-scroll">
            <BotsManager />
          </div>
        </div>

        <!-- 外观 -->
        <div v-show="activeKey === 'appearance'" class="panel" data-testid="system-panel-appearance">
          <div class="panel-head">
            <div>
              <h3 class="panel-title">外观</h3>
              <p class="panel-desc">设置主题、主题色与界面语言。</p>
            </div>
            <t-button theme="primary" size="small" data-testid="system-save-btn" @click="save">保存</t-button>
          </div>
          <div class="panel-card">
            <t-form label-align="left" :label-width="120">
              <t-form-item label="主题色">
                <t-radio-group v-model="settings.primaryColor">
                  <t-radio-button value="green">绿色</t-radio-button>
                  <t-radio-button value="blue">蓝色</t-radio-button>
                  <t-radio-button value="purple">紫色</t-radio-button>
                </t-radio-group>
              </t-form-item>
              <!-- 主题模式 / 界面语言：产品尚未提供深色样式与 i18n 运行时，
                   保留为禁用态并说明，避免做成「点了保存却毫无变化」的假控件 -->
              <t-form-item label="主题模式">
                <t-radio-group v-model="settings.theme" disabled>
                  <t-radio-button value="light">浅色</t-radio-button>
                  <t-radio-button value="dark">深色</t-radio-button>
                </t-radio-group>
                <span class="form-hint">深色主题尚未提供，目前仅支持浅色</span>
              </t-form-item>
              <t-form-item label="界面语言">
                <t-select v-model="settings.language" :options="langOptions" style="width: 200px" disabled />
                <span class="form-hint">当前仅提供简体中文</span>
              </t-form-item>
            </t-form>
          </div>
        </div>

        <!-- 模型服务 -->
        <div v-show="activeKey === 'model'" class="panel panel-wide" data-testid="system-panel-model">
          <div class="panel-head">
            <div>
              <h3 class="panel-title">模型服务</h3>
              <p class="panel-desc">管理多个模型服务商及其下的模型，供 Bot 引用。</p>
            </div>
          </div>
          <div class="panel-card panel-card-flush">
            <ProvidersManager />
          </div>
        </div>

        <!-- 搜索 -->
        <div v-show="activeKey === 'search'" class="panel panel-wide" data-testid="system-panel-search">
          <div class="panel-head">
            <div>
              <h3 class="panel-title">搜索</h3>
              <p class="panel-desc">管理联网搜索提供方及其 API 配置，供 Bot 检索使用。</p>
            </div>
          </div>
          <div class="panel-card panel-card-flush">
            <SearchProvidersManager />
          </div>
        </div>

        <!-- 统计 -->
        <div v-show="activeKey === 'usage'" class="panel panel-wide" data-testid="system-panel-usage">
          <div class="panel-head">
            <div>
              <h3 class="panel-title">统计</h3>
              <p class="panel-desc">模型用量、缓存命中与各 Bot×模型的消耗情况。</p>
            </div>
          </div>
          <div class="panel-scroll">
            <UsageStats />
          </div>
        </div>

        <!-- 关于 -->
        <div v-show="activeKey === 'about'" class="panel" data-testid="system-panel-about">
          <div class="panel-head">
            <div>
              <h3 class="panel-title">关于</h3>
              <p class="panel-desc">应用版本与运行信息。</p>
            </div>
          </div>
          <div class="panel-card">
            <div class="about-row"><span class="about-k">Git Commit</span><span class="about-v" data-testid="system-about-commit">{{ APP_COMMIT }}</span></div>
          </div>
        </div>

        <!-- 管理后台：整合进系统设置页内。
             这里必须用 v-if 而非 v-show——v-show 只切 display，组件仍会挂载并在
             onMounted 里请求 /api/admin/* ，非管理员进来就会预加载一堆 403 的管理接口。
             叠加 isAdmin 判断，即使 activeKey 被异常置为 admin-* 也不会挂载。 -->
        <div v-if="isAdmin && activeKey === 'admin-users'" class="panel panel-wide" data-testid="system-panel-admin-users">
          <UsersView embedded />
        </div>
        <div v-if="isAdmin && activeKey === 'admin-skills'" class="panel panel-wide" data-testid="system-panel-admin-skills">
          <SkillsView embedded />
        </div>
        <div v-if="isAdmin && activeKey === 'admin-config'" class="panel panel-wide" data-testid="system-panel-admin-config">
          <ConfigView embedded />
        </div>
        <div v-if="isAdmin && activeKey === 'admin-stats'" class="panel panel-wide" data-testid="system-panel-admin-stats">
          <StatsView embedded />
        </div>
        <div v-if="isAdmin && activeKey === 'admin-system'" class="panel panel-wide" data-testid="system-panel-admin-system">
          <SystemMonitorView embedded />
        </div>
      </section>
    </div>
  </SettingsShell>
</template>

<script setup>
import { ref, computed, onMounted, watch } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { useUserStore } from '@/stores/user'
import SettingsShell from '@/components/SettingsShell.vue'
import BotsManager from '@/components/BotsManager.vue'
import ProvidersManager from '@/components/ProvidersManager.vue'
import SearchProvidersManager from '@/components/SearchProvidersManager.vue'
import UsageStats from '@/components/UsageStats.vue'
import UsersView from '@/views/admin/UsersView.vue'
import SkillsView from '@/views/admin/SkillsView.vue'
import ConfigView from '@/views/admin/ConfigView.vue'
import StatsView from '@/views/admin/StatsView.vue'
import SystemMonitorView from '@/views/admin/SystemMonitorView.vue'

const userStore = useUserStore()

// 只认服务端确认过的角色：localStorage 里的 role 可被随意伪造，
// 用它决定是否渲染管理面板等于把前端鉴权交给攻击者。
const isAdmin = computed(() => userStore.isAdmin)

onMounted(() => {
  // 向服务端确认角色，确认后管理入口与面板才会出现（失败即按非管理员处理）
  userStore.ensureProfile().catch(() => {})
})

const navItems = [
  { key: 'bots', label: 'Bots', icon: 'application' },
  { key: 'appearance', label: '外观', icon: 'palette' },
  { key: 'model', label: '模型服务', icon: 'server' },
  { key: 'search', label: '搜索', icon: 'internet' },
  { key: 'usage', label: '统计', icon: 'chart' },
  // 管理后台功能整合进系统设置（仅管理员可见，页内切换面板）
  { key: 'admin-users', label: '用户管理', icon: 'user', admin: true },
  { key: 'admin-skills', label: '技能管理', icon: 'code', admin: true },
  { key: 'admin-config', label: '系统配置', icon: 'setting', admin: true },
  { key: 'admin-stats', label: '统计概览', icon: 'chart-bar', admin: true },
  { key: 'admin-system', label: '系统监控', icon: 'desktop', admin: true },
  { key: 'about', label: '关于', icon: 'info-circle' }
]

const visibleNavItems = computed(() =>
  navItems.filter(item => !item.admin || isAdmin.value)
)

const APP_COMMIT = import.meta.env.APP_COMMIT || 'unknown'

const activeKey = ref('bots')

function onNavClick(item) {
  activeKey.value = item.key
}

const langOptions = [
  { label: '简体中文', value: 'zh-CN' },
  { label: 'English', value: 'en-US' }
]

const defaults = {
  theme: 'light',
  primaryColor: 'green',
  language: 'zh-CN',
  apiBase: 'https://api.example.com/v1',
  apiKey: ''
}

function readSettings() {
  try {
    return { ...defaults, ...JSON.parse(localStorage.getItem('bp_system') || '{}') }
  } catch {
    return { ...defaults }
  }
}

const settings = ref(readSettings())

// 主题色调色板：写入 TDesign 的品牌色 CSS 变量，组件与自定义样式（均引用
// var(--td-brand-color, …)）会立即跟随变化——这是「保存」真正生效的部分。
const BRAND_PALETTES = {
  green: { base: '#00a870', hover: '#00915f', active: '#007a4f', light: '#e3f9f0' },
  blue: { base: '#0052d9', hover: '#0047ba', active: '#003cab', light: '#e0ebff' },
  purple: { base: '#7c3aed', hover: '#6d28d9', active: '#5b21b6', light: '#f0e7ff' }
}

function applyPrimaryColor(name) {
  const p = BRAND_PALETTES[name] || BRAND_PALETTES.green
  const style = document.documentElement.style
  style.setProperty('--td-brand-color', p.base)
  style.setProperty('--td-brand-color-hover', p.hover)
  style.setProperty('--td-brand-color-active', p.active)
  style.setProperty('--td-brand-color-light', p.light)
}

// 进入页面即按已保存的偏好生效（此前保存只写 localStorage，刷新后毫无变化）
onMounted(() => applyPrimaryColor(settings.value.primaryColor))
// 选中即预览，无需等保存
watch(() => settings.value.primaryColor, applyPrimaryColor)

function save() {
  try {
    localStorage.setItem('bp_system', JSON.stringify(settings.value))
  } catch (e) {
    MessagePlugin.error(`保存失败：${e.message || e}`)
    return
  }
  applyPrimaryColor(settings.value.primaryColor)
  MessagePlugin.success('系统设置已保存')
}
</script>

<style scoped>
.sys-layout {
  display: flex;
  height: 100%;
}
/* 左侧子导航 */
.sys-nav {
  width: 200px;
  flex-shrink: 0;
  padding: 20px 12px;
  border-right: 1px solid #ececec;
  background: #fafafa;
  display: flex;
  flex-direction: column;
  gap: 4px;
  overflow-y: auto;
}
.nav-item {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 9px 12px;
  border: none;
  background: transparent;
  border-radius: 8px;
  font-size: 14px;
  color: #555;
  cursor: pointer;
  text-align: left;
  transition: background 0.15s, color 0.15s;
}
.nav-item:hover { background: #efefef; }
.nav-item.active {
  background: #fff;
  color: #1d1d1f;
  font-weight: 600;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.06);
}
.nav-icon { font-size: 16px; }

/* 右侧内容 */
.sys-content {
  flex: 1;
  overflow-y: auto;
  padding: 28px 32px;
}
.panel { max-width: 760px; }
.panel-wide { max-width: none; height: 100%; display: flex; flex-direction: column; }
.panel-wide .panel-card-flush {
  flex: 1;
  padding: 0;
  overflow: hidden;
  display: flex;
}
.panel-wide .panel-card-flush > * { flex: 1; min-width: 0; }
.embed-panel { height: 100%; display: flex; flex-direction: column; }
.embed-panel .panel-card { flex: 1; overflow-y: auto; }
.panel-scroll {
  flex: 1;
  overflow-y: auto;
  padding-right: 4px;
}
.panel-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  margin-bottom: 16px;
}
.panel-title {
  font-size: 18px;
  font-weight: 600;
  margin: 0 0 4px;
  color: #1d1d1f;
}
.panel-desc {
  font-size: 13px;
  color: #888;
  margin: 0;
}
.form-hint {
  margin-left: 10px;
  font-size: 12px;
  color: #999;
}
.panel-card {
  background: #fff;
  border: 1px solid #ececec;
  border-radius: 12px;
  padding: 20px 24px;
}
.about-row {
  display: flex;
  padding: 10px 0;
  border-bottom: 1px solid #f2f2f2;
  font-size: 14px;
}
.about-row:last-child { border-bottom: none; }
.about-k { width: 120px; color: #888; }
.about-v { color: #1d1d1f; }
</style>