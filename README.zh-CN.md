[English](README.md) | **简体中文** | [日本語](README.ja.md) | [Русский](README.ru.md)

<p align="center">
  <img src="web/public/icon-app.svg" alt="thinkbot" width="128" height="128" />
</p>

<h1 align="center">thinkbot</h1>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.27+-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go 1.27+" />
  <img src="https://img.shields.io/badge/Docker-compose-2496ED?style=flat-square&logo=docker&logoColor=white" alt="Docker compose" />
  <img src="https://img.shields.io/github/license/kasuganosora/thinkbot?style=flat-square" alt="License" />
  <img src="https://img.shields.io/github/stars/kasuganosora/thinkbot?style=flat-square" alt="Stars" />
  <img src="https://img.shields.io/github/last-commit/kasuganosora/thinkbot?style=flat-square" alt="Last commit" />
</p>

<p align="center">
  <b>服务器出了问题，它第一个找到你——而且你可以直接跟它聊这个问题。</b><br/>
  开源、自托管的长期在线 AI 伙伴：会聊天、会记事、会动手。
</p>

<p align="center">
  <b>简体中文</b> · <a href="README.md">English</a> · <a href="#快速开始">快速开始</a> · <a href="docs/configuration.md">配置</a> · <a href="docs/deploy.md">部署</a>
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

## 它是谁

thinkbot 不只是一个「有问必答」的助手。它有自己的人格（你来写），记得你们聊过的事，会在群里自然地插两句，也会在服务器出事时第一个来找你。它不追求每句话都回，而是想做一个**一直在线、有分寸、靠得住的伙伴**。

## 亮点一览

- **有性格，不是客服腔**：人格写在 SOUL.md 里，说话方式、口癖、兴趣都由你定；聊天、水群、报告告警都用同一种口吻。
- **会水群，也懂得闭嘴**：群里不必每次都 @ 它。它按「聊天节奏」决定要不要接话：有发言倾向、连发防抖和连续发言上限，被 @ 或被回复则一定回应。

- **会主动找你**：服务器告警、部署结果、你让它记着的提醒，它会主动发到你的 Telegram，而不是躺在日志里。
- **记得住，也忘得掉**：分层长期记忆，几周前聊过的事还能接上；你说「忘掉这件事」，它就真的删掉，不会再学回来。
- **真能动手**：每个 bot 有自己的沙箱容器，可以执行命令、读写文件、开浏览器、联网搜索和抓网页，还能加载技能和 MCP 工具。
- **一个部署，多处聊**：Web、Telegram、Misskey、Mastodon 共用同一个 bot，跨平台上下文接得上；一个部署里可以养多个人格不同的 bot。
- **模型自己选**：OpenAI 兼容接口（如 DeepSeek）、OpenAI Responses、Anthropic、Gemini，在网页上点几下就能换。
- **你说了算**：自托管、数据存本地 SQLite；工具权限、用量额度、用户管理都在 Web 管理台里。

## 支持的平台

| 平台 | 状态 | 能做什么 |
|---|---|---|
| Web | ✅ 可用 | 内置聊天界面 + 管理台，可看工具调用过程、沙箱终端与桌面 |
| Telegram | ✅ 可用 | 私聊、群聊（@它或回复它）；发图 / 发文件；群管理工具（置顶、删消息、封禁等，需在工具权限中开启）；**接收服务器告警** |
| Misskey | ✅ 可用 | 回复提及、旁听时间线；发帖、反应、转发、关注、搜索、投票 |
| Mastodon | 🚧 开发中 | 代码已合并，尚未在生产环境测试。回复提及与回复、可选订阅时间线；发帖、收藏、转发等。实例地址和 Access Token 在 Web 管理台「Bot 设置 → 平台」里配置 |

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
- **在群里当个帮手**：被 @ 或被回复时一定回应；平时怎么接话见下面的「在群里水群」。

### 7. 在群里水群

把它拉进 Telegram 群，不用每句都 @ 它：群里的普通消息也会经过它的「聊天节奏」判断，它会挑一部分自然地接上，其余的只听不说（听到的内容照样可以记下来）。

```text
🧑 阿杰：周末有人去漫展吗
🧑 小林：去！就是还没想好 cos 谁
🤖 小林可以考虑去年那部机甲番的女主，道具好做，人还少
🧑 阿杰：哈哈它居然也懂
🧑 小林：@thinkbot 那你说要不要带假发
🤖 带吧，现场一出汗，自己的头发肯定先塌
```

在「Bot 设置 → 平台 → 聊天节奏」里按会话类型（单聊 / 群聊 / 频道）调节：

- **发言倾向**（0.01–1.0，群聊默认 0.4）：对没有 @ 它的消息，按这个概率决定开不开口；1.0 等于每条都回。
- **防抖静默等待**（群聊默认 3 秒）：有人连发几条时当作一段话，不会每条都插嘴。
- **连续发言上限**（群聊默认 3）：连着说了几轮就先停一停，避免霸屏。
- 被 **@ 或被回复** 时不受这些限制，一定回应；单聊默认关闭节奏，即时回复；Web 聊天不受节奏影响。

另外，它自己也可以判断「这条不用回」，只记下不说话。Misskey / Mastodon 的时间线帖子还可以开启「主动参与」（`engagement.*` 配置，内置 observer / lurker / moderator / active 几档预设），按相关度评分和频率限制决定要不要参与，见 [docs/configuration.md](docs/configuration.md)。

### 8. 更多玩法

- **一个部署，多个 bot**：每个 bot 有自己的人格、模型、记忆、沙箱容器和平台账号，互不串台。
- **一个 bot，多个平台**：同一个 bot 同时挂在 Telegram 和 Misskey 上，在一边聊过的事，另一边也接得上（Mastodon 支持开发中：代码已合并，尚未在生产环境测试）。
- **定时与提醒**：可以让它「明早 9 点提醒我」，或用定时任务让它按计划自己干活。

## 记忆是怎么工作的

简单说，像人一样分「短期」和「长期」：

1. **记下来**：聊天中值得记的内容先进入短期记忆（约两周后过期）。
2. **整理**：后台定期把短期记忆提炼成长期记忆（事实、偏好），去重、合并、更新。还可以开启「梦境」：每天夜里给最近的记忆打分，重要的留下，琐碎的随时间淡出，顺便更新它对你的印象。
3. **想起来**：回复前，它会找出和当前话题相关的记忆一起参考。匹配是字面层面的，不是语义检索：不使用 embedding 模型；sqlite-vec 扩展可用时用本地的字符二元组哈希向量做近邻匹配，否则退回关键词匹配。所以几周前提过的爱好，聊天里出现相近的字词时就能被想起来。
4. **忘掉**：让它忘掉某件事，相关记忆会被删除，并留下记号防止日后重新学回来。管理员也能在「Bot 设置 → 记忆」里逐条查看和删除。

长对话可以自动压缩成摘要继续聊；管理员还能用 `/compact` 精简当前会话、`/clear` 从头开始。详见 [docs/memory.md](docs/memory.md)。

## 快速开始

需要：一台装了 Docker（含 `docker compose` 插件，v2.24 或更新）的机器、一个模型 API Key、（可选）一个 Telegram Bot Token（找 [@BotFather](https://t.me/BotFather) 要）。

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
    # 默认端口映射是 "8080:8080"，会对局域网 / 公网开放。只想本机访问就保留下面两行：
    ports: !override
      - "127.0.0.1:8080:8080"
YAML
```

> `!override` 需要 Docker Compose v2.24+。不写它的话，override 里的 `ports` 会和默认的 `8080:8080` **合并**而不是替换，导致端口冲突。部署在服务器上又要远程访问时，删掉 `ports` 这两行，并自行用反向代理 / 防火墙保护。

**3. 启动**

```bash
docker compose up -d --build
```

首次构建需要几分钟（如果提示权限不足，用 `sudo` 或把当前用户加入 `docker` 组）。`docker compose ps` 显示 `healthy` 就好了。

**4. 在网页上完成设置**

打开 `http://localhost:8080`，用上面的账号登录，然后：

1. **系统设置 → 模型服务**：添加服务商，填 API Key，添加模型，并**打开服务商的「启用」开关**（新加的服务商默认是关闭的，不打开的话启动 bot 会报 `LLM "…" not found in config`）；
2. **系统设置 → Bots → 新建 Bot**，选好主模型；
3. **Bot 设置 → 平台**：添加 Telegram，填入 Bot Token 并启用；确认页面顶部 Bot 状态是「运行中」（新建的 bot 默认是停止状态，点一下启用）。第一次启动 bot 时会构建它的沙箱镜像（约 1.4 GB），需要等一两分钟；
4. **用户设置 → 生成授权码**，把 `TB-XXXX-XXXX` 发给你的 Telegram bot，完成账号绑定（接收告警通知需要这一步）。

现在去 Telegram 跟它打个招呼吧 👋 也可以直接在网页里聊。

> 登录后记得在「用户设置 → 安全设置」改密码。管理员已创建后，override 里的两行 `AUTH_BOOTSTRAP_*` 就不再起作用，可以删掉。

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
