<p align="center">
  <img src="web/public/icon-app.svg" alt="thinkbot" width="128" height="128" />
</p>

<h1 align="center">thinkbot</h1>

<p align="center">
  <b>服务器出了问题，它第一个找到你——而且你可以直接跟它聊这个问题。</b><br/>
  开源、自托管的长期在线 AI 伙伴：会聊天、会记事、会动手。
</p>

<p align="center">
  简体中文 · <a href="README.en.md">English</a>
</p>

---

你在 Telegram 里跟它聊天，它记得你说过的话；硬盘、RAID、部署脚本出了状况，它会主动发消息告诉你，你回一句「严重吗？」它就接着聊。它还能在自己的沙箱里跑命令、查网页、改代码，干完把结果告诉你。

thinkbot 适合想要一个**自己掌控的 AI 助手**的人：个人开发者、自己维护服务器的人、想在群里或 Fediverse 上养一个 bot 的人。一条 `docker compose` 就能跑起来，数据都在你自己的机器上。

```text
🤖 [server-1/smartd · INFO] sda、sdb 各发来一条 SMART EmailTest。
   这是 smartd 在测试通知通道，不是故障，两块盘都正常。
🙋 嗯，在测试
🤖 好。sda 和 sdb 都发了一条，说明两块盘的告警通道都是通的——
   以后真有 SMART 问题，会第一时间到你这儿。
```

## 为什么用它

- **会主动找你**：服务器告警、部署结果、你让它记着的提醒，它会主动发到你的 Telegram，而不是躺在日志里。
- **记得住，也忘得掉**：分层长期记忆，几周前聊过的事还能接上；你说「忘掉这件事」，它就真的删掉，不会再学回来。
- **真能动手**：每个 bot 有自己的沙箱容器，可以执行命令、读写文件、开浏览器、联网搜索和抓网页，还能加载技能和 MCP 工具。
- **一个部署，多处聊**：Web、Telegram、Misskey 共用同一个 bot，跨平台上下文接得上；一个部署里可以养多个人格不同的 bot。
- **模型自己选**：OpenAI 兼容接口（如 DeepSeek）、OpenAI Responses、Anthropic、Gemini，在网页上点几下就能换。
- **你说了算**：自托管、数据存本地 SQLite；工具权限、用量额度、用户管理都在 Web 管理台里。

## 支持的平台

| 平台 | 状态 | 能做什么 |
|---|---|---|
| Web | ✅ 可用 | 内置聊天界面 + 管理台，可看工具调用过程、沙箱终端与桌面 |
| Telegram | ✅ 可用 | 私聊、群聊（@它或回复它）；发图 / 发文件；群管理工具（置顶、删消息、封禁等，需在工具权限中开启）；**接收服务器告警** |
| Misskey | ✅ 可用 | 回复提及、旁听时间线；发帖、反应、转发、关注、搜索、投票 |
| Mastodon | 🚧 开发中 | 适配器在 [`feat/mastodon-channel`](https://github.com/kasuganosora/thinkbot/tree/feat/mastodon-channel) 分支，尚未合入 master |

## 用它来做什么

### 1. 服务器出了问题，它第一个找到你

smartd、mdadm、cron、备份或部署脚本调用一下 `thinkbot-notify`，告警就会以 bot 自己的口吻发到**你平时跟它聊天的那个 Telegram 对话**里。告警会记进对话历史，所以你可以直接追问，它知道你在说哪条。

```text
🤖 [server-1/mdadm · INFO] RebuildFinished on /dev/md/md3 (mismatches found: 140672)
   md3 的例行校验跑完了，发现 140672 处不一致；阵列仍是 [UU]，两块盘都在线。
   不紧急，建议抽空看看。
🙋 不用处理
🤖 好的。

        ……第二天……

🤖 [server-1/mdadm · WARN] RebuildStarted on /dev/md/md3
   md3 又开始校验了。不过进度停在 89.6%，速度 0K/sec，看起来像卡住了，
   建议确认一下是不是挂起了。

        ……之后……

🤖 [server-1/mdadm · INFO] RebuildFinished on /dev/md/md3 (mismatches found: 32384)
   这次跑完了，不一致降到 32384，阵列健康。
```

部署脚本同理：最后一步把结果发给 bot，你回「有报错吗？」就能接着问（它依据通知里带的内容回答）。接入步骤见 [配置 › 告警通知快速接入](docs/configuration.md#告警通知notify快速接入)。

> 通知走独立通道：不会被 bot 的「这条不回复」逻辑吞掉；模型出错或超时会退回原文发送；warn / critical 一定附上原始告警文本；同一告警会去重和限流。目前只投递到 Telegram。

### 2. 交给它一件多步骤的活

```text
🙋 在 blog-app 里对本地那个提交跑一下 eslint 和 vitest，没问题就推上去
🤖 缺依赖，先装上了。
   ESLint 报了 2 个错，但基线提交上就有，我单独开了一个提交修掉。
   vitest 有 1 个失败，在基线上同样失败，和这次改动无关。
   第一次 push 失败了，查了下远端配置，修正后已推送。
   小结：改动 8 个文件，ESLint 0 错误，已推送。
```

命令都在这个 bot 自己的沙箱容器里执行，不碰宿主机。

### 3. 帮你查资料，并给出出处

```text
🙋 这次别凭印象，真的去网上查一下 X 今天发布的更新说明
🤖 查到了，以官方发布页为准：…… 主要变化有三点：……
   来源：X 官方发布说明页面（链接）
```

内置 `web_search` / `web_fetch`，搜索后端可在「系统设置 → 搜索」里配置，失败时自动换下一家。

### 4. 用技能做事

```text
🙋 用 use_skill 加载一下代码评审的技能，告诉我它的第一原则是什么
🤖 已加载。它的第一原则是：……
```

技能就是一个带 `SKILL.md` 的目录，放进 `skills/` 或在管理台里添加即可。

### 5. 记得你，也尊重你的「忘掉」

```text
        ……三周前……
🙋 我的咖啡一般是燕麦拿铁，少糖
🙋 最近在业余写一个记账小程序

        ……今天……
🙋 早上好，帮我想想今天先做什么
🤖 早！要不先来杯少糖燕麦拿铁 ☕ 然后把记账小程序上次卡住的导出功能收个尾？

🙋 之前那个周末旅行计划，相关的都忘掉吧
🤖 好，旅行计划相关的记忆已经删掉了，以后不会再提。
```

### 6. 陪你玩

- **有人格地闲聊**：在「Bot 设置 → 人格」写它的 SOUL.md（性格、说话方式），聊动画、聊游戏都按这个人设来。
- **一起跑文字冒险**：它可以抛出选项让你选——Telegram 里是按钮，Misskey 里是原生投票，Web 里是卡片。
- **发图给它看**：主模型支持图片，或配置了视觉模型时，它能看懂你发的截图和梗图。发贴纸时它看到的是贴纸对应的表情，可以顺着接梗。
- **在群里当个帮手**：被 @ 或被回复时回应；开启主动参与后，它会自己判断什么时候插一句话，并有频率限制，不会刷屏。

### 7. 更多玩法

- **一个部署，多个 bot**：每个 bot 有自己的人格、模型、记忆、沙箱容器和平台账号，互不串台。
- **一个 bot，多个平台**：同一个 bot 同时挂在 Telegram 和 Misskey 上，在一边聊过的事，另一边也接得上（Mastodon 即将支持）。
- **定时与提醒**：可以让它「明早 9 点提醒我」，或用定时任务让它按计划自己干活。

## 记忆是怎么工作的

简单说，像人一样分「短期」和「长期」：

1. **记下来**：聊天中值得记的内容先进入短期记忆（约两周后过期）。
2. **整理**：后台定期把短期记忆提炼成长期记忆（事实、偏好），去重、合并、更新。还可以开启「梦境」：每天夜里给最近的记忆打分，重要的留下，琐碎的随时间淡出，顺便更新它对你的印象。
3. **想起来**：回复前，它会找出和当前话题相关的记忆一起参考——按意思相近匹配，所以几周前提过的爱好，聊到相关话题时会自己冒出来。
4. **忘掉**：让它忘掉某件事，相关记忆会被删除，并留下记号防止日后重新学回来。管理员也能在「Bot 设置 → 记忆」里逐条查看和删除。

长对话可以自动压缩成摘要继续聊；管理员还能用 `/compact` 精简当前会话、`/clear` 从头开始。详见 [docs/memory.md](docs/memory.md)。

## 快速开始

需要：一台装了 Docker 的机器、一个模型 API Key、（可选）一个 Telegram Bot Token（找 [@BotFather](https://t.me/BotFather) 要）。

**1. 下载并准备配置**

```bash
git clone https://github.com/kasuganosora/thinkbot.git && cd thinkbot
cp .env.example .env
mkdir -p data/container logs/container
```

`.env` 的默认值就能直接用，不用改。

**2. 设置首位管理员账号**（改成你自己的密码，至少 6 位）

```bash
cat > docker-compose.override.yml <<'YAML'
services:
  thinkbot:
    environment:
      AUTH_BOOTSTRAP_ADMIN: admin
      AUTH_BOOTSTRAP_PASSWORD: change-me-please
YAML
```

**3. 启动**

```bash
docker compose up -d --build
```

首次构建需要几分钟。`docker compose ps` 显示 `healthy` 就好了。

**4. 在网页上完成设置**

打开 `http://localhost:8080`，用上面的账号登录，然后：

1. **系统设置 → 模型服务**：添加服务商，填 API Key，添加模型；
2. **系统设置 → Bots → 新建 Bot**，选好主模型；
3. **Bot 设置 → 平台**：添加 Telegram，填入 Bot Token 并启用；确认页面顶部 Bot 状态是「运行中」（不是的话点一下启用）；
4. **用户设置 → 生成授权码**，把 `TB-XXXX-XXXX` 发给你的 Telegram bot，完成账号绑定（接收告警通知需要这一步）。

现在去 Telegram 跟它打个招呼吧 👋 也可以直接在网页里聊。

> 登录后记得在「用户设置 → 安全设置」改密码。管理员已创建后，override 里的两行就不再起作用，可以删掉。

## 下一步

| 想了解 | 看这里 |
|---|---|
| 所有配置项、首位管理员、工具权限、告警通知接入 | [docs/configuration.md](docs/configuration.md) |
| 部署细节、更新、备份、回滚、迁移 | [docs/deploy.md](docs/deploy.md) |
| 记忆系统 | [docs/memory.md](docs/memory.md) |
| 外部通知接口（notify）完整规范 | [docs/notify.md](docs/notify.md) |
| 功能详解、架构、项目结构、技术栈 | [docs/architecture.md](docs/architecture.md) |
| 裸机运行、前端开发、Swagger、测试 | [docs/development.md](docs/development.md) |

## License

MIT
