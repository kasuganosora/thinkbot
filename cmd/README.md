# cmd — 应用程序入口

thinkbot 的主程序入口，初始化日志、数据库、依赖注入容器，并启动所有模块。

## 功能

- 初始化 Zap 结构化日志（带 lumberjack 日志轮转）
- 初始化 SQLite 数据库连接
- 通过 `go.uber.org/fx` 组装所有功能模块
- 信号监听（SIGINT/SIGTERM）实现优雅关闭

## 模块依赖图

```
cmd/main.go
├── config.Module   — 配置加载
├── db              — 数据库初始化
├── auth.Module     — 认证（引导管理员）
├── dao             — 数据访问层
├── channel         — 渠道适配器
├── bot.Module      — Bot 管理
├── sandbox.Module  — 沙箱工作空间
├── api.Module      — HTTP API 服务
├── stats.Module    — 统计记录
└── workflow.Module — 工作流引擎
```

## 启动

```bash
# 直接运行
go run ./cmd

# 编译后运行
go build -o thinkbot ./cmd && ./thinkbot
```

配置通过 `.env` 文件或环境变量提供。
