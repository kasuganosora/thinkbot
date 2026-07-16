import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'
import { execSync } from 'node:child_process'

function getGitCommit() {
  try {
    return execSync('git rev-parse --short HEAD').toString().trim()
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
