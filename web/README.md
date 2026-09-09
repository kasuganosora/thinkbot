# web/ — 前端源码

thinkbot 的 Web 前端，承担两块职责：

- **聊天界面**：多 Bot 会话、工具调用展示、Markdown 渲染、内置终端；
- **管理台**：个人/系统设置、Bot 配置，以及仅管理员可见的后台（用户、技能、配置、统计、系统监控）。

## 技术栈

依赖与版本以 `package.json` 为准：

| 分类 | 依赖 |
| --- | --- |
| 框架 | vue ^3.4.21、vue-router ^4.3.0（hash 模式）、pinia ^2.1.7 |
| UI 组件库 | tdesign-vue-next ^1.9.4、tdesign-icons-vue-next ^0.2.6 |
| 终端 | @xterm/xterm ^5.5.0、@xterm/addon-fit ^0.10.0 |
| Markdown | marked ^18.0.5 + dompurify ^3.4.11（渲染 + 净化） |
| 构建 | vite ^5.2.8、@vitejs/plugin-vue ^5.0.4 |

## 目录结构

```
web/
├── index.html              # SPA 入口
├── vite.config.js          # Vite 配置（输出目录、dev 端口与代理）
├── package.json
├── package-lock.json
├── .gitignore              # node_modules/、dist/、*.local
└── src/
    ├── main.js             # 应用入口
    ├── App.vue
    ├── layouts/            # MainLayout：登录后的整体框架
    ├── router/             # 路由表 + 登录守卫与 admin 权限校验
    ├── stores/             # Pinia：user（登录态/角色）、bot（会话与消息）
    ├── api/                # http.js（请求封装）、services.js（接口入口）、mockDb.js（mock 数据）
    ├── views/              # 页面级视图（见下）
    ├── components/         # 通用组件 + bot/（Bot 配置分页，16 个）+ common/（XtermConsole）
    ├── i18n/               # toolLabels：工具名 → 显示文案映射
    ├── styles/             # tokens.css（设计变量）、global.css
    └── utils/              # markdown、format、brand（主题色）、spring、userPreferences
```

## 主要视图（src/views/）

| 视图 | 路由 | 说明 |
| --- | --- | --- |
| Chat.vue | `/chat`、`/chat/bot/:botId/:sessionId?` | 聊天主界面 |
| Login.vue | `/login` | 登录页（公开，已登录自动跳转聊天） |
| UserSettings.vue | `/settings/user` | 个人设置 |
| SystemSettings.vue | `/settings/system` | 系统设置 |
| BotSettings.vue | `/settings/bot/:id?` | Bot 创建与配置（基本信息、平台、MCP、记忆、定时任务、心跳、做梦、工具权限等分页，见 `src/components/bot/`） |
| admin/UsersView.vue | `/admin/users` | 用户管理（仅管理员） |
| admin/SkillsView.vue | `/admin/skills` | 技能管理（仅管理员） |
| admin/ConfigView.vue | `/admin/config` | 系统配置（仅管理员） |
| admin/StatsView.vue | `/admin/stats` | 用量统计（仅管理员） |
| admin/SystemMonitorView.vue | `/admin/system` | 系统监控（仅管理员） |

路由守卫（`src/router/index.js`）：未登录一律重定向到 `/login`；`meta.admin` 页面会先向服务端确认角色（HttpOnly cookie），非管理员挡回聊天页。

## 开发与构建

npm scripts 以 `package.json` 为准：

```bash
cd web
npm install     # 安装依赖
npm run dev     # 本地开发，监听 0.0.0.0:54727，/api 代理到 http://localhost:8080
npm run build   # 生产构建
npm run preview # 预览构建产物
```

开发要点（来自 `vite.config.js`）：

- 路径别名 `@` → `./src`；
- 构建时注入 `import.meta.env.APP_COMMIT`（git short commit，取不到则为 `unknown`）；
- 后端接口未就绪时，API 层走 mock（见 `src/api/http.js` 的 `USE_MOCK` 与 `src/api/mockDb.js`；当前 `USE_MOCK` 为 `false`，走真实后端）。

## 构建产物与 static/ 的关系

`vite.config.js` 中 `build.outDir: '../static'` 且 `emptyOutDir: true`：

- `npm run build` 的产物（入口 `index.html` + 带内容哈希的 `assets/`）直接输出到**仓库根目录的 `static/`**，每次构建会先清空该目录；
- 运行期由 Go 后端从 `static/` 提供 SPA 服务，详见 [`static/README.md`](../static/README.md)；
- `static/` 为生成目录，请勿手工编辑，改动请在 `web/` 完成后重新构建。
