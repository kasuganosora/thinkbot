# Userbot / MTProto Transport — 设计文档（WIP）

> 分支: `feat/userbot-mtproto`（基于 master@818acd6，2026-10-15）
> 预研报告: /data/research/userbot/{01,02}*.md
> 状态: 设计阶段，未引入依赖；M0 登录 demo 等待 api_id/api_hash + 专用小号

## 目标

为 thinkbot 增加 UserTransport（MTProto user 账号），与现有 Bot API Transport 叠加而非替换：

1. 大文件收发（旁路 Bot API 20MB 下载 / 50MB 上传硬上限，MTProto ~2GB）
2. channel/group 潜伏采集（被动 update 消费 + 限速历史回补）
3. 读取入群前历史（bot 做不到）

## 架构

```
agent/bot.Channel (现有接口，不变)
  ├─ channel/misskey/     Bot API webhook
  ├─ channel/telegram/    Bot API long-poll/webhook (BotTransport)
  └─ channel/telegramuser/  ← 新增: MTProto user 账号 (UserTransport)
        ├─ gotd/td 客户端 + session 加密落盘 (/data, 0600)
        ├─ updates 引擎: UpdateNewChannelMessage → ingress.Receive
        ├─ 限速下载器: >20MB 附件走 upload.getFile 分片流
        └─ join/history 工具: ratelimit+floodwait 纪律
```

- 复用 `agent/bot.Channel` + `Sender` 接口，agent 管线零改动
- 依赖候选: github.com/gotd/td (MIT, 活跃)；快速 MVP 可用 celestix/gotgproto（倾向裸 gotd，避免双层封装）
- session 存储加密（age/argon2 或 AES-GCM），密钥不入库

## 纪律（防封号，README "How To Not Get Banned"）

- 专用小号，只读为主，不主动发言/不自动行为
- join 节流：新号养号期 + 每日少量
- getHistory 回补：串行 + 间隔 + 日预算，FLOOD_WAIT 自动退避（contrib/ratelimit + floodwait）

## 里程碑

- [ ] M0: 登录 demo（auth.Flow Constant + 验证码经 TG 转发给 luna）→ session 落盘
- [ ] M1: join 测试频道，拉历史入库
- [ ] M2: 验收 = userbot 收下 85MB APK 并进分析链
