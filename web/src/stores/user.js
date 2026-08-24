import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { authApi } from '@/api/services'

// 登录资料缓存键。
//
// ⚠️ 这份缓存只是「上次登录的展示信息」（昵称/头像等），用于首屏免闪烁渲染，
// **不是鉴权凭据**：真正的凭据是服务端下发的 HttpOnly cookie。任何人都能在
// 控制台把 bp_user 里的 role 改成 admin，所以 role 必须经服务端确认后才可用于
// 权限判定（见 ensureProfile / isAdmin）。
const CACHE_KEY = 'bp_user'

/** 读取本地缓存的登录资料（session 优先：未勾选「记住我」时只存本次会话） */
function readCache() {
  for (const store of [sessionStorage, localStorage]) {
    try {
      const raw = store.getItem(CACHE_KEY)
      if (raw) return JSON.parse(raw)
    } catch {
      // 缓存损坏（手工改坏 / 存储被禁用）→ 视为未登录，交给服务端 401 兜底
    }
  }
  return null
}

function writeCache(profile, remember) {
  const raw = JSON.stringify(profile)
  try {
    if (remember) {
      localStorage.setItem(CACHE_KEY, raw)
      sessionStorage.removeItem(CACHE_KEY)
    } else {
      sessionStorage.setItem(CACHE_KEY, raw)
      localStorage.removeItem(CACHE_KEY)
    }
  } catch {
    // 隐私模式等存储不可用：内存态仍可用，仅失去刷新后免闪烁的能力
  }
}

function clearCache() {
  try { localStorage.removeItem(CACHE_KEY) } catch { /* ignore */ }
  try { sessionStorage.removeItem(CACHE_KEY) } catch { /* ignore */ }
}

export const useUserStore = defineStore('user', () => {
  const user = ref(readCache())
  // role 是否已由服务端确认。仅当为 true 时 role 才可参与权限判定。
  const roleVerified = ref(false)
  // 记住我：决定资料缓存写 localStorage（跨会话）还是 sessionStorage（关页即失效）
  const remember = ref(!!localStorage.getItem(CACHE_KEY))

  const isLoggedIn = computed(() => !!user.value)
  // 管理员判定：必须是服务端确认过的 role，缓存里的 role 一律不作数
  const isAdmin = computed(() => roleVerified.value && user.value?.role === 'admin')

  function login(profile, opts = {}) {
    remember.value = opts.remember !== false
    user.value = profile
    // 登录响应由服务端直接下发，其中的 role 是可信的
    roleVerified.value = true
    writeCache(profile, remember.value)
  }

  function logout() {
    user.value = null
    roleVerified.value = false
    _mePromise = null
    clearCache()
  }

  function updateProfile(patch) {
    user.value = { ...user.value, ...patch }
    writeCache(user.value, remember.value)
  }

  // 进行中的 me() 请求，避免路由守卫与多个面板并发时重复请求
  let _mePromise = null

  /**
   * 向服务端确认当前登录态与角色（基于 cookie），并用权威值覆盖本地缓存。
   * 权限相关的判断（admin 面板挂载、管理路由准入）都应先 await 这个方法。
   * @param {boolean} [force] 强制重新拉取
   * @returns {Promise<object>} 服务端确认后的资料
   */
  async function ensureProfile(force = false) {
    if (roleVerified.value && !force) return user.value
    if (!_mePromise) {
      _mePromise = authApi.me()
        .then((profile) => {
          if (!profile) throw new Error('未登录')
          // 服务端字段为权威值；本地额外字段（昵称等展示项）保留
          user.value = {
            ...(user.value || {}),
            ...profile,
            nickname: profile.displayName || profile.username || user.value?.nickname,
          }
          roleVerified.value = true
          writeCache(user.value, remember.value)
          return user.value
        })
        .finally(() => { _mePromise = null })
    }
    return _mePromise
  }

  return {
    user, isLoggedIn, isAdmin, roleVerified, remember,
    login, logout, updateProfile, ensureProfile,
  }
})
