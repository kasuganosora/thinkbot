# cmd — 应用程序入口

thinkbot 的主程序入口（`cmd/main.go`，`package main`），初始化日志、数据库、依赖注入容器，并启动所有模块。

## 启动环境变量

| 变量 | 默认 | 说明 |
|------|------|------|
| `DB_PATH` | `data/thinkbot.db` | SQLite 路径；父目录不存在会自动创建（`data/` 落在 docker 持久化卷内） |
| `LOG_LEVEL` | `info` | 日志级别（不再硬编码 debug） |

两个变量经 `EnvKeyToConfigKey` 映射为配置键 `db.path` / `log.level`。

## 功能

- 初始化 Zap 结构化日志（`log.InitWithConfig`，级别默认 `info`）
  - `stdout` — 全量输出，console 格式（不额外注册 stderr core，
    避免守护进程 `> log 2>&1` 合流后 WARN/ERROR 重复落盘）
  - 文件 — `./logs/thinkbot.log`，JSONL 格式，由 lumberjack 轮转
  - 控制台格式文件 — `./logs/thinkbot.console.log`，可读纯文本、独立轮换，供实时 tail
- 打开 SQLite 数据库（`db.OpenSQLiteWithLogger`，慢查询阈值 200ms，忽略 RecordNotFound）
- 执行 `dao.Migrate(db)` 完成建表迁移
- 通过 `go.uber.org/fx` 组装所有功能模块并 `app.Run()`
  （fx 自身监听 SIGINT/SIGTERM 实现优雅关闭；框架日志用 `fx.NopLogger` 关闭）
- 承载 Swagger 注解：`@title thinkbot API`、`@host localhost:8080`、`@BasePath /`，
  安全定义 `CookieAuth`（cookie 名 `token`）

## 模块装配

```
cmd/main.go
├── fx.Provide  *zap.Logger / *zap.SugaredLogger  — 复用已初始化的全局 logger
├── fx.Provide  *gorm.DB                          — SQLite（默认 data/thinkbot.db）
├── fx.Invoke   dao.Migrate                       — 数据库迁移
├── config.Module     — 配置加载（.env + DB 配置表）
├── singleinst.Module — 单实例版本协商（须在 bot.Module 之前，避免双实例并发消费）
├── auth.Module       — 认证
├── bot.Module        — Bot 管理（agent/bot）
├── identity.Module   — 跨平台身份绑定
├── stats.Module     — LLM 用量统计
└── api.Module       — HTTP API 服务
```

## 启动

```bash
# 直接运行
go run ./cmd

# 编译后运行
go build -o thinkbot ./cmd && ./thinkbot
```

程序不接受命令行参数。配置通过 `.env` 文件或环境变量提供；
`.env` 路径默认为当前目录下 `.env`，可用环境变量 `CONFIG_FILE` 覆盖。
