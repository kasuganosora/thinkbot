/**
 * spring.js — Apple HIG 风格弹簧动画工具（damping/response 语义）
 *
 * 设计意图（依据 skills/apple-design/SKILL.md §4 Behavior over animation）：
 *  - Apple 用两个设计师友好的参数描述弹簧，而非物理三件套（质量/刚度/阻尼）：
 *      damping（阻尼比）  1.0 = 临界阻尼（无过冲、平滑收敛）
 *                        <1.0 = 过冲并振荡，值越小越弹
 *      response（响应时间）数值上逼近"到达目标"的秒数，越小越跟手；
 *                        注意它不是固定时长——弹簧没有固定时长，收敛时间由参数涌现
 *  - 默认 UI 统一 damping 1.0（优雅、不喧宾夺主）；只有携带动量的手势（甩动/投掷）
 *    才配 <1.0 的过冲。本工具默认导出 Apple 官方值：damping 1.0 / response 0.4。
 *
 * 为什么自己写而不用 CSS transition / @keyframes（skill §3 Interruptibility）：
 *  - CSS 动画无法在半途被抓取并反转，重定向必然产生速度断层（"砖墙"）。
 *  - requestAnimationFrame 弹簧天然"从当前呈现值出发"（presentation value），
 *    重定目标时把当前速度作为初速带入，连续且可无限次中断。
 *  - 每个动画实例只驱动 transform/opacity 等合成器友好属性（skill §11），
 *    且逐帧直接写 style，不触碰 DOM 结构。
 *
 * 可访问性（skill §14 Reduced motion）：
 *  - 尊重 prefers-reduced-motion：reduce 时弹簧退化为短促的透明度/静态过渡，
 *    保留"状态变了"的可读反馈，但不产生位移运动。
 */

/** 是否处于"减弱动态效果"偏好（每次调用实时读取，支持系统设置热切换） */
export function prefersReducedMotion() {
  try {
    return window.matchMedia('(prefers-reduced-motion: reduce)').matches
  } catch {
    // SSR / 无 matchMedia 环境兜底：按无障碍优先，返回 true（不做运动）
    return true
  }
}

/**
 * 把 Apple 的 damping/response 换算成"二阶系统"系数。
 *
 * 推导：临界阻尼弹簧的标准形式 x'' = ω²·(target − x) + 2ζω·x'，
 * 其中 ζ = damping（阻尼比），ω = 2π / response。
 * ω 由 response 决定：response 越小 → ω 越大 → 越快到达目标。
 * 用显式欧拉积分逐帧推进即可，帧步长取 rAF 实际间隔（应对掉帧时保持速度正确）。
 *
 * @param {number} damping  阻尼比，默认 1.0（临界阻尼，无过冲）
 * @param {number} response 响应秒数，默认 0.4（Apple 的 Move/reposition 档位）
 */
function springCoeffs(damping, response) {
  const omega = (2 * Math.PI) / Math.max(response, 0.0001)
  return { omega, zeta: damping }
}

/** 判断弹簧是否已收敛（位移与速度都足够小，可安全停帧） */
function settled(pos, target, vel, epsilon) {
  return Math.abs(target - pos) < epsilon && Math.abs(vel) < epsilon
}

/**
 * animateSpring — 单值弹簧动画。
 *
 * 关键行为（对齐 skill §3「Interruptibility」）：
 *  1. 每个元素持有自己的动画上下文（WeakMap<Element, Ctx>）；
 *  2. 重复调用（重定目标）时**复用**上下文：从当前呈现值与当前速度继续，
 *     不重置、不跳变——这就是"可中断、可反转"的实现基础；
 *  3. 返回 stop() 句柄，组件卸载时必须调用以取消 rAF（防止泄漏与幽灵帧）。
 *
 * @param {{ el: Element, from?: number, to: number, damping?: number, response?: number,
 *           epsilon?: number, onUpdate: (value:number, velocity:number)=>void,
 *           onComplete?: ()=>void, disabled?: boolean }} opts
 *   from      起始值；重定目标时忽略（永远用当前呈现值，skill §3）
 *   disabled  为 true 时不做弹簧（reduced-motion 场景），直接跳终值并回调
 * @returns {() => void} stop
 */
export function animateSpring(opts) {
  const {
    el, to, onUpdate, onComplete,
    damping = 1.0, response = 0.4, epsilon = 0.001,
    from, disabled = false,
  } = opts

  if (typeof onUpdate !== 'function') return () => {}
  if (disabled) {
    // reduced-motion：不做位移运动，直接落终值（等价于"静态过渡"）
    onUpdate(to, 0)
    if (onComplete) onComplete()
    return () => {}
  }

  // 元素级动画上下文：同一元素的连续 animateSpring 调用共享状态，
  // 使新目标从"此刻屏幕上的值 + 此刻速度"出发（速度衔接，无砖墙）。
  const ctxMap = animateSpring._ctx || (animateSpring._ctx = new WeakMap())
  const { omega, zeta } = springCoeffs(damping, response)

  const ctx = ctxMap.get(el)
  const state = ctx || {
    pos: (typeof from === 'number') ? from : to,
    vel: 0,
    raf: 0,
    last: 0,
  }
  // 已有动画在跑：先取消旧 rAF，但保留 pos/vel（这正是中断的本质）
  if (ctx && ctx.raf) cancelAnimationFrame(ctx.raf)
  state.target = to
  state.onUpdate = onUpdate
  state.onComplete = onComplete
  state.coef = { omega, zeta, epsilon }
  ctxMap.set(el, state)

  const step = (now) => {
    // 帧间隔：用真实时钟而非固定 16.7ms，掉帧时速度才不会失真
    const dt = state.last ? Math.min((now - state.last) / 1000, 0.064) : 1 / 60
    state.last = now
    const { omega: w, zeta: z, epsilon: eps } = state.coef
    const diff = state.target - state.pos
    // 半隐式欧拉：先更新速度再更新位置，临界阻尼下数值稳定
    state.vel += (w * w * diff - 2 * z * w * state.vel) * dt
    state.pos += state.vel * dt

    if (settled(state.pos, state.target, state.vel, eps)) {
      state.pos = state.target
      state.vel = 0
      state.raf = 0
      state.last = 0
      state.onUpdate(state.pos, 0)
      const done = state.onComplete
      state.onComplete = null
      if (done) done()
      return
    }
    state.onUpdate(state.pos, state.vel)
    state.raf = requestAnimationFrame(step)
  }

  state.raf = requestAnimationFrame(step)

  // stop 句柄：外部（组件卸载/强制终止）取消 rAF；上下文保留在 WeakMap，
  // 后续若再有新调用仍能从冻结值续跑（可中断性的另一半：随时可停）
  return () => {
    if (state.raf) cancelAnimationFrame(state.raf)
    state.raf = 0
    state.last = 0
  }
}
