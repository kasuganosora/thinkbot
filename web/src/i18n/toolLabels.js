/**
 * Tool name → display label map.
 *
 * Design notes:
 *  - Keys are raw LLM tool names (e.g. "read_file", "sandbox_exec").
 *  - Values are display labels shown in UI (Chinese by default).
 *  - This file is i18n-ready: when vue-i18n is introduced, these keys can
 *    become message keys and values move into locale JSON files (zh-CN.json,
 *    en-US.json). The import site only needs to switch from a static import
 *    to `$t('tools.xxx')`.
 *
 * Unknown tool names fall through to the raw English `name` — no crash.
 */

export default {
  // ── 文件操作 ──
  read_file:              '读取文件',
  write_file:             '写入文件',
  list_dir:               '列出目录',
  list_files:             '列出文件',
  edit_file:              '编辑文件',
  replace_in_file:        '替换内容',
  delete_file:            '删除文件',
  move_file:              '移动文件',
  search_content:         '搜索内容',

  // sandbox-prefixed aliases (same tool, different backend path)
  sandbox_read_file:      '读取文件',
  sandbox_write_file:     '写入文件',
  sandbox_list_dir:       '列出目录',
  sandbox_replace_in_file: '替换内容',
  sandbox_delete_file:    '删除文件',
  sandbox_move_file:      '移动文件',
  sandbox_search_content: '搜索内容',
  sandbox_health:         '健康检查',

  // ── 命令执行 ──
  shell:                  '执行命令',
  sandbox_exec:           '容器命令',

  // ── 网络 ──
  web_search:             '网页搜索',
  web_fetch:              '抓取网页',

  // ── 计算 / 数据 ──
  calculate:              '数学计算',
  random:                 '随机数',
  uuid:                   '生成 UUID',
  datetime_calc:          '日期计算',
  now:                    '当前时间',
  current_time:           '获取时间',

  // ── 文本处理 ──
  text_hash:              '文本哈希',
  text_encode:            '文本编码',
  text_diff:              '文本对比',
  text_stats:             '文本统计',

  // ── 技能 ──
  skill_trigger:          '触发技能',

  // ── 平台 / 容器（非 sandbox 命名空间的防御性别名）──
  read:                   '读取文件',
  write:                  '写入文件',
  list:                   '列出目录',
  edit:                   '编辑文件',
  exec:                   '执行命令',
  run_command:            '运行命令',
  bg_exec:                '后台执行',
  bg_status:              '后台任务状态',

  // ── 消息平台 ──
  send:                   '发送消息',
  reply:                  '回复消息',
  react:                  '表情回应',
  get_contacts:           '获取联系人',
  search_memory:          '检索记忆',

  // ── 用户选择 ──
  user_choice:            '向你提问',
  sandbox_user_choice:    '向你提问',

  // ── 浏览器（MCP browser__ 前缀）──
  browser__navigate:      '打开网页',
  browser__click:         '点击元素',
  browser__fill:          '填写表单',
  browser__screenshot:    '截图',
  browser__get_text:      '读取页面文本',
  browser__wait:          '等待元素',
  browser__back:          '后退',
  browser__forward:       '前进',
  browser__cookies_list:  '查看 Cookie',
  browser__close:         '关闭浏览器',
  browser__fetch:         'HTTP 请求',
  browser__snapshot:      '页面快照',
  browser__type:          '输入文字',
  browser__select:        '选择选项',
  browser__hover:         '悬停元素',
  browser__press:         '按键',
  browser__scroll:        '滚动页面',
  browser__tabs:          '标签页',
  browser__evaluate:      '执行脚本',
  browser__network:       '网络请求',
  browser__pdf:           '导出 PDF',

  // ── 任务 / 工作流 ──
  task:                   '提交任务',
  task_detail:            '任务详情',
  task_control:           '任务控制',
  task_status:            '任务状态',

  // ── 记忆 / 定时 / 子代理 ──
  memory:                 '记忆',
  cron:                   '定时任务',
  spawn:                  '子任务',
  use_skill:              '加载技能',
  dreaming:               '整理记忆',
  heartbeat:              '心跳',

  // ── 工具发现 ──
  list_tools:             '列出工具',
  tool_search:            '搜索工具',
  list_all_tools:         '全部工具',
}
