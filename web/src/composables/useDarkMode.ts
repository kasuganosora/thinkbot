import { ref, watch } from 'vue'

const STORAGE_KEY = 'thinkbot-theme'

type Theme = 'light' | 'dark'

const theme = ref<Theme>(
  localStorage.getItem(STORAGE_KEY) as Theme ??
    (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'),
)

watch(theme, (val) => {
  localStorage.setItem(STORAGE_KEY, val)
  document.documentElement.classList.toggle('dark', val === 'dark')
})

// 初始化时应用
document.documentElement.classList.toggle('dark', theme.value === 'dark')

export function useDarkMode() {
  function toggle() {
    theme.value = theme.value === 'dark' ? 'light' : 'dark'
  }

  return { theme, toggle }
}
