import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'
import { execSync } from 'node:child_process'

function getGitCommit() {
  try {
    // stdio: 忽略 stdin/stderr，避免无 git 或仓库异常时把报错刷进构建日志
    const out = execSync('git rev-parse --short HEAD', { stdio: ['ignore', 'pipe', 'ignore'] }).toString().trim()
    return out || 'unknown'
  } catch (_) {
    return 'unknown'
  }
}

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url))
    }
  },
  define: {
    'import.meta.env.APP_COMMIT': JSON.stringify(getGitCommit())
  },
  build: {
    outDir: '../static',
    emptyOutDir: true
  },
  server: {
    host: '0.0.0.0',
    port: 54727,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true
      }
    }
  }
})
