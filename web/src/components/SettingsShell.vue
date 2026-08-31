<template>
  <div class="settings-shell">
    <div class="settings-topbar">
      <t-button variant="text" @click="$router.push({ name: 'chat' })">
        <template #icon><t-icon name="chevron-left" /></template>
        返回工作台
      </t-button>
      <span class="settings-title">{{ title }}</span>
    </div>
    <div class="settings-content" :class="{ 'is-wide': wide }">
      <div class="settings-inner" :class="{ 'inner-wide': wide }">
        <slot />
      </div>
    </div>
  </div>
</template>

<script setup>
defineProps({
  title: { type: String, default: '设置' },
  wide: { type: Boolean, default: false }
})
</script>

<style scoped>
.settings-shell {
  width: 100%;
  height: 100%;
  display: flex;
  flex-direction: column;
  background: #f3f3f5;
}
.settings-topbar {
  height: 56px;
  flex-shrink: 0;
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 0 20px;
  background: #fff;
  border-bottom: 1px solid #ececec;
}
.settings-title {
  font-size: 16px;
  font-weight: 600;
}
.settings-content {
  flex: 1;
  overflow-y: auto;
  padding: 28px 20px;
}
.settings-content.is-wide {
  padding: 0;
  overflow: hidden;
}
.settings-inner {
  max-width: 720px;
  margin: 0 auto;
}
.settings-inner.inner-wide {
  max-width: none;
  margin: 0;
  height: 100%;
}
</style>
