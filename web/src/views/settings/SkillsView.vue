<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { Search, Wrench } from 'lucide-vue-next'
import { skillsApi } from '@/api/client'
import { useToast, TButton, TBadge, TSpinner, TEmpty, TPageHeader, TCard } from '@/components/ui'
import type { SkillInfo } from '@/types/api'

const toast = useToast()
const skills = ref<SkillInfo[]>([])
const loading = ref(true)
const search = ref('')

const filtered = () =>
  skills.value.filter((s) => s.name.toLowerCase().includes(search.value.toLowerCase()))

async function load() {
  loading.value = true
  try {
    const result = await skillsApi.list()
    skills.value = result.skills || []
  } finally {
    loading.value = false
  }
}

async function toggle(name: string, enabled: boolean) {
  try {
    if (enabled) await skillsApi.disable(name)
    else await skillsApi.enable(name)
    await load()
  } catch (e) {
    toast.error(e instanceof Error ? e.message : '操作失败')
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <TPageHeader title="技能管理" subtitle="查看和启停机器人技能">
      <template #actions>
        <div class="search-wrap">
          <Search :size="14" class="search-icon" />
          <input v-model="search" class="search-box" placeholder="搜索…" />
        </div>
      </template>
    </TPageHeader>

    <div v-if="loading" class="loading-state"><TSpinner size="lg" /></div>

    <TEmpty v-else-if="filtered().length === 0" text="暂无技能" />

    <div v-else class="page-content">
      <div class="skill-grid">
        <TCard v-for="skill in filtered()" :key="skill.name" padding="default">
          <div class="skill-top">
            <div class="skill-title-row">
              <Wrench :size="14" class="skill-icon" />
              <h3 class="skill-name">{{ skill.name }}</h3>
            </div>
            <TBadge :variant="skill.enabled ? 'success' : 'secondary'">
              {{ skill.enabled ? '启用' : '禁用' }}
            </TBadge>
          </div>
          <p class="skill-desc">{{ skill.description || '无描述' }}</p>
          <div class="skill-bottom">
            <span v-if="skill.category" class="skill-cat">{{ skill.category }}</span>
            <TButton variant="ghost" size="sm" @click="toggle(skill.name, skill.enabled)">
              {{ skill.enabled ? '禁用' : '启用' }}
            </TButton>
          </div>
        </TCard>
      </div>
    </div>
  </div>
</template>

<style scoped>
@import '@/assets/page-common.css';

.skill-grid {
  display: grid;
  gap: 0.75rem;
  grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
}

.skill-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 0.5rem;
}
.skill-title-row { display: flex; align-items: center; gap: 0.375rem; }
.skill-icon { color: var(--muted-foreground); }
.skill-name {
  font-size: 0.875rem;
  font-weight: 600;
  font-family: var(--font-mono);
}
.skill-desc {
  font-size: 0.8125rem;
  color: var(--muted-foreground);
  line-height: 1.5;
  margin-bottom: 0.75rem;
}
.skill-bottom {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.skill-cat {
  font-size: 0.6875rem;
  color: var(--muted-foreground);
}
</style>
