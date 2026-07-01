<template>
  <SettingsShell title="系统设置" wide>
    <div class="sys-layout">
      <!-- 左侧子导航 -->
      <nav class="sys-nav" data-testid="system-settings-nav" aria-label="系统设置导航">
        <button
          v-for="item in navItems"
          :key="item.key"
          class="nav-item"
          :class="{ active: activeKey === item.key }"
          :data-testid="`system-nav-${item.key}`"
          :aria-current="activeKey === item.key ? 'page' : undefined"
          @click="activeKey = item.key"
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
              <t-form-item label="主题模式">
                <t-radio-group v-model="settings.theme">
                  <t-radio-button value="light">浅色</t-radio-button>
                  <t-radio-button value="dark">深色</t-radio-button>
                  <t-radio-button value="auto">跟随系统</t-radio-button>
                </t-radio-group>
              </t-form-item>
              <t-form-item label="主题色">
                <t-radio-group v-model="settings.primaryColor">
                  <t-radio-button value="green">绿色</t-radio-button>
                  <t-radio-button value="blue">蓝色</t-radio-button>
                  <t-radio-button value="purple">紫色</t-radio-button>
                </t-radio-group>
              </t-form-item>
              <t-form-item label="界面语言">
                <t-select v-model="settings.language" :options="langOptions" style="width: 200px" />
              </t-form-item>
            </t-form>
          </div>
        </div>

        <!-- 对话设置 -->
        <div v-show="activeKey === 'chat'" class="panel" data-testid="system-panel-chat">
          <div class="panel-head">
            <div>
              <h3 class="panel-title">对话设置</h3>
              <p class="panel-desc">控制发送方式、输出与上下文保留。</p>
            </div>
            <t-button theme="primary" size="small" @click="save">保存</t-button>
          </div>
          <div class="panel-card">
            <t-form label-align="left" :label-width="120">
              <t-form-item label="发送快捷键">
                <t-radio-group v-model="settings.sendKey">
                  <t-radio-button value="enter">Enter 发送</t-radio-button>
                  <t-radio-button value="cmd-enter">⌘ + Enter 发送</t-radio-button>
                </t-radio-group>
              </t-form-item>
              <t-form-item label="流式输出">
                <t-switch v-model="settings.stream" />
              </t-form-item>
              <t-form-item label="保留历史轮数">
                <t-input-number v-model="settings.contextRounds" :min="1" :max="50" style="width: 160px" />
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
            <div class="about-row"><span class="about-k">应用名称</span><span class="about-v">Bot 平台</span></div>
            <div class="about-row"><span class="about-k">版本</span><span class="about-v">v1.0.0 (mock)</span></div>
            <div class="about-row"><span class="about-k">前端框架</span><span class="about-v">Vue 3 + TDesign</span></div>
            <div class="about-row"><span class="about-k">数据来源</span><span class="about-v">本地 Mock（待接入后端）</span></div>
          </div>
        </div>
      </section>
    </div>
  </SettingsShell>
</template>

<script setup>
import { ref } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import SettingsShell from '@/components/SettingsShell.vue'
import BotsManager from '@/components/BotsManager.vue'
import ProvidersManager from '@/components/ProvidersManager.vue'
import SearchProvidersManager from '@/components/SearchProvidersManager.vue'
import UsageStats from '@/components/UsageStats.vue'

const navItems = [
  { key: 'bots', label: 'Bots', icon: 'application' },
  { key: 'appearance', label: '外观', icon: 'palette' },
  { key: 'chat', label: '对话设置', icon: 'chat' },
  { key: 'model', label: '模型服务', icon: 'server' },
  { key: 'search', label: '搜索', icon: 'internet' },
  { key: 'usage', label: '统计', icon: 'chart' },
  { key: 'about', label: '关于', icon: 'info-circle' }
]
const activeKey = ref('bots')

const langOptions = [
  { label: '简体中文', value: 'zh-CN' },
  { label: 'English', value: 'en-US' }
]

const defaults = {
  theme: 'light',
  primaryColor: 'green',
  language: 'zh-CN',
  sendKey: 'enter',
  stream: true,
  contextRounds: 10,
  apiBase: 'https://api.example.com/v1',
  apiKey: ''
}

const settings = ref({ ...defaults, ...JSON.parse(localStorage.getItem('bp_system') || '{}') })

function save() {
  localStorage.setItem('bp_system', JSON.stringify(settings.value))
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
